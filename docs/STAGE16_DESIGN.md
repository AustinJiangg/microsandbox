# Stage 16 design — auth: `X-API-Key` → team, team-scoped resources, a data-plane access token

> Status: **proposed.** This is the first production-fidelity stage after the Stage 8–15
> decomposition (`docs/E2B_ALIGNMENT_ROADMAP.md` §"Later — production fidelity"). It adds the
> one thing every prior stage deliberately skipped because we were "one repo, one machine":
> **identity**. Read `docs/ARCHITECTURE.md` (the layers) and `docs/STAGE14_DESIGN.md` (the
> store + catalog seams this builds on) first.

## 1. Goal

Today **anyone who can reach the api owns every sandbox.** The mux in
`services/cmd/api/main.go` is bare — `POST /sandboxes`, `GET /sandboxes`,
`DELETE /sandboxes/{id}`, the template routes — none of them check who is calling. The
metadata store has no notion of an owner, so `GET /sandboxes` returns *everyone's*
sandboxes, and `DELETE /sandboxes/{id}` will tear down *anyone's* VM. The data plane is
similar: `client-proxy` routes a request to a sandbox purely by the `<port>-<id>` host —
the only thing protecting a sandbox's `envd` is that its id is hard to guess.

Stage 16 gives the system **identity**, mirroring E2B's model:

1. **Lifecycle plane** — the api authenticates every request with an `X-API-Key` header
   that resolves to a **team**; resources (sandboxes, template builds) **belong to a team**;
   `list` / `delete` / build-status are **scoped to the caller's team**. A missing or
   unknown key is `401`; another team's sandbox is `404` (not "forbidden" — we don't even
   admit it exists).
2. **Data plane** — each sandbox gets a **per-sandbox access token** minted at create time.
   `client-proxy` requires it (an `X-Access-Token` header) before routing to the in-VM
   **control** services (`envd` :49983, `code-interpreter` :49999). User-exposed ports
   (e.g. a web server on :8000 reached via `get_host(8000)`) stay **public**, exactly as
   E2B's exposed-port URLs are public.

This is still a **learning implementation, not security-audited** (§9). The point is to
reproduce E2B's *auth seams* — a team-scoped control plane and a token-gated data plane —
not to be a hardened gateway.

## 2. What E2B actually does (the shape we mirror)

Verified against the E2B model (`e2b-dev/infra`):

- The **api** authenticates with an **`X-API-Key`** header. A key maps to a **team**
  (organisation). Keys live in **Postgres** (Supabase), stored **hashed**, not in plaintext.
- Every resource — sandboxes, templates, builds — carries a **`team_id`**. The api filters
  reads and authorises writes by the team the key resolved to. You cannot see or touch
  another team's resources.
- The **data plane** (`envd`) is reachable through `client-proxy` by the sandbox hostname;
  access to it is gated by a **per-sandbox secret** (E2B signs/authenticates envd access),
  while **exposed user ports are public URLs** (that *is* the feature).

We reproduce all three with single-machine-appropriate implementations behind the seams
Stage 14 already built (the `store.Store` interface, the `catalog.Catalog` interface).

## 3. Current state (what has no identity today)

| Layer | File | Today |
|---|---|---|
| api routes | `services/cmd/api/main.go:86-95` | bare mux, no auth wrapper |
| create | `services/cmd/api/handlers.go:25` | no owner recorded |
| list | `services/cmd/api/handlers.go:99` | returns **all** rows |
| delete | `services/cmd/api/handlers.go:76` | deletes **any** id |
| store schema | `services/pkg/store/store.go:61` | `sandboxes`/`builds`, **no `team_id`**, no teams/keys tables |
| catalog | `services/pkg/catalog/catalog.go:23` | `Set(id, node)` / `Get(id) → node` — **no token** |
| client-proxy | `services/cmd/client-proxy/proxy.go:39` | routes by host, **no token check** |
| SDK | `src/microsandbox/client.py` | sends **no credentials** at all |

## 4. Lifecycle-plane design (teams, keys, scoping)

### 4.1 Store: two new tables + a `team_id` column + auth queries

