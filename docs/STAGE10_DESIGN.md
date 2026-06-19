# Stage 10 design: `TemplateService` (the template builder) inside `orchestrator` + `pkg/storage`

> Status: **agreed direction.** Third stage of the E2B-alignment refactor — read
> `docs/E2B_ALIGNMENT_ROADMAP.md` (the map), then `docs/STAGE9_DESIGN.md` / `STAGE8_DESIGN.md`
> (the seams already in place) and `docs/STAGE6_DESIGN.md` (the template model this builds on).
> Stage 10 turns today's manual `scripts/build-template.sh` into E2B's **"accept sync, build
> async, poll status"** service: a gRPC `TemplateService` in the orchestrator, a
> `StorageProvider` abstraction, an api REST surface + metadata, and an SDK build call. The
> sandbox create path, the wire protocol, and `daemon/` stay byte-stable. Three sub-steps
> (10a → 10b → 10c).

## 1. Goal & non-goals

**Goal.** Make building a custom template a programmatic, asynchronous operation, mirroring
E2B's template builder:

- **`pkg/storage`** — a `StorageProvider` interface (+ `Local`) abstracting *where a template's
  built artifacts live*. The seam that becomes object storage later.
- **`template-manager.proto` `TemplateService`** — inside the **orchestrator** (E2B keeps the
  builder in the orchestrator). `TemplateCreate` mints a `build_id`, kicks an **async** build
  goroutine, and returns immediately; `TemplateBuildStatus` is polled for progress.
- **`api`** — a REST `/templates` surface that calls the orchestrator's `TemplateService` over
  gRPC and persists a `builds` row to SQLite (Stage 8 explicitly deferred the templates table
  to "the `TemplateService` in Stage 10").
- **SDK** — `build_template(...)`: POST a recipe, poll until ready, then the usual
  `Sandbox(template=name)`.

This is the local equivalent of `e2b template build`: the build wraps the existing pipeline
(`docker build` → `build-rootfs.sh` → `build-snapshot.sh`), runs in the background, and the
caller polls a status — exactly E2B's model.

**Non-goals** (bounded out; each a later stage or a deliberate single-machine simplification):

- **The sandbox create path is unchanged.** `SandboxConfig` still carries the template **name**
  and `pkg/template.Resolve` still maps name → artifact paths. E2B resolves a name → buildID in
  the api and passes the buildID to `SandboxService.Create`; that wire change is deferred (it
  only earns its keep with build history / rollback / multi-host, which are themselves deferred).
- **No buildID-keyed storage layout / no build history.** See Decision 2: the Firecracker
  snapshot bakes in the rootfs's **absolute path**, so artifacts must be built **in place** at
  `vendor/templates/<name>/` (latest wins). The `build_id` is the async-job handle, not a
  storage key.
