# Stage 8 design: split the control plane into `api` + `orchestrator` (gRPC)

> Status: **agreed direction.** First stage of the E2B-alignment refactor — read
> `docs/E2B_ALIGNMENT_ROADMAP.md` (the map) and `docs/ARCHITECTURE.md` (today's layers)
> first. Stage 8 establishes E2B's #1 seam — a REST `api` in front of a per-node gRPC
> `orchestrator` — and a metadata store, while leaving the wire protocol, the in-VM
> daemon (`daemon/`), and the SDK's surface byte-stable. Three sub-steps (8a → 8b → 8c).

## 1. Goal & non-goals

**Goal.** Dissolve the single `control-plane/` binary into the first two E2B services:

- **`orchestrator`** (per machine) — a **gRPC `SandboxService`** that owns the Firecracker
  microVM fleet (cold start / snapshot restore / destroy) and the warm pool, plus a
  **per-node HTTP data proxy** (the vsock bridge) that routes data-plane traffic to the
  right VM.
- **`api`** — the public **REST** front: it persists sandbox metadata (SQLite), picks a
  node (trivially — there is one), and drives lifecycle by calling the orchestrator over
  gRPC. It temporarily forwards the data path to the orchestrator's proxy so the SDK
  keeps a single base URL this stage.

Today's logic is **ported, not rewritten**: `control-plane/`'s `microvm.go` / `pool.go` /
`proxy.go` / `template.go` move under `services/pkg/`, and `server.go`'s handlers split
across the two binaries. The point of Stage 8 is the *boundary*, not new behavior.

**Non-goals** (kept out to bound the diff; each is a later stage):

- **No `client-proxy` yet** (Stage 9). The data plane still enters through `api`, which
  reverse-proxies it to the orchestrator's per-node proxy. That passthrough is explicitly
  temporary scaffolding removed in Stage 9.
- **No `TemplateService` yet** (Stage 10). Template **name → artifact path** resolution
  stays as today (`pkg/template`, `vendor/templates/<name>/`); building is still the
  manual `scripts/build-*.sh`.
- **Don't touch `daemon/` (envd), `protocol.py`, or the data-plane bytes.** `/health`
  `/execute` `/files/*` `/commands` and their JSON/SSE shapes are unchanged; only *who
  proxies them* moves. The SDK's request/response surface is unchanged this stage.
- **No networking change** — vsock stays; **no auth**; **no real placement** (the api
  holds one orchestrator address by flag); **no `Pause`/`Resume`** behavior (defined in
  the proto for fidelity, but the server returns `Unimplemented` — they need runtime
  checkpointing, Stage 12+).

## 2. Target architecture (Stage 8 end state)

```
                    REST                                  gRPC SandboxService
  Python SDK ───────────────►  api  ─────────────────────────────────────────►  orchestrator
 (base_url = api, unchanged)    │  POST /sandboxes  → Create                        │ (one per machine)
                                │  DELETE /sandboxes/{id} → Delete                   │  pkg/fc   : Firecracker lifecycle
                                │  GET /sandboxes  → store / List                    │  pkg/pool : warm pool (--pool*)
                                │  persists sandbox rows → SQLite (pkg/store)        │  pkg/template : name→artifacts
                                │                                                    │
                                │  data plane (TEMPORARY this stage):               │  per-node data proxy  :5007
                                │  ANY /sandboxes/{id}/{rest} ──reverse-proxy──────► │  ANY /{rest}, X-Sandbox-Id
                                                                                     │      │ look up VM by id
                                                                                     │      ▼ vsock bridge (pkg/proxy)
                                                                                     └────► envd in the VM  (daemon/, UNCHANGED)
```

The orchestrator runs **two listeners**: gRPC (lifecycle, e.g. `:9090`) and the HTTP data
proxy (e.g. `:5007`). The proxy is **header-routed** (`X-Sandbox-Id`) from the start —
that is exactly the contract `client-proxy` will speak in Stage 9, so Stage 9 just slots
`client-proxy` into the role `api`'s temporary passthrough plays here.

## 3. Key design decisions

### Decision 1 — the gRPC `SandboxService` contract (the seam)

`services/proto/orchestrator/orchestrator.proto`, method names mirroring E2B's
`SandboxService`:

```proto
service SandboxService {
  rpc Create(SandboxCreateRequest) returns (SandboxCreateResponse);
  rpc Delete(SandboxDeleteRequest) returns (google.protobuf.Empty);
  rpc List  (google.protobuf.Empty)  returns (SandboxListResponse);
  rpc Pause (SandboxPauseRequest)  returns (google.protobuf.Empty);   // Unimplemented in Stage 8
  rpc Resume(SandboxResumeRequest) returns (SandboxCreateResponse);   // Unimplemented in Stage 8
}
message SandboxConfig {                 // what api hands the orchestrator
  string template      = 1;             // "" = default image
  bool   from_snapshot = 2;             // restore (warm-pool eligible) vs cold start
  uint32 vcpu          = 3;             // 0 = orchestrator default (1)
  uint32 mem_mb        = 4;             // 0 = orchestrator default (512)
}
message SandboxCreateRequest  { SandboxConfig config = 1; }
message SandboxCreateResponse { string sandbox_id = 1; }   // the "sb_…" id, already health-probed
message SandboxDeleteRequest  { string sandbox_id = 1; }
message SandboxListResponse   { repeated string sandbox_ids = 1; }
```

The orchestrator keeps the **in-memory `id → microVM` registry** it has today (server.go);
`Create` mints the id, builds the VM (pool / restore / cold-start, all already
health-probed via `healthyOrDestroy`), registers it, and returns the id. The data proxy
resolves `X-Sandbox-Id` against that same registry. **Plaintext gRPC** (insecure creds),
like E2B on-cluster — TLS/auth is out of scope.

### Decision 2 — module layout: a `services/` module; dissolve `control-plane/`

New host module `microsandbox/services` rooted at `services/` (see the roadmap for why
this shape). The four lifecycle files move verbatim-as-possible into focused packages;
`server.go` / `main.go` split into the two `cmd/` mains. `control-plane/` (its own
module) is **deleted** once the move is green. `daemon/` is untouched. A repo-root
`go.work` (`use (./services ./daemon)`) is added for ergonomics.

### Decision 3 — metadata store `pkg/store` (SQLite + sqlc)

`api` owns metadata, like E2B's `api` owns Postgres. Stage 8 adds one table:

```sql
CREATE TABLE sandboxes (
  id                TEXT PRIMARY KEY,   -- "sb_…"
  template          TEXT NOT NULL,
  status            TEXT NOT NULL,      -- "running" (lifecycle states grow later)
  orchestrator_addr TEXT NOT NULL,      -- which node holds it (one for now; real in Stage 9+)
  created_at        TIMESTAMP NOT NULL
);
```

Typed queries via `sqlc generate` from `pkg/store/{schema.sql,queries.sql}`; driver is
`modernc.org/sqlite` (cgo-free → the api binary stays static). `api` inserts on Create,
deletes on Delete, and serves `GET /sandboxes` from the table. The `templates` table
arrives with the `TemplateService` in Stage 10. The DB path is a flag (default
`vendor/microsandbox.db`); tests use a temp path.

### Decision 4 — `api` is lifecycle + a *temporary* data passthrough

`api`'s REST surface (Go 1.22 `ServeMux`):

| Route | Does |
|---|---|
| `GET /health` | api liveness (the test fixture waits on it) |
| `POST /sandboxes` `{from_snapshot, template}` | gRPC `Create`; insert row; `201 {"id"}` |
| `DELETE /sandboxes/{id}` | gRPC `Delete`; delete row; `204` / `404` |
| `GET /sandboxes` | list from the store; `200 {"sandboxes":[…]}` (new) |
| `ANY /sandboxes/{id}/{rest...}` | **temporary**: reverse-proxy to the orchestrator data proxy with `X-Sandbox-Id: {id}`, path `/{rest}` |

The passthrough keeps `Sandbox(base_url=…)` pointing at one place this stage, so the SDK
needs **no change** in Stage 8. Stage 9 deletes this row and the SDK sends data to
`client-proxy` instead. The body/headers/SSE pass through untouched (same
`httputil.ReverseProxy` + flush-every-write discipline as `pkg/proxy` uses for vsock).

### Decision 5 — `orchestrator` owns VMs, the pool, and the per-node proxy

The `--vendor-dir`, `--pool-size`, `--pool name=K` flags move onto `orchestrator`
(it owns VMs, so it owns warming). `checkHostArtifacts`, the firecracker REST calls, and
the snapshot/`vsock_override` restore all move into `pkg/fc` unchanged. The data proxy is
`pkg/proxy`'s vsock bridge, now fronted by an HTTP handler that reads `X-Sandbox-Id`
instead of a path wildcard. Graceful shutdown (destroy pools + registry) stays on the
orchestrator.

### Decision 6 — gRPC codegen as a reproducible Go-tool step

`protoc-gen-go` + `protoc-gen-go-grpc` pinned as `go tool` dependencies (Go 1.24+), driven
by a `//go:generate` line so `go generate ./services/proto/...` regenerates
`services/pkg/grpc/orchestrator/*.pb.go` without a system-wide protoc install ritual.
(`protoc` itself is still required on PATH; documented in the build script. If that proves
annoying we switch to `buf`, but `go tool` keeps the dependency set in `go.mod`.)

### Decision 7 — testable without KVM; the e2e suite is the parity oracle

- **Go units, KVM-free:** the moved `pool_test.go` / `pools_test.go` / `proxy_test.go` /
  `template_test.go` keep passing in their new packages; new ones cover the proto
  round-trip and `pkg/store` CRUD. `go test ./services/...` is green anywhere.
- **`tests/conftest.py` grows** from launching one `vendor/control-plane` to launching the
  pair: start `orchestrator` (gRPC + proxy ports), then `api` (pointed at them), wait on
  `api`'s `/health`. The microVM group still auto-skips without go/firecracker/kvm.
- **Parity proof:** the existing Python e2e suite
  (`test_sandbox`/`test_stateful`/`test_files`/`test_microvm`/`test_microvm_snapshot`/`test_template`)
  passing unchanged *is* the proof the decomposition altered topology only — same
  discipline as Stage 7's rootfs flip.

## 4. Code "from → to" map

| Now (`control-plane/`, one `package main`) | Stage 8 (`services/` module) |
|---|---|
| `microvm.go` (FC lifecycle, snapshot restore, `checkHostArtifacts`) | `pkg/fc/` |
| `pool.go` (+ `pool_test.go`, `pools_test.go`) | `pkg/pool/` |
| `proxy.go` (vsock round-tripper, `waitHealthy`) (+ `proxy_test.go`) | `pkg/proxy/` |
| `template.go` (`resolveTemplate`) (+ `template_test.go`) | `pkg/template/` |
| `server.go` `handleCreate`/`handleDestroy` + registry | `cmd/orchestrator` gRPC `Create`/`Delete`/`List` impl |
| `server.go` `handleProxy` (path-wildcard) | `cmd/orchestrator` data proxy (`X-Sandbox-Id` header) |
| `server.go` `parsePoolSpecs`, pool wiring | `cmd/orchestrator` (flags + wiring) |
| `main.go` flags/mux/shutdown | split: `cmd/orchestrator/main.go` (gRPC+proxy+pool) and `cmd/api/main.go` (REST+gRPC client+store) |
| `scripts/build-control-plane.sh` | `scripts/build-services.sh` (builds all of `cmd/`) |
| `control-plane/go.mod`, the whole dir | **deleted** after the move is green |
| — (new) | `proto/orchestrator/orchestrator.proto`, `pkg/grpc/orchestrator/` (generated) |
| — (new) | `pkg/store/` (schema.sql, queries.sql, sqlc output) |

## 5. Go layout introduced this stage

```
services/
  go.mod                       # module microsandbox/services  (go 1.25)
  proto/orchestrator/orchestrator.proto
  cmd/
    orchestrator/main.go       # gRPC SandboxService (:9090) + data proxy (:5007) + warm pool
    api/main.go                # REST (:8080) → gRPC client + SQLite + temp data passthrough
  pkg/
    fc/        # ← microvm.go
    pool/      # ← pool.go (+ tests)
    proxy/     # ← proxy.go (+ tests)
    template/  # ← template.go (+ tests)
    store/     # schema.sql, queries.sql, sqlc-generated Go, hand-written wrapper
    grpc/orchestrator/   # generated *.pb.go
go.work                        # use (./services ./daemon)
```

## 6. Three independently verifiable sub-steps

### Stage 8a — carve `pkg/`, stand up the `services/` module, keep behavior identical
Pure relocation, no new seam. Create `services/go.mod`; move `microvm/pool/proxy/template`
into `pkg/{fc,pool,proxy,template}` (package renames, export what the mains need); rebuild
**today's exact HTTP behavior** as a single `cmd/orchestrator` (same REST surface for now)
so nothing above it notices. Repoint `scripts/build-*.sh` and `tests/conftest.py` at the
new binary; delete `control-plane/`. **Verify:** `go test ./services/...` (moved unit
tests green) + full Python e2e green — behavior is byte-identical, only the code moved.

### Stage 8b — introduce the gRPC seam: `orchestrator` (gRPC) + `api` (REST)
Add `orchestrator.proto` + codegen. `cmd/orchestrator` drops its REST surface and serves
gRPC `SandboxService` (Create/Delete/List over `pkg/fc`+`pkg/pool`) **plus** the
header-routed data proxy. New `cmd/api` serves the REST surface, calls the orchestrator
over gRPC for lifecycle, and reverse-proxies the data path (temporary). `conftest.py`
launches orchestrator then api. **Verify:** proto round-trip unit test + full e2e green
(SDK still uses one base URL = api).

### Stage 8c — persist metadata in SQLite (`pkg/store`)
Add `pkg/store` (sqlc + `modernc.org/sqlite`) and the `sandboxes` table; `api` inserts on
Create, deletes on Delete, and serves `GET /sandboxes` from it. **Verify:** `pkg/store`
CRUD unit test + full e2e green (additive on 8b).

## 7. Keeping tests green (honest trade-offs)

- **8a is a no-behavior-change refactor** — the riskiest *mechanical* step (a big move +
  `control-plane/` deletion) is taken first and proven by the unchanged e2e suite, before
  any new seam is added. If 8a is green, the move is sound.
- **New dependencies, called out, not drift:** 8b adds gRPC (`google.golang.org/grpc`,
  `google.golang.org/protobuf`) + the codegen tools; 8c adds `modernc.org/sqlite` and
  `sqlc`. The host side was stdlib-only; this is the conscious, documented move off it,
  justified by "the gRPC seam and a real store are the E2B lesson."
- **`protoc` must be on PATH** for `go generate` (Decision 6). The build script checks for
  it and prints install guidance; generated files are committed so a normal build/test
  needs only the Go toolchain, not protoc.
- **The temporary api→proxy passthrough (Decision 4)** is the one deliberately un-E2B-like
  thing in Stage 8; it exists only so the SDK is untouched until `client-proxy` lands in
  Stage 9, where it is deleted. Marked `// TEMPORARY (Stage 8): removed in Stage 9` in
  code so it is not mistaken for the intended shape.
- **After 8c lands:** update `CLAUDE.md` (the control plane is now `api`+`orchestrator`;
  `go test ./services/...`; the new dev-up flow), `docs/ARCHITECTURE.md` (the new seam),
  and the README.
