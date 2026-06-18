# Stage 9 design: `client-proxy` + sandbox `catalog`; sink the data plane off `api`

> Status: **agreed direction.** Second stage of the E2B-alignment refactor — read
> `docs/E2B_ALIGNMENT_ROADMAP.md` (the map) and `docs/STAGE8_DESIGN.md` (the seam Stage 8
> stood up) first. Stage 9 introduces E2B's edge data proxy (`client-proxy`) and a sandbox
> routing `catalog`, then removes the temporary data passthrough Stage 8 left in `api`, so
> `api` becomes lifecycle-only. The wire protocol, the in-VM daemon (`daemon/`), and the
> `orchestrator` stay byte-stable; only *where the SDK sends the data path* moves. Three
> sub-steps (9a → 9b → 9c).

## 1. Goal & non-goals

**Goal.** Deliver the next two E2B components and retire Stage 8's scaffolding:

- **`client-proxy`** (edge data proxy) — reads `X-Sandbox-Id`, looks up the sandbox's node
  in the catalog, and reverse-proxies the data path to that node's orchestrator data proxy
  (`:5007`) → vsock → `envd`. This is the role `api`'s temporary passthrough plays today.
- **`pkg/catalog`** (in-memory, E2B-shaped `sandbox → node` routing table) — written when a
  sandbox is created, read on every data request to route it.
- **`api` becomes lifecycle-only.** Its temporary `/sandboxes/{id}/{rest...}` reverse proxy
  (Stage 8, `handlers.go:handleProxy`) is deleted; the data plane never touches `api` again.
  The SDK sends lifecycle to `api` and the data path to `client-proxy`.