- **No `ChunkService`** (E2B's build-context upload). The recipe is a Dockerfile *string*; this
  stage supports `FROM microsandbox-agent` + `RUN` recipes, not arbitrary local-file `COPY`.
- **No layered-step build cache** (E2B's; a noted later enhancement). **Object storage stays
  `Local`** (the interface is the seam). **No auth.** **`default` cannot be built via the API**
  (it is the stock image baked into `vendor/` by `build-rootfs.sh`/`build-snapshot.sh`).

## 2. Target architecture (Stage 10 end state)

```
  build:  build_template(name, dockerfile, with_snapshot)
 SDK ─────────────────────────────────────▶ api ──gRPC TemplateService──▶ orchestrator
  │ POST /templates {name, dockerfile}       │ records a build row (SQLite)  │ TemplateCreate: mint bld_id,
  │  → {build_id}                            │  → {build_id}                 │  start a build goroutine, return
  │ poll: GET /templates/builds/{id}         │ TemplateBuildStatus           │  (pkg/build: docker build →
  └───────────────────────────────────────  │  → {state, detail}            │   build-rootfs.sh → build-snapshot.sh,
                                             │                               │   written in place via pkg/storage)
                                             ▼                               ▼  in-mem build registry: state / last log
                              (on SUCCESS) vendor/templates/<name>/{rootfs.ext4, snapshot/}
  then:  Sandbox(template=name) → the normal create path (UNCHANGED; name-resolved via pkg/template)
```

The orchestrator now serves **two** gRPC services on its one port — `SandboxService` (Stage 8)
and `TemplateService` (Stage 10) — plus its data proxy. A build is long (docker build + VM boot
+ kernel warm-up + a 512 MB snapshot write), which is exactly why `TemplateCreate` returns a
handle and the api polls.

## 3. Key design decisions

### Decision 1 — `TemplateService` lives in the orchestrator; build async, poll status

E2B keeps the template builder in the orchestrator (it needs the same docker + KVM +
firecracker the VM fleet needs). `services/proto/templatemanager/template-manager.proto`:

```proto
service TemplateService {
  rpc TemplateCreate(TemplateCreateRequest) returns (TemplateCreateResponse);
  rpc TemplateBuildStatus(TemplateBuildStatusRequest) returns (TemplateBuildStatusResponse);
}
message TemplateCreateRequest {
  string name          = 1;   // template name (validated like SandboxConfig.template; "default" rejected)
  string dockerfile    = 2;   // the recipe contents (FROM microsandbox-agent + RUN ...)
  bool   with_snapshot = 3;   // also build the warm snapshot (default true)
}
message TemplateCreateResponse { string build_id = 1; }   // "bld_..."; poll status with it
message TemplateBuildStatusRequest { string build_id = 1; }
message TemplateBuildStatusResponse {
  enum State { BUILDING = 0; SUCCESS = 1; FAILED = 2; }
  State  state  = 1;
  string detail = 2;          // last log line / error message
}
```

`TemplateCreate` mints a `bld_…` id, registers `buildID → {state, detail}` in an in-memory
`map` (guarded by a mutex, like the sandbox registry), starts a goroutine running `pkg/build`,
and returns the id; on completion the goroutine flips state to `SUCCESS`/`FAILED` with the last
log line. `TemplateBuildStatus` reads the registry (unknown id → `codes.NotFound`). Plaintext
gRPC on the same `grpc.Server` as `SandboxService`.

### Decision 2 — `pkg/storage` publishes **in place**, not under `{buildID}/` (a code-imposed constraint)

`fc.go` (`Restore`, ~line 153) and `scripts/build-snapshot.sh` show the snapshot references its
rootfs by the **absolute path baked in at build time** — `/snapshot/load` only overrides the
vsock uds (`vsock_override`), never the drive path. So a snapshot only restores if its rootfs is
still at the exact path it was built against. That rules out E2B's "build under `builds/{buildID}/`,
publish by pointer" model on one machine: a rename would invalidate the snapshot's baked path.

So artifacts are built **in place** at the canonical template dir:

```go
type StorageProvider interface {
    // TemplateDir is the directory holding a template's published artifacts
    // (rootfs.ext4 + snapshot/), i.e. where pkg/template.Resolve(name) looks.
    TemplateDir(name string) (string, error)   // "default" / invalid name -> error
}
type Local struct { root string }              // root = vendorDir
// Local.TemplateDir("foo") = {vendorDir}/templates/foo
```

The build writes rootfs + snapshot straight into `TemplateDir(name)`, so the snapshot bakes the
**final** rootfs path and `from_snapshot` works. `pkg/template.Resolve` is unchanged (it already
reads `vendor/templates/<name>/`). The `build_id` is purely the async-job handle.

`StorageProvider` is still the seam: a future object-storage impl would *materialize* artifacts
to a local path before boot (firecracker reads local files; the snapshot bakes a local path) —
which is exactly why a single machine's storage shape differs from E2B's buildID-keyed object
store. A good thing to understand, not a shortcut. (Atomicity — avoiding a half-built template
being seen mid-rebuild — is a `*.new` + `rename` refinement noted for the cleanup pass; the
snapshot still bakes the final `rootfs.ext4` path, so it's compatible.)

### Decision 3 — `pkg/build` wraps the existing scripts; the exec is injectable for tests

```go
type Builder struct {
    storage    storage.StorageProvider
    scriptsDir string  // the repo's scripts/ (new --scripts-dir orchestrator flag)
    run        func(name string, args ...string) (string, error)  // injectable for unit tests
}
func (b *Builder) Build(buildID, name, dockerfile string, withSnapshot bool) error
```

Steps: write `dockerfile` to a temp build context → `docker build -f <ctx>/Dockerfile -t
microsandbox-tmpl-<name> <ctx>` → `build-rootfs.sh <image> <templateDir>/rootfs.ext4` → if
`withSnapshot`, `build-snapshot.sh <templateDir>/rootfs.ext4 <templateDir>/snapshot`. The two
lower scripts self-locate `REPO_ROOT` via `BASH_SOURCE` (so they build the daemon, find
firecracker/vmlinux), so a `--scripts-dir` pointing at the repo's `scripts/` is enough. The
injectable `run` lets 10a unit-test the command sequence + the storage calls **without docker or
KVM**. (`build-template.sh` stays as the manual CLI; `pkg/build` is the programmatic equivalent,
both driving the same lower scripts.)

### Decision 4 — `api` REST `/templates` + a minimal SQLite `builds` table

`api` gains a `TemplateService` gRPC client on the same connection
(`pb.NewTemplateServiceClient(conn)`) and:

| Route | Does |
|---|---|
| `POST /templates` `{name, dockerfile, with_snapshot}` | gRPC `TemplateCreate`; insert `builds` row (state `building`); `201 {"build_id"}` |
| `GET /templates/builds/{id}` | gRPC `TemplateBuildStatus`; update the row; `200 {"state","detail"}` / `404` |
| `GET /templates` (optional) | list builds from the store |

`pkg/store` gains one table (mirroring E2B's api owning templates in Postgres):

```sql
CREATE TABLE IF NOT EXISTS builds (
  build_id   TEXT PRIMARY KEY,   -- "bld_..."
  name       TEXT NOT NULL,      -- template name
  state      TEXT NOT NULL,      -- building | success | failed
  detail     TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
```

### Decision 5 — SDK `build_template(...)`; recipe as a Dockerfile string

A module-level helper:

```python
build_template(name, dockerfile, *, with_snapshot=True,
               base_url=None, poll_interval=1.0, timeout=600.0) -> None
```

POST `/templates`, then poll `GET /templates/builds/{id}` until `success` (return) or `failed`
(`RuntimeError(detail)`) or `timeout`. The recipe is the Dockerfile **contents** (a real "submit
a recipe → server builds it async" API, not "rebuild a file already in the repo"); this stage
supports `FROM microsandbox-agent` + `RUN` recipes. Afterward, `Sandbox(template=name)` boots it
through the unchanged create path.

## 4. Code "from → to" map

| Now | Stage 10 |
|---|---|
| — (new) | `pkg/storage/` (`StorageProvider` + `Local` + tests) |
| — (new) | `pkg/build/` (`Builder` wrapping the scripts + injectable exec + tests) |
| — (new) | `proto/templatemanager/template-manager.proto`, `pkg/grpc/templatemanager/` (generated) |
| `cmd/orchestrator` (SandboxService only) | + `TemplateService` (async build + in-mem build registry) + `--scripts-dir` flag |
| `cmd/api` (/sandboxes only) | + `POST /templates`, `GET /templates/builds/{id}`, a `TemplateService` gRPC client |
| `pkg/store` (sandboxes table) | + `builds` table + CRUD |
| `client.py` (Sandbox only) | + `build_template(...)` |
| `scripts/gen-proto.sh` | also compiles `template-manager.proto` |
| `pkg/template.Resolve` / `SandboxConfig` / the create path / `daemon/` / `protocol.py` | **unchanged** |

## 5. Go layout introduced this stage

```
services/
  proto/templatemanager/template-manager.proto   # TemplateService
  pkg/
    storage/
      storage.go        # StorageProvider interface + Local
      storage_test.go
    build/
      build.go          # Builder: docker build -> build-rootfs.sh -> build-snapshot.sh, via storage
      build_test.go     # injected fake `run`; asserts the command sequence + storage calls
    grpc/templatemanager/   # generated *.pb.go
  cmd/
    orchestrator/templates.go   # TemplateService impl + the in-mem build registry
    api/templates.go            # the /templates REST handlers + gRPC client
```

## 6. Three independently verifiable sub-steps

### Stage 10a — `pkg/storage` + `pkg/build` (KVM-free unit tests; injected exec)
Add `pkg/storage` (`StorageProvider` + `Local` + tests: `TemplateDir` layout, `default`/invalid
rejection). Add `pkg/build` (`Builder`) with the injectable `run`, unit-tested by asserting the
exact command sequence (`docker build …`, `build-rootfs.sh …`, `build-snapshot.sh …` only when
`with_snapshot`) and that artifacts target the storage `TemplateDir`. Nothing is wired into the
running services yet. **Verify:** `go test ./services/...` green; existing services untouched.

### Stage 10b — `template-manager.proto` `TemplateService` in the orchestrator
Add the proto + codegen (extend `gen-proto.sh`). The orchestrator registers a second gRPC
service: `TemplateCreate` kicks the `pkg/build` goroutine and registers the build;
`TemplateBuildStatus` reads the in-memory registry. Add the `--scripts-dir` flag.
**Verify:** proto round-trip + async state-machine unit tests (with a fake builder) + full Python
e2e green (the sandbox path is untouched; the new gRPC service is present but unused by the api).

### Stage 10c — api REST `/templates` + SQLite `builds` table + SDK `build_template` + e2e
`api` serves `/templates`, persists `builds` rows, and proxies status. `pkg/store` gains the
`builds` table. The SDK gains `build_template(...)`. A new e2e builds the `example` recipe
through the API (`with_snapshot=false` to skip the 512 MB snapshot and cold-start the result),
polls to `success`, then `Sandbox(template=...)` and reads the marker file. **Verify:** full e2e
green (+ the new build test). Then update `CLAUDE.md`, `docs/ARCHITECTURE.md`, the README, and
mark Stage 10 done in `docs/E2B_ALIGNMENT_ROADMAP.md`.

## 7. Keeping tests green (honest trade-offs)

- **10a stands the libraries up and unit-tests them with a fake exec before any build runs**;
  10b adds the gRPC service (sandbox path untouched → e2e stays green); 10c wires the api + SDK
  and proves the whole path with a real build. Same find-it/prove-it/clean-it cadence as Stages
  8–9 — tests green at every step.
- **New dependencies:** only the gRPC codegen re-run for `template-manager.proto`; `pkg/build`
  shells out (like the rest of the host shells out to docker/firecracker), so no new Go library.
- **In-place build window:** during a rebuild `vendor/templates/<name>/` is briefly incomplete;
  on one machine with infrequent builds this is acceptable, with `*.new` + `rename` as the
  cleanup-pass refinement. E2B avoids it via buildID isolation + an atomic catalog flip; ours is
  a single-machine simplification, forced by the snapshot's baked rootfs path (Decision 2).
- **Builds are slow** (docker build + boot + kernel warm-up + snapshot) — which is *why* the API
  is async + polled. The new e2e uses `with_snapshot=false` to bound its runtime.
- **Safety note carried forward:** still a learning implementation, not security-audited. A
  template build runs `docker build` on a user-supplied Dockerfile on the host — fine for local
  learning, **not** safe to expose to untrusted input; docs must keep saying so.
```