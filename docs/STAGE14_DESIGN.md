# Stage 14 design: the storage swaps go live — catalog → Redis, store → Postgres

> Status: **done** (14a + 14b landed; 14c is this doc sweep). The first half of the roadmap's
> last remaining *deferred* item (`docs/E2B_ALIGNMENT_ROADMAP.md` §5 "Still deferred": *"the
> storage swaps go live: SQLite→Postgres, in-mem→Redis, Local→object storage"*). Decision D3
> built all three state seams behind **E2B-shaped interfaces** precisely so the swap would be a
> one-implementation change, not a redesign. This stage cashed in two of the three:
> **`pkg/catalog` in-mem → Redis** (14a) and **`pkg/store` SQLite → Postgres** (14b). The third
> (`Local → object storage`) is split out to **Stage 15** (see §10). Real-machine e2e stayed
> **37/37** on Postgres + Redis.
>
> **Scope split (decided with the user).** The three seams are *not* equally hard. Catalog
> and store are near-isomorphic — the catalog is already an interface, and the store rides
> `database/sql`, so both are "swap the driver, learn the ops." Object storage is **not**
> isomorphic: `pkg/storage`'s own comment flags that the Firecracker snapshot bakes in its
> rootfs's *absolute path*, so artifacts can't merely live in a bucket — they must be
> *materialized to a local path before boot*, and that's exactly where Stage 13's UFFD
> (pluggable memfile page source) pays off. So **object storage is split out into its own
> Stage 15** (`Local → object storage`), and this doc covers only the two isomorphic swaps.
>
> **Why bother, honestly.** On *one box* this is **not** a performance win — Redis and
> Postgres each add a socket hop + serialization that the in-process map and the local
> SQLite file don't pay. The payoff is **fidelity and the properties the swap unlocks**:
> (1) a catalog that is genuinely *shared across processes* (the api writes it, client-proxy
> reads it) instead of living inside one process behind a control-RPC shim; (2) a store with
> *real* concurrency (no single-writer lock) that *survives a restart* the same way E2B's
> Postgres does; and (3) the precondition for the still-deferred multi-host work (a shared
> catalog + shared store is what lets a second orchestrator/proxy join). This doc claims no
> latency improvement; it claims the architecture gets one step more real.

## 1. Goal & non-goals

**Goal.** Make the running control plane keep its two pieces of mutable state in the same
*kind* of place E2B does, behind the interfaces D3 already defined:

- **`pkg/catalog` → Redis.** Add a `catalog.Redis` implementing the existing
  `Catalog{Set,Get,Delete}` interface, backed by `github.com/redis/go-redis/v9` (pure Go).
  The api writes `sandbox:<id> → node` **directly** to Redis on create; client-proxy reads
  it **directly** on every data request. This **removes the api→client-proxy internal
  control RPC** (`PUT/DELETE /routes/{id}`, the `--internal-addr`/`--client-proxy-internal`
  flags, `client-proxy`'s `handleRouteSet/Delete`, and the api's `catalogClient`) — that
  channel only existed because the map lived inside one process. Result: the topology gets
  *closer* to E2B, not just a backend swap.
- **`pkg/store` → Postgres.** Extract a `store.Store` **interface** from today's concrete
  type, keep the SQLite implementation, and add a Postgres one backed by
  `github.com/jackc/pgx/v5` (pure Go, via its `database/sql` `stdlib` adapter). A single
  `store.Open(dsn)` dispatches by URL scheme (`postgres://…` vs `sqlite:///…`). Porting the
  three SQLite-isms — `?`→`$N` placeholders, the `SetMaxOpenConns(1)` single-writer cap, and
  `created_at` stored as text — *is* the lesson.
- **Flip the defaults (decided with the user).** The binaries now **default to Postgres +
  Redis**; the e2e suite provisions both via a new `docker-compose.yml`. The lightweight
  implementations are demoted, not deleted (see Decision 1) — SQLite stays selectable for
  the store; the in-mem catalog survives only as a unit-test double (it can no longer back
  the running system once the control RPC is gone — Decision 2).

**Non-goals** (bounded out / deferred):

- **Object storage (`pkg/storage`).** Deferred to **Stage 15** — it is non-isomorphic (the
  snapshot's baked-in absolute path) and is where UFFD's pluggable page source matters. Not
  touched here.
- **Real schema migrations.** Today's `CREATE TABLE IF NOT EXISTS` on `Open` (a "poor man's
  migration") is kept for both engines. Versioned migrations (golang-migrate, etc.) are a
  noted later concern, not this stage.
- **`sqlc`.** Roadmap §4 floated `sqlc`; with six hand-written statements, plain
  `database/sql` stays clearer. The interface is the seam, not the codegen.
- **Auth / TLS / multi-host.** Plaintext, single-machine, as everywhere else in this repo.
  Redis and Postgres run with no auth on loopback. **Not** safe to expose — same standing
  caveat as the rest of the project.
- **A latency claim.** See the honesty note above.

## 2. Target architecture (what moves)

The two seams move in opposite-shaped ways: the catalog change is mostly about **topology**
(who writes/reads, and deleting a shim), the store change is mostly about **dialect** (same
caller, a second driver behind an interface).

```
  BEFORE (Stage 13)                          AFTER (Stage 14)

  ┌─ api ─┐  PUT/DELETE /routes/{id}          ┌─ api ─┐
  │       │ ───────────────────────►          │       │ ──SET sandbox:<id> <node>──┐
  │       │     (internal control RPC,         │       │                            ▼
  └───────┘      :5008 — REMOVED)              └───────┘                       ┌─ Redis ─┐
                       │                                                       │ catalog │
                       ▼                       ┌ client-proxy ┐ ──GET──────────┤ :6379   │
              ┌ client-proxy ┐                 │  (hot path)  │ ◄──────────────┘─────────┘
              │ in-mem map   │  ──reads own────└──────────────┘
              │ (1 process)  │     map
              └──────────────┘

  ┌─ api ─┐ database/sql (?, 1 conn)           ┌─ api ─┐ store.Open("postgres://…")
  │ store │ ──────────► SQLite file            │ store │ ──pgx ($N, pooled)──► Postgres :5432
  └───────┘  (modernc.org/sqlite)              └───────┘   (interface; sqlite:// still works)
```

Component → what changes:

| Component | Change |
|---|---|
| `pkg/catalog` | + `redis.go` (`Redis`, implements `Catalog`); `InMemory` kept as a test double |
| `cmd/api` | writes Redis directly (`catalog.Redis`); **deletes** `catalogClient` + `--client-proxy-internal`; store via `--store-dsn` (default Postgres) |
| `cmd/client-proxy` | reads Redis directly; **deletes** the internal control listener (`--internal-addr`, `handleRouteSet/Delete`) |
| `pkg/store` | `Store` becomes an **interface**; `sqlite.go` (today's code) + `postgres.go` (pgx); `Open(dsn)` dispatches by scheme |
| infra | new repo-root `docker-compose.yml` (postgres + redis); `conftest`/`dev-up.sh` bring them up |
| deps | + `github.com/redis/go-redis/v9`, `github.com/jackc/pgx/v5` (both pure Go) |

The data path itself is **unchanged**: SDK → client-proxy → orchestrator → VM NIC → envd.
Only *where the routing fact lives* (Redis, not a process-local map) and *where the metadata
lives* (Postgres, not a file) move. The Python e2e is the behavioral oracle, as since
Stage 11 — a green suite proves the swap changed *state location*, not behavior.

## 3. The two swaps in detail

### 3.1 Catalog → Redis (the topology one)

The Stage 9 design put the in-mem catalog *inside* client-proxy because that's the process
that reads it on the hot path, and had the api mutate it over an internal HTTP control port
(`PUT/DELETE /routes/{id}`). That control RPC was always a **single-machine shim** for "the
map lives in another process's memory." Redis is a shared store both processes can reach, so:

- **api, on create:** after the orchestrator returns the VM, `SET sandbox:<id> <node>` in
  Redis (where `<node>` is `--orchestrator-proxy`, exactly today's value). The Stage 9
  rollback discipline is preserved — a failed `SET` (Redis down) rolls the VM back, just as
  a failed control-RPC `PUT` did. On destroy, `DEL sandbox:<id>`.
- **client-proxy, on each data request:** `GET sandbox:<id>` to find the node, then
  reverse-proxy as today. The catalog read is now a Redis round-trip instead of a map lookup
  — negligible on loopback, and off the per-page hot path (it's per *request*, not per byte).
- **deleted:** the api's `catalogClient`, the `--client-proxy-internal` flag, client-proxy's
  `--internal-addr` listener and `handleRouteSet/handleRouteDelete`. Net **less** code.

Redis key shape mirrors E2B's `sandbox:catalog:<id>`; we use `sandbox:<id>` → node string.
A short **TTL with refresh** (E2B expires catalog rows) is a *noted option* but out of scope
here — our api explicitly `DEL`s on destroy, so we don't rely on expiry for correctness.

### 3.2 Store → Postgres (the dialect one)

Today `store.Store` is a concrete struct over `*sql.DB`. We extract the **interface** (the
six methods already on it: `InsertSandbox/DeleteSandbox/ListSandboxes` +
`InsertBuild/UpdateBuild/ListBuilds`, plus `Close`) and provide two implementations behind a
scheme-dispatching `Open`:

```go
type Store interface {
    InsertSandbox(id, template string) error
    DeleteSandbox(id string) error
    ListSandboxes() ([]Sandbox, error)
    InsertBuild(buildID, name string) error
    UpdateBuild(buildID, state, detail string) error
    ListBuilds() ([]Build, error)
    Close() error
}

// Open dispatches by DSN scheme: "postgres://…" → pgx, "sqlite:///path" (or bare path) → modernc.
func Open(dsn string) (Store, error)
```

The three SQLite-isms and how each ports — this table *is* the teaching content of 14b:

| SQLite-ism (today) | Why it's there | Postgres port |
|---|---|---|
| `?` placeholders | SQLite/MySQL dialect | `$1, $2, …` (pgx/Postgres) |
| `db.SetMaxOpenConns(1)` | SQLite allows one writer; serializing dodges "database is locked" | **drop it** — Postgres has real MVCC concurrency; use a normal pool |
| `created_at` scanned as `string` | SQLite stores `CURRENT_TIMESTAMP` as text | Postgres returns `timestamptz`; scan into `time.Time` and `.Format(time.RFC3339)` so the api's JSON is byte-identical |

Everything else carries over unchanged: the `schema` string (Postgres also supports `CREATE
TABLE IF NOT EXISTS`, `TIMESTAMP`/`timestamptz`, `DEFAULT CURRENT_TIMESTAMP`), the `Sandbox`/
`Build` structs, and the method semantics (idempotent delete, newest-first list). The api's
call sites (`a.store.Insert…`) don't change — they now hold a `store.Store` interface value
instead of a `*store.Store`.

## 4. Key design decisions

### Decision 1 — flip the default to the real backends; keep the light ones as escape hatches
Per the user. The binaries default to Postgres + Redis (`--store-dsn postgres://…`, the
catalog requires Redis); the e2e provisions them. This trades the old "stdlib-only,
zero-dependency quickstart" for production fidelity — a conscious cost. We **keep** the
lightweight implementations rather than delete them, because they're not dead weight:
- SQLite stays a *first-class store* selectable via a `sqlite://` DSN — the api is its sole
  user, so a single-process / no-docker run still works end to end on SQLite.
- the in-mem catalog stays as a **unit-test double** (it already backs
  `clientproxy_test.go` via the `Catalog` interface), but can **no longer back the running
  system** — see Decision 2.

### Decision 2 — Redis lets us delete the control RPC; that's why in-mem can't back the running system anymore
The api→client-proxy control port existed *only* to mutate a map that lived in another
process. With a shared Redis both processes reach directly, the shim is pure deletion (more
E2B-faithful). The consequence — worth stating because it's the stage's sharpest lesson — is
an **asymmetry**: the *catalog* is shared by two processes (api writes, client-proxy reads),
so once the shim is gone **Redis is not just the default but the only cross-process option**;
the *store* has a single owner (the api), so **SQLite remains viable** as a light alternative.
The number of writers/readers, not the backend's "weight," is what forces Redis here.

### Decision 3 — pure-Go drivers only (the static-binary story is load-bearing)
Every host binary in this repo is static (it's why `pkg/store` uses cgo-free
`modernc.org/sqlite` and `pkg/uffd` is hand-rolled). We hold that line:
`github.com/redis/go-redis/v9` and `github.com/jackc/pgx/v5` (with its `stdlib`
`database/sql` adapter) are both pure Go, no cgo. The binaries stay statically linkable.

### Decision 4 — `store` becomes an interface with two impls (consistency over DRY)
We could keep one struct and rebind placeholders (`sqlx`-style). Instead we extract the
interface and write `sqlite.go` + `postgres.go`, matching how `pkg/catalog` and `pkg/storage`
already expose their seams. The ~40 lines of duplicated SQL is the price of making the swap
*literal and reviewable*: two files, one contract, and the diff between them is exactly the
dialect table in §3.2. For a learning repo that clarity beats the DRYer single-struct trick.

### Decision 5 — provisioning is `docker-compose`; e2e hard-requires it; Go units stay hermetic
docker is already a hard dependency (the rootfs is exported from a docker image), so a
`docker-compose.yml` for `postgres:16-alpine` + `redis:7-alpine` adds no new *class* of
dependency. Two layers, deliberately different:
- **e2e (Python):** the `control_plane` session fixture brings the compose services up
  (reusing them if already running) and points the binaries at them; **missing docker is a
  loud failure, not a silent skip** — that's what "flip the default" means. (The pre-existing
  KVM/firecracker gate still skips the *whole* microVM group on boxes that can't run VMs at
  all, so non-VM CI is unaffected.)
- **Go units (`go test ./services/...`):** stay **hermetic and dependency-free** — they test
  `InMemory`/`sqlite` directly, and the `Redis`/`postgres` variants **auto-skip** unless a
  `REDIS_ADDR`/`MSB_TEST_PG_DSN` env points at a live service (which compose/CI sets). The
  flip applies to the *running system and e2e*, not to forcing every unit test to need a
  server — constructing an impl directly never goes through the default.

### Decision 6 — the parity oracle stays behavioral (unchanged since Stage 11)
Where state lives is invisible to the wire. The Python e2e suite (currently 37/37) must stay
green and unchanged in *count* and *behavior* — now running against Postgres + Redis. A green
suite is the proof that the swap moved storage, not semantics.

## 5. Code "from → to" map

| concern | from (Stage 13) | to (Stage 14) |
| --- | --- | --- |
| catalog backend | `catalog.InMemory` in client-proxy | `catalog.Redis` (shared); `InMemory` → test double |
| catalog write path | api → client-proxy control RPC (`PUT/DELETE /routes/{id}`) | api `SET`s Redis directly; control RPC **deleted** |
| catalog read path | client-proxy reads its own map | client-proxy `GET`s Redis directly |
| client-proxy ports | data `:8081` + internal control `:5008` | data `:8081` only (`--internal-addr` **deleted**) |
| api flags | `--client-proxy-internal`, `--db <sqlite path>` | (former **deleted**); `--store-dsn`, `--redis-addr` |
| store type | concrete `*store.Store` (SQLite) | `store.Store` **interface**; `sqlite` + `postgres` impls |
| store default | SQLite file | Postgres DSN; SQLite via `sqlite://` scheme |
| store concurrency | `SetMaxOpenConns(1)` | native Postgres pool (cap dropped) |
| placeholders / time | `?` / text | `$N` / `time.Time` formatted |
| provisioning | none | `docker-compose.yml`; conftest + dev-up bring it up |
| deps | `modernc.org/sqlite` | + `redis/go-redis/v9`, `jackc/pgx/v5` (pure Go) |

## 6. Layout introduced this stage

```
docker-compose.yml                 # NEW: postgres:16-alpine + redis:7-alpine, healthchecks
services/pkg/catalog/
  catalog.go        # interface + InMemory (unchanged; InMemory now "test double")
  redis.go          # NEW: Redis, implements Catalog over go-redis
  redis_test.go     # NEW: KVM-free; skips unless REDIS_ADDR set
services/pkg/store/
  store.go          # CHANGED: now defines the Store interface + Open(dsn) dispatcher + shared schema/types
  sqlite.go         # today's *Store body, renamed to the sqlite impl
  postgres.go       # NEW: pgx impl ($N placeholders, time.Time scan, normal pool)
  store_test.go     # sqlite hermetic; postgres variant skips unless MSB_TEST_PG_DSN set
services/cmd/api/          # writes Redis directly; catalogClient + --client-proxy-internal deleted; --store-dsn/--redis-addr
services/cmd/client-proxy/ # reads Redis directly; internal control listener deleted
scripts/dev-up.sh          # brings up compose; passes --store-dsn/--redis-addr; drops the internal-addr wiring
tests/conftest.py          # control_plane fixture brings compose up, points binaries at PG/Redis
```

## 7. Three independently verifiable sub-steps

### Stage 14a — catalog → Redis; delete the control RPC ✅
Add `docker-compose.yml` (redis service) + the `go-redis` dep. Implement `catalog.Redis`
(`Set`/`Get`/`Delete` over `sandbox:<id>`). Wire the api to `SET`/`DEL` Redis directly and
client-proxy to `GET` it directly; **delete** the api `catalogClient`, the
`--client-proxy-internal`/`--internal-addr` flags, and client-proxy's internal control
listener + handlers. Add `--redis-addr` to both. `conftest` brings up redis and points both
binaries at it. Go units: `InMemory` test unchanged, `Redis` test skips unless `REDIS_ADDR`.
**Verify:** `go test ./services/...` green; Python e2e green against Redis (routing still
works, create rollback still rolls back if Redis is stopped).

### Stage 14b — store → Postgres behind an interface ✅
Add postgres to `docker-compose.yml` + the `pgx` dep. Extract the `store.Store` interface;
rename today's body to `sqlite.go`; add `postgres.go` (the §3.2 dialect ports); make
`Open(dsn)` dispatch by scheme. Replace the api's `--db` with `--store-dsn` (default a
Postgres DSN; `sqlite:///…` still works); the api field becomes `store.Store`. `conftest`
points the api at the compose Postgres. Go units: sqlite test hermetic, postgres test skips
unless `MSB_TEST_PG_DSN`. **Verify:** `go test ./services/...` green (both store impls where
available); Python e2e green against Postgres (sandboxes + template builds persist and list).

### Stage 14c — docs, defaults, dev-up, honest review ✅
Finalize this doc's status; update `CLAUDE.md` (the "Done" list + the store/catalog
descriptions + common-commands `docker compose up`), `docs/ARCHITECTURE.md` (the state-seam
lines), and the roadmap (move the catalog+store half of "the storage swaps go live" to done;
object storage stays as Stage 15). Make `dev-up.sh` bring up compose and pass the new flags.
Confirm the **warm pool** and **template build** paths are unaffected (they don't touch these
seams). Run the full e2e against Postgres + Redis (target: still 37/37) and give the 🔴/🟡/🟢
self-review.

## 8. Keeping tests green (honest trade-offs)

- **The flip is the cost.** A plain `pytest` now needs docker + the compose services for the
  VM cases (it already needed docker, KVM, firecracker, and passwordless-sudo networking, so
  this is one more provisioned dependency, not a new *kind*). The fixture brings them up so
  the developer experience stays "run pytest," but on a box without docker it now **fails
  loudly** for the VM group instead of running on the old in-process state — that's the
  deliberate meaning of "flip the default," chosen over the silent-skip convenience.
- **Go units stay hermetic** (Decision 5): `go test ./services/...` needs no server — the
  `Redis`/`postgres` variants self-skip without their env. This preserves the project's "host
  units are KVM-/dependency-free" discipline for the parts that *can* be.
- **Behavioral parity, not perf.** This stage must not change the e2e count or any observable
  behavior; if the suite stays 37/37 against Postgres + Redis, the swap is proven correct.
  No latency numbers are claimed (and on one box, none would be favorable — see the header).
- **Safety note carried forward.** Redis and Postgres run with no auth on loopback for this
  single-box learning setup. This remains a learning implementation, **not security-audited**;
  the sandbox is inbound-reachable / outbound-denied (Stage 12), and nothing here makes it
  safe to expose to untrusted input.

## 9. New dependencies (called out, per the roadmap's discipline)

| dependency | why | cgo? |
|---|---|---|
| `github.com/redis/go-redis/v9` | the standard Go Redis client; backs `catalog.Redis` | pure Go |
| `github.com/jackc/pgx/v5` (+ `/stdlib`) | the modern pure-Go Postgres driver; backs `store`'s `postgres.go` via `database/sql` | pure Go |
| docker images `postgres:16-alpine`, `redis:7-alpine` | test/dev provisioning only — not linked into any binary | n/a |

Both Go modules are pure Go, preserving the static-binary property every host service relies
on (the same reason `pkg/store` chose `modernc.org/sqlite`).

## 10. What this sets up (Stage 15 and beyond)

With the catalog and store shared and durable, the remaining deferred work gets its
precondition: **Stage 15 — `Local → object storage`** (materialize a template's
`rootfs.ext4`/`snapfile` to a local cache before boot; stream the `memfile` page-by-page from
the bucket via the Stage-13 UFFD handler — the non-isomorphic seam this stage deliberately
left out), and then the "later — production fidelity" line (auth, real multi-host scheduling
over the now-shared catalog/store, a TypeScript SDK).