This mirrors E2B's split: `api` does auth/placement/persistence and **writes the catalog**;
`client-proxy` is a stateless edge that **reads the catalog** and routes data into the right
sandbox on the right node. We do it over the existing vsock transport (D1) and header-mode
routing (E2B's documented fallback for hosts without `<port>-<id>` DNS).

**Non-goals** (kept out to bound the diff; each is a later stage):

- **Don't touch the `orchestrator`.** Its data proxy (`cmd/orchestrator/dataproxy.go`) has
  routed by the `X-Sandbox-Id` header since Stage 8b precisely so that Stage 9 could slot
  `client-proxy` in front of it. `client-proxy` forwards the same header through unchanged.
- **Don't touch `daemon/` (envd), `protocol.py`, or the data-plane bytes.** `/execute`'s
  SSE, `/files/*`, `/commands` and their JSON shapes are unchanged. The SDK's *user surface*
  (`run_code` / `files` / `commands`) is unchanged; only the transport addressing of its
  data calls (the URL + one added header) moves — the same discipline as every prior stage.
- **No `TemplateService`** (Stage 10): template `name → artifact` resolution stays as today.
- **No real `<port>-<sandboxID>` hostnames / TAP networking** (Stage 12): routing is by the
  `X-Sandbox-Id` header over vsock, not by per-sandbox DNS. **No auth**; **no multi-node
  placement** (the catalog's `node` value is the single orchestrator's data-proxy address).

## 2. Target architecture (Stage 9 end state)

```
   lifecycle  (POST / DELETE / GET /sandboxes)
 SDK ──────────────────────────────────────────►  api  ──gRPC──►  orchestrator   (UNCHANGED)
  │  base_url = :8080                               │ Create/Delete/List      │ gRPC SandboxService :9090
  │                                                 │ store (SQLite)          │ data proxy :5007
  │                       register / deregister      │                        │  X-Sandbox-Id → lookup → vsock
  │                       (internal :5008)           ▼                        ▲        → envd (daemon/)
  │  data  (X-Sandbox-Id: id)                  client-proxy ───────────────────┘
  └─────────────────────────────────────────►  (data :8081)   catalog.Get(id) = orchestrator data-proxy addr
        data_url  (learned from the create response)   └─ catalog (pkg/catalog, in-memory)
```

The key property: **the data bytes never pass through `api`.** `api` writes one catalog row
per sandbox at create time (a cold-path control message) and deletes it at destroy time;
every data request does a *local* map lookup inside `client-proxy` and is reverse-proxied
straight to the orchestrator. `client-proxy` runs **two listeners** — a public data port and
an internal control port — exactly as the orchestrator runs gRPC + data-proxy ports.

## 3. Key design decisions

### Decision 1 — the catalog lives in `client-proxy`; `api` writes it over an internal RPC

E2B's catalog is **Redis**: `api` writes it, `client-proxy` reads it, neither owns it. On one
machine, without standing up a 4th process, one of our two services must host the in-memory
table. We host it in **`client-proxy`** and have `api` push writes:

- **On `Create`:** `api` calls `PUT /routes/{id}` on the client-proxy's internal port with the
  node (= the orchestrator's data-proxy address).
- **On `Delete`:** `api` calls `DELETE /routes/{id}`.
- **On every data request:** `client-proxy` does a *local* `catalog.Get(id)` and routes.

Why this side of the seam (vs. hosting the catalog in `api` and having `client-proxy` read it):

- **It fully achieves the stage's goal** — "sink the data plane off `api`." The data path is a
  local map read in `client-proxy`; it never reaches `api`, with no caching machinery needed.
  Hosting the catalog in `api` would put a per-request lookup back on `api` (or force a cache
  with staleness handling on the hot path).
- **Locality on the hot path.** The catalog is read on every data request (hot) and written on
  every lifecycle event (cold). Put the state in the process that reads it hot; push the rare
  writes. The cold-path cost (one api→client-proxy call per create/delete) is the cheap side.
- **No authority concern.** The catalog is *ephemeral routing state*, rebuildable from the
  orchestrator's live registry; it is not the durable record. The durable record stays in
  `api`'s SQLite (`pkg/store`); the live truth stays in the orchestrator's in-memory registry.
  It is fine for the edge to hold the ephemeral routing cache.

**Swap to Redis later (Stage 12)** is a one-implementation change behind the `pkg/catalog`
interface: `api` writes Redis, `client-proxy` reads Redis, and the internal RPC disappears —
the catalog becomes the shared store both reach, exactly as in E2B.

### Decision 2 — `pkg/catalog` interface (E2B-shaped `sandbox → node`)

```go
package catalog

// Catalog maps a sandbox id to its node. Here "node" is the orchestrator's data-proxy
// address (e.g. "127.0.0.1:5007"); in E2B it is the orchestrator's IP and client-proxy
// hits :5007 on it. Same shape. The interface is the seam: InMemory now, Redis later.
type Catalog interface {
	Set(id, node string)
	Get(id string) (node string, ok bool)
	Delete(id string)
}

// InMemory is a sync.RWMutex-guarded map. client-proxy embeds one.
```

Unit-tested KVM-free: `Set`/`Get`/`Delete`, absent-id, overwrite, concurrent access.

### Decision 3 — `client-proxy` runs two listeners

Modeled on the orchestrator's two-listener shape (`cmd/orchestrator/main.go`):

| Listener | Route | Behavior |
|---|---|---|
| **data** `--addr` (default `:8081`) | `GET /health` | client-proxy's own liveness (the fixture / dev-up wait on it) |
| | `/{rest...}` (catch-all) | read `X-Sandbox-Id`; `catalog.Get` → reverse-proxy to `http://{node}/{rest}`, **preserving `X-Sandbox-Id`**; 400 if the header is missing, 404 if the id is unknown |
| **internal** `--internal-addr` (default `:5008`) | `PUT /routes/{id}` `{"node": …}` | `catalog.Set` (api calls this on Create) |
| | `DELETE /routes/{id}` | `catalog.Delete` (api calls this on Delete) |

The data reverse proxy reuses the discipline `api`'s temporary `dataProxy` uses today
(`cmd/api/main.go`): `FlushInterval: -1` so `/execute`'s SSE streams live, body/headers passed
through untouched. The control endpoints get their **own port** so the data port's catch-all
`/{rest...}` can't swallow `/routes/...` and so the routing table isn't writable from the
public data port. Plaintext loopback, like the rest of the on-machine plane (no auth — out of
scope, per E2B on-cluster).

### Decision 4 — `api` becomes lifecycle-only; `Create` returns `data_url`

- **Delete the passthrough.** Remove `handleProxy` (`handlers.go`), the `dataProxy` field and
  the `--orchestrator-proxy` *proxying* use (`main.go`), and the `/sandboxes/{id}/{rest...}`
  route. After Stage 9 `api` serves only `GET /health`, `POST /sandboxes`, `DELETE
  /sandboxes/{id}`, `GET /sandboxes`.
- **Register on Create / deregister on Delete.** `handleCreate` (after the store insert) calls
  the client-proxy internal port `PUT /routes/{id} {node: orchestratorProxyAddr}`;
  `handleDestroy` calls `DELETE /routes/{id}`. `api` gains `--client-proxy-internal`
  (the internal control address it writes to).
- **`--orchestrator-proxy` is repurposed, not removed.** It stops meaning "where I proxy data"
  and starts meaning "the node value I register in the catalog for sandboxes on this
  orchestrator." (`--orchestrator-grpc` is unchanged: lifecycle still goes over gRPC.)
- **`POST /sandboxes` response grows** from `{"id"}` to `{"id","data_url"}` — `api` tells the
  SDK where to send the data path (E2B-style: the server returns the connection endpoint).
  `data_url` comes from a new `--data-url` flag (default `http://127.0.0.1:8081`, the
  client-proxy public data port). It is constant across sandboxes this stage; Stage 12 makes
  it a per-sandbox `<port>-<id>` hostname, so returning it per-create is forward-looking.

**Registration is load-bearing, not best-effort.** Unlike the store write (a sandbox runs fine
without its metadata row — the orchestrator registry is the operational truth), a sandbox with
no catalog row is unreachable on the data path. So if registration fails, `Create` **rolls
back**: it calls the orchestrator's `Delete` to destroy the just-built VM and returns 500. No
booted-but-unroutable zombie is left behind.

### Decision 5 — the SDK splits its data path to `client-proxy` (user surface unchanged)

`src/microsandbox/client.py`, transport layer only:

- `_create` stores `self._data_url = info.get("data_url")` (falling back to a `data_url=`
  constructor arg / `MICROSANDBOX_DATA_URL` env / `http://127.0.0.1:8081`, for direct testing).
- Data calls (`_stream`, `_post_json`) target `self._data_url + <daemon path>` with header
  `X-Sandbox-Id: {id}`, instead of `base_url + /sandboxes/{id}<path>`. The `/sandboxes/{id}`
  prefix is dropped for data (lifecycle still uses `base_url + /sandboxes/...`); `_sandbox_path`
  is retired.
- **Wire bytes unchanged.** `ExecuteRequest`'s JSON, the SSE `OutputEvent` parsing, and the
  file/command request/response shapes (`protocol.py`) do not change — only the URL and one
  added header. `run_code` / `sandbox.files` / `sandbox.commands` are untouched.

### Decision 6 — testable without KVM; the e2e suite is the parity oracle

- **Go units, KVM-free:** `pkg/catalog` CRUD + concurrency; `client-proxy`'s header routing
  (missing header → 400, unknown id → 404, known id → forwarded to the right node with the
  header preserved) and its internal register/deregister endpoints, all via `httptest` — no VM.
  `go test ./services/...` stays green anywhere.
- **`tests/conftest.py` grows** from launching the pair (orchestrator + api) to launching the
  **trio**: orchestrator, then client-proxy, then api (pointed at both). It waits on each one's
  `/health`. The SDK learns `data_url` from the create response, so the fixture still only
  yields the api base URL. The microVM group still auto-skips without go/firecracker/kvm.
- **`scripts/build-services.sh` is unchanged** — it builds one binary per `cmd/*`, so it picks
  up `client-proxy` automatically. `scripts/dev-up.sh` grows to start the trio.
- **Parity proof:** the existing Python e2e suite passing unchanged through the new data path
  (SDK → client-proxy → catalog → orchestrator → vsock → envd) *is* the proof the
  decomposition altered topology only — same discipline as Stage 7/8.

## 4. Code "from → to" map

| Now (Stage 8 end state) | Stage 9 |
|---|---|
| `cmd/api/main.go` `dataProxy` field + `/sandboxes/{id}/{rest...}` route | **deleted** (9c) |
| `cmd/api/handlers.go` `handleProxy` | **deleted** (9c); replaced by catalog register/deregister in `handleCreate`/`handleDestroy` |
| `cmd/api` `POST /sandboxes` → `{"id"}` | `{"id","data_url"}`; new `--data-url`, `--client-proxy-internal` flags; `--orchestrator-proxy` repurposed to the registered node value |
| — (new) | `pkg/catalog/` (`Catalog` interface, `InMemory`, `catalog_test.go`) |
| — (new) | `cmd/client-proxy/` (data listener + internal listener + reverse proxy + tests) |
| `client.py` data via `base_url + /sandboxes/{id}/…` | data via `data_url + …` + `X-Sandbox-Id`; `_sandbox_path` retired |
| `tests/conftest.py` launches orchestrator + api | launches orchestrator + **client-proxy** + api |
| `scripts/dev-up.sh` starts 2 services | starts 3 services |
| `cmd/orchestrator/*`, `daemon/`, `protocol.py` bytes | **unchanged** |

## 5. Go layout introduced this stage

```
services/
  cmd/
    client-proxy/
      main.go          # two listeners: data (:8081) + internal control (:5008); wires the catalog
      proxy.go         # data handler: X-Sandbox-Id -> catalog.Get -> reverse-proxy to the node
      routes.go        # internal handler: PUT/DELETE /routes/{id} -> catalog.Set/Delete
      clientproxy_test.go
  pkg/
    catalog/
      catalog.go       # Catalog interface + InMemory
      catalog_test.go
```

(`cmd/api`, `cmd/orchestrator`, the other `pkg/*`, and `proto/` are edited in place or
untouched; no new module — this all lives in the existing `microsandbox/services` module.)

## 6. Three independently verifiable sub-steps

### Stage 9a — `pkg/catalog` + `client-proxy`; wire api→client-proxy registration (api keeps its passthrough)
Add `pkg/catalog` (interface + `InMemory` + tests). Stand up `cmd/client-proxy` with both
listeners (data + internal control), unit-tested with `httptest`. Give `api` a catalog client
that registers on Create / deregisters on Delete (with the rollback-on-failure of Decision 4) —
but **leave `api`'s data passthrough in place**, so the SDK is unaffected and the path the e2e
suite exercises is unchanged. **Verify:** `go test ./services/...` green (new catalog +
client-proxy unit tests) + full Python e2e green (unchanged SDK path).

### Stage 9b — flip the SDK's data path to `client-proxy`; conftest + dev-up start the trio
`api`'s `POST /sandboxes` returns `data_url`; the SDK sends its data calls there with
`X-Sandbox-Id`. `conftest.py` and `dev-up.sh` start orchestrator + client-proxy + api. The e2e
suite now exercises the full new path; `api`'s passthrough is still present but unused.
**Verify:** full e2e green through `client-proxy` (the parity proof).

### Stage 9c — remove `api`'s data passthrough; `api` is lifecycle-only
Delete `handleProxy`, the `dataProxy` field, and the `/sandboxes/{id}/{rest...}` route.
**Verify:** full e2e green (api now never on the data path) + an assertion that `api`'s
`/sandboxes/{id}/execute` now 404s (the passthrough is gone). Then update `CLAUDE.md`,
`docs/ARCHITECTURE.md`, and the README to describe the api/client-proxy/orchestrator trio.

## 7. Keeping tests green (honest trade-offs)

- **9a stands the new component up and unit-tests it before any traffic moves**; 9b flips the
  SDK and proves the new path with the unchanged e2e suite; 9c removes the now-dead passthrough.
  Same find-it/prove-it/clean-it cadence as Stage 8b/8c — tests green at every step.
- **Registration is load-bearing (Decision 4):** unlike the best-effort store write, a failed
  catalog write means an unreachable sandbox, so `Create` rolls the VM back rather than
  returning a zombie. This is the one place Stage 9 adds failure-path logic.
- **No new dependencies.** `pkg/catalog` is a plain stdlib in-memory structure; `client-proxy`
  uses `net/http` + `httputil.ReverseProxy`, the same toolkit `api`'s passthrough uses today.
- **The api→client-proxy registration coupling** is the single-machine stand-in for "api writes
  the catalog store"; it is marked in code as the seam that becomes a shared-store write when
  the catalog swaps to Redis (Stage 12). It is not the intended final shape, just its
  one-machine form behind the `pkg/catalog` interface.
- **Safety note carried forward:** still a learning implementation, not security-audited. Stage
  9 adds no network face to the sandboxes (still vsock); the "fully offline" property holds.
  Docs must keep saying it is not safe to expose to untrusted input.
```