`pkg/store` gains a `teams` table and an `api_keys` table, and a `team_id` column on the
existing `sandboxes` and `builds` tables. Keys are stored **hashed** (`sha256` hex) — the
store never sees a plaintext key.

```sql
CREATE TABLE IF NOT EXISTS teams (
    id   TEXT PRIMARY KEY,
    name TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS api_keys (
    key_hash   TEXT PRIMARY KEY,         -- sha256(key) hex; never the plaintext
    team_id    TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
-- and, on the existing tables:
--   sandboxes.team_id TEXT NOT NULL DEFAULT 'default'
--   builds.team_id    TEXT NOT NULL DEFAULT 'default'
```

**Migration wrinkle (honest).** The store's schema is applied with `CREATE TABLE IF NOT
EXISTS` — a "poor man's migration" that the store comments already flag as un-versioned.
That is fine for the two *new* tables, but it will **not** add `team_id` to a `sandboxes`
table that already exists (the conftest reuses a Postgres already listening on :5432, so a
prior session's DB has the old schema). So each backend runs an **idempotent `ALTER`**:

- Postgres: `ALTER TABLE sandboxes ADD COLUMN IF NOT EXISTS team_id TEXT NOT NULL DEFAULT 'default'`.
- SQLite (`modernc`, no `ADD COLUMN IF NOT EXISTS`): check `PRAGMA table_info(sandboxes)`
  and `ALTER TABLE … ADD COLUMN …` only when the column is absent.

The `DEFAULT 'default'` backfills any pre-existing rows into the default team, so an
upgraded DB stays consistent. This is the cheapest honest migration; a real system would
version its migrations.

The `Store` interface (today 7 methods, `services/pkg/store/store.go:44`) becomes
team-aware:

```go
type Store interface {
    // sandboxes — now team-scoped
    InsertSandbox(id, template, teamID string) error
    SandboxTeam(id string) (teamID string, ok bool, err error) // ownership lookup (for delete)
    DeleteSandbox(id string) error                             // unchanged; called after the ownership check
    ListSandboxes(teamID string) ([]Sandbox, error)
    // builds — now team-scoped
    InsertBuild(buildID, name, teamID string) error
    BuildTeam(buildID string) (teamID string, ok bool, err error)
    UpdateBuild(buildID, state, detail string) error
    ListBuilds(teamID string) ([]Build, error)
    // auth
    ResolveAPIKey(keyHash string) (teamID string, ok bool, err error)
    EnsureTeam(id, name string) error    // idempotent (INSERT … ON CONFLICT/OR IGNORE)
    InsertAPIKey(keyHash, teamID string) error // idempotent
    Close() error
}
```

`Sandbox`/`Build` structs gain a `TeamID` field (kept out of the api's JSON, which is
unchanged). Both impls (`sqlite.go`, `postgres.go`) get the same statements, differing only
in the three dialect points the package already documents (`?` vs `$N`, single-writer cap,
`created_at` scan). The Go store unit tests (`store_test.go`) run hermetically on SQLite;
the Postgres variant keeps self-skipping without `MSB_TEST_PG_DSN`.

### 4.2 Api: an auth middleware + team-scoped handlers + a seeded dev key

A small wrapper authenticates every route except `/health`:

```go
func (a *api) withAuth(next http.HandlerFunc) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        key := r.Header.Get("X-API-Key")
        if key == "" { writeJSON(w, 401, {"error": "missing X-API-Key"}); return }
        team, ok, err := a.store.ResolveAPIKey(hashKey(key)) // sha256 hex
        if err != nil { writeJSON(w, 500, …); return }       // store/DB down ≠ bad key
        if !ok { writeJSON(w, 401, {"error": "invalid API key"}); return }
        next(w, r.WithContext(withTeam(r.Context(), team)))
    }
}
```

Handlers read the team from the context and thread it through the store:

- `handleCreate` → `InsertSandbox(id, template, team)`; the team also rides into the
  data-plane token step (§5).
- `handleList` → `ListSandboxes(team)` — only the caller's sandboxes.
- `handleDestroy` → `SandboxTeam(id)` first: unknown id **or** a different team → `404`
  (we don't leak existence). Only then the existing teardown (orchestrator `Delete` →
  catalog `Delete` → store `DeleteSandbox`). **Ordering matters**: we must not kill another
  team's VM, so the ownership check precedes the gRPC `Delete`.
- `handleTemplateCreate` → `InsertBuild(id, name, team)`; `handleTemplateList` →
  `ListBuilds(team)`; `handleTemplateBuildStatus` → `BuildTeam(id)` check → `404` if not the
  team's.

**Seeding.** On startup the api ensures a default team and a default dev key so local use
works out of the box:

- `--seed-team` (default `default`), `--seed-api-key` (default `msb_dev_key`). The api calls
  `EnsureTeam` + `InsertAPIKey(hashKey(seedKey), seedTeam)` (both idempotent). Passing
  `--seed-api-key ""` disables seeding (a deployment that provisions keys out-of-band).

This is the E2B shape (keys in Postgres, resolved per request) at learning scale.

## 5. Data-plane design (the per-sandbox access token)

### 5.1 Mint at create, carry in the catalog

The token is **routing-adjacent ephemeral state**, so it lives where the route lives — the
**catalog (Redis)**, next to the node — not in Postgres. The `catalog.Catalog` interface
(`Set(id, node)` / `Get(id) → node`) becomes value-carrying:

```go
type Route struct {
    Node  string // orchestrator data-proxy addr (unchanged meaning)
    Token string // per-sandbox access token; "" only for legacy/never-set
}
type Catalog interface {
    Set(id string, r Route) error
    Get(id string) (r Route, ok bool, err error)
    Delete(id string) error
}
```

- `Redis` stores the `Route` as a small JSON blob at `sandbox:<id>` (one key, as today).
- `InMemory` (the unit-test double) stores `map[string]Route`.

On create, the api mints a token (`crypto/rand`, `sbx_` + 32 hex), writes
`catalog.Set(id, Route{Node: a.nodeAddr, Token: token})`, and returns it to the SDK:

```json
{ "id": "sb_…", "data_url": "http://…", "token": "sbx_…" }
```

The Redis write is still load-bearing exactly as today (a failure rolls the VM back).

### 5.2 client-proxy enforces it — for control ports only

`handleData` (`proxy.go:39`) gains a token check **scoped to the in-VM control ports**:

```go
port, id, ok := parseHostRoute(r.Host)        // unchanged
route, found, err := s.catalog.Get(id)         // now returns Route{Node, Token}
…                                              // 502 on err, 404 on !found (unchanged)
if isControlPort(port) {                        // 49983 (envd) | 49999 (code-interpreter)
    if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Access-Token")),
                                   []byte(route.Token)) != 1 {
        writeJSON(w, 401, {"error": "invalid or missing access token"})
        return
    }
}
// user ports (anything else) are public: no token — that's the exposure feature
… route to route.Node (unchanged) …
```

**Why control-ports-only.** `tests/test_ports.py` reaches a user server on :8000 through
`client-proxy` with *only* a Host header and no token — because an exposed port is a public
URL (E2B's model). Gating only :49983/:49999 keeps that property and that test green, while
the SDK's own control traffic (run_code / files / commands) is authenticated. The control
ports are the constants in `pkg/fc` (`EnvdTCPPort`, `CodeInterpreterTCPPort`), mirrored in
the SDK (`client.py`).

### 5.3 SDK threads the token

`Sandbox` stores `self._token` from the create response and sends it as `X-Access-Token` on
every data call — `_envd` (files/commands, port 49983) and `_stream` (run_code, port
49999). `connect.py`'s `unary` / `server_stream` already accept a `headers` dict, so this is
a header merge, no wire change. `get_host()` and direct user-port access are untouched
(public).

## 6. SDK auth surface

```python
Sandbox(api_key=None, …)          # arg → MICROSANDBOX_API_KEY env → None
build_template(…, api_key=None)   # same resolution
```

- The SDK sends `X-API-Key` on **lifecycle** calls (`_control_plane`) and the template
  build calls (`_api_request`). No key set → no header → the api answers `401`, surfaced as
  a clear `RuntimeError`.
- **No silent default in the SDK** (faithful to E2B, which requires `E2B_API_KEY`). For
  local ergonomics the *api* seeds the well-known dev key `msb_dev_key`; the conftest and
  the `dev-up.sh` smoke set `MICROSANDBOX_API_KEY=msb_dev_key` so existing usage Just Works.

## 7. Tests

**Go units (KVM-free, hermetic):**
- `store_test.go` — teams/keys round-trip, `ResolveAPIKey` hit/miss, team-scoped
  insert/list/delete isolation, the idempotent seed. (SQLite hermetic; PG variant self-skips.)
- `catalog_test.go` / `redis_test.go` — `Route{Node,Token}` round-trips.
- `clientproxy_test.go` — control port requires a matching token (401 on missing/wrong),
  user port routes with no token.

**Python e2e (the behavioral oracle, on real VMs):** a new `tests/test_auth.py`:
- create with no key → `401`; with a wrong key → `401`; with the dev key → works.
- **team isolation**: provision a second key/team (via the api's seed flags or a tiny store
  insert helper in the fixture); team A creates a sandbox, team B's `list` doesn't see it,
  team B's `delete` returns `404`, A's VM stays alive.
- **data plane**: a sandbox whose `X-Access-Token` is dropped/garbled gets `401` from
  client-proxy on `run_code`; the correct token works (already exercised by every other
  e2e test, which now flows the token).

The conftest sets `MICROSANDBOX_API_KEY=msb_dev_key` for the session so the whole existing
suite (37 cases) stays green unchanged; `test_auth.py` overrides/omits it per case.

## 8. Sub-steps (each independently verifiable, tests green at every step)

Ordered so the suite is green after every commit. The design doc lands first, matching the
project's rhythm (e.g. Stage 15's `docs: add Stage 15 design` preceded `15a`).

- **doc** — `docs: add Stage 16 design` (this file). *(this commit)*
- **16a — lifecycle auth + team scoping.** Store tables/columns/queries + migration; the api
  auth middleware, seeding, and team-scoped handlers; the SDK sends `X-API-Key`; conftest
  sets the dev key. Green: lifecycle authenticated + team-scoped, data plane unchanged.
  `test_auth.py` lifecycle cases + the store units.
  → `feat: authenticate the api with X-API-Key and scope resources to teams`
- **16b — data-plane token.** `catalog.Route{Node,Token}`; the api mints + returns the token;
  client-proxy validates it for the control ports; the SDK sends `X-Access-Token`. Green: data
  plane gated, user ports still public. `test_auth.py` data-plane case + catalog/client-proxy units.
  → `feat: gate the data plane with a per-sandbox access token`
- **16c — docs.** Finalize this doc's status, update `CLAUDE.md` (architecture paragraph +
  Done list + the smoke command needs the key), `E2B_ALIGNMENT_ROADMAP.md` (auth done),
  `ARCHITECTURE.md`.
  → `docs: mark Stage 16 (auth: X-API-Key→team + data-plane token) done`

## 9. Honest assessment & deferred gaps

- **What is real:** a team-scoped control plane (keys hashed in Postgres, resolved per
  request, resources owned and isolated) and a token-gated data plane — E2B's two auth seams,
  at learning scale. The store/catalog interfaces absorbed it without a redesign (the Stage-14
  seams paying off again).
- **Deliberately deferred (faithful to E2B but out of scope here):**
  - **No key management API** — keys are seeded by flag, not minted/rotated/revoked over an
    endpoint. E2B has team/key admin; we don't.
  - **No transport security** — everything is plaintext HTTP on loopback (as every prior
    stage). An `X-API-Key` over plaintext is only "auth" in the learning sense; a real
    deployment needs TLS. The token uses a constant-time compare, but the channel is not
    confidential.
  - **No rate limiting / quotas / per-team resource caps** — E2B enforces team limits; we
    don't.
  - **Data-plane token is bearer-only** — minted at create, valid for the sandbox's life, no
    expiry/refresh/signing (E2B signs envd access). Good enough to gate the control ports;
    not a JWT.
- **Safety rule, reinforced:** this remains **not security-audited** and **never safe to
  expose to untrusted input**. Stage 12 made the sandbox inbound-reachable; Stage 16 adds a
  lock on the *control* plane, but the implementation is for learning, not protection. The
  docs must keep saying so.
