# E2B alignment roadmap

> Status: **agreed direction.** This document supersedes the "what's next" bullet list
> in `CLAUDE.md` for the post–Stage 7 work. It reframes the project's goal from
> *"climb the isolation ladder"* (done — we reached Firecracker microVMs in Stages 0–7)
> to *"decompose the monolithic control plane into E2B's real component architecture,"*
> in one repo, on one machine. Read `docs/ARCHITECTURE.md` (the current layers) first.
>
> The per-stage detail lives in its own design doc (`docs/STAGE8_DESIGN.md`, …) as
> each stage is picked up; this file is the map that ties them together.
>
> **Progress:** Stages 8 (api + orchestrator gRPC split), 9 (client-proxy + routing catalog),
> 10 (TemplateService + `pkg/storage`), 11 (`envd` → ConnectRPC + a separate
> `code-interpreter` — which **ended the byte-stable-protocol discipline**; the e2e oracle is
> now behavioral), and **12 (per-sandbox TAP/netns networking; the data path flipped vsock →
> TCP routed by real `<port>-<id>` hostnames; user-port exposure; vsock retired — reversing
> Decision D1)**, and **13 (UFFD lazy snapshot restore behind `--uffd`; `File` stays the
> default)** are **done** — see their design docs and the "Done" list in `CLAUDE.md`. The
> remainder (the storage swaps going live, then auth / multi-host / a TS SDK) is the
> **deferred** forward plan.

## 1. Why this document

Stages 0–7 were about *isolation strength*: subprocess → Docker → resident container →
Firecracker microVM, then rewriting the in-VM daemon in Go (`envd`, Stage 7). The
isolation story is now where we want it. What remains un-E2B-like is the **shape of the
host side**: today a single Go binary (`control-plane/`, ~970 LoC, one `package main`)
fuses everything E2B splits across services — the public API, the per-node VM manager,
the template builder, the warm pool, and the data-path proxy.

E2B's design value is in those **seams**: a REST API that only does lifecycle, a gRPC
boundary to per-node orchestrators, a template builder that runs as an async job, a
two-hop proxy that routes data-plane traffic to the right sandbox. This roadmap pulls
our monolith apart along exactly those seams. We keep one repo and one machine (so we
defer the things that only matter across hosts — auth, multi-node scheduling, a second
SDK), but we mirror E2B's component boundaries faithfully, because the boundaries are
the lesson.

## 2. What E2B actually is (verified against `e2b-dev/infra` @ main)

A condensed, source-verified map. **Three of these correct assumptions previously
written in `CLAUDE.md`** and are called out inline.

| Component | Language / form | Responsibility | API it exposes |
|---|---|---|---|
| **`api`** | Go (gin), REST from `spec/openapi.yml` | public control plane; auth (API-key→team), pick a node, persist metadata | REST: `POST/GET/DELETE /sandboxes`, `…/pause`, `…/resume`, `POST /v3/templates`, build-status… |
| **`orchestrator`** | Go, one **per node** (`raw_exec`, host-level for KVM) | Firecracker lifecycle, snapshots, sandbox networking, **+ the template builder + per-node proxy** | gRPC `SandboxService` (Create/Delete/List/Pause/Checkpoint…), `TemplateService`, `ChunkService` |
| **template builder** | Go, **inside `orchestrator`** | build a template image → rootfs+memfile+snapfile | gRPC `TemplateService` (TemplateCreate/TemplateBuildStatus/…) — **❶ not a separate `template-manager` dir as CLAUDE.md said** |
| **`client-proxy`** | Go (edge) | route user traffic to the right sandbox on the right node | HTTP; parses `<port>-<sandboxID>.host`, looks up Redis catalog, reverse-proxies to the node's proxy `:5007` → `envd :49983` |
| **`envd`** | Go, in-VM, `0.0.0.0:49983` | run **OS processes** and **filesystem** ops | **ConnectRPC** `Process` + `Filesystem` services (+ `/files` multipart, `/health`) — **❷ no Jupyter; `run_code()` is a separate `code-interpreter` service on `:49999`** |
| store | **Postgres + `sqlc`** (Supabase on GCP) | teams, API keys, templates+builds, paused-sandbox snapshots | — **❸ `sqlc`, not `ent`** |
| catalog | **Redis** | `sandbox:catalog:<id>` → orchestrator IP, for proxy routing + resume | — |
| artifacts | object storage (**GCS** default; S3/Local) | per-build `{buildID}/rootfs.ext4`,`memfile`,`snapfile`,`metadata.json` | `StorageProvider` interface |
| scheduling / discovery | **Nomad** (jobs) + **Consul** (service DNS); api discovers the orchestrator fleet via the Nomad node list | — | — |

**End-to-end lifecycle:** SDK `POST /sandboxes` → `api` (auth, `placement.BestOfK` over
the node list) → gRPC `SandboxService.Create` on the chosen node → `orchestrator` boots
Firecracker (snapshot+UFFD lazy restore), allocates a TAP/netns network slot → `api`
writes the Redis catalog row → SDK connects to `https://49983-<id>.e2b.app` (ConnectRPC),
which the cloud LB → `client-proxy` (catalog lookup) → node proxy `:5007` → `envd :49983`
routes into the VM.

## 3. Target architecture in this repo

One repo, one machine. The decomposition was done **vsock-first** (Decision D1) and then
**Stage 12 gave every sandbox a real NIC and flipped the data path to TCP** (see below). We
reproduce E2B's *seams* with single-machine-appropriate implementations behind E2B-shaped
interfaces:

```
                       REST  (lifecycle + templates)
  Python SDK ───────────────────────────────────────►  api  ─────┐
   (src/microsandbox)                              (REST; trivial  │ gRPC
        │                                            placement;    │ SandboxService / TemplateService
        │ data plane (execute / files / commands,    persists      ▼
        │  ConnectRPC, Host: <port>-<id>)            SQLite)   orchestrator  (one per machine)
        └───────────────────►  client-proxy ──(catalog)──►        • Firecracker lifecycle  (pkg/fc)
                               (edge; parses             ──┐       • warm pool             (pkg/pool)
                                Host <port>-<id>)           │       • per-sandbox net slot  (pkg/network)
                                                            │       • TemplateService        (build pipeline)
                                                            │       • per-node data proxy ──TCP→VM NIC──► envd :49983
                                                            │                                       (daemon/, Stage 12)
   store: SQLite (pkg/store)  ·  catalog: in-mem (pkg/catalog)  ·  artifacts: local dir (pkg/storage)
        all three behind E2B-shaped interfaces → swappable to Postgres / Redis / object storage later
```

Component → where it lives in this repo:

| E2B component | This repo | Notes |
|---|---|---|
| `api` | `services/cmd/api` | REST lifecycle + templates; persists to `pkg/store`; gRPC client to orchestrator; trivial single-node placement |
| `orchestrator` | `services/cmd/orchestrator` | gRPC `SandboxService`+`TemplateService`; owns `pkg/fc`, `pkg/pool`; hosts the per-node data proxy |
| template builder | inside orchestrator (`pkg/build` + `TemplateService`) | wraps today's `scripts/build-*.sh`; async build + status polling, exactly like E2B |
| `client-proxy` | `services/cmd/client-proxy` | edge data proxy; **header-mode** routing (no `<port>-<id>` DNS/TLS on one box) |
| `envd` | `daemon/` | **unchanged this round** — still the Stage-7 Go daemon (HTTP `/health /execute /files/* /commands` over vsock) |
| Postgres+sqlc | `pkg/store` (SQLite via `modernc.org/sqlite` + `sqlc`) | pure-Go driver keeps the static-binary story; interface mirrors E2B |
| Redis catalog | `pkg/catalog` (in-memory) | interface mirrors E2B's `sandbox-catalog` |
| object storage | `pkg/storage` (`Local`) | `StorageProvider` interface; `{buildID}/…` layout |
| Nomad/Consul | — | deferred (single machine); api holds one orchestrator address by flag |

## 4. Decisions that scope this work

Three forks were put to the user; all three took the lower-risk, staged option:

- **D1 — Networking: vsock first, TAP later.** ✅ **Done in Stage 12.** The whole
  decomposition was done over the existing vsock transport (everything kept working, every
  step verifiable), and per-sandbox TAP/netns networking + real `<port>-<sandboxID>` port
  exposure became its own later stage. The proxy *topology* (two hops, a catalog) was
  E2B-faithful from Stage 9; **Stage 12 swapped the *transport* it rides (vsock → TCP over a
  per-sandbox TAP/netns NIC) and retired vsock.**
- **D2 — `envd` untouched this round.** E2B's `envd` is `Process`+`Filesystem` ConnectRPC
  with `run_code()` split into a separate `code-interpreter`. We keep the Stage-7 merged
  HTTP daemon for now and focus the refactor on the control-plane seams the user named;
  the `envd` rewrite is a later stage.
- **D3 — Lightweight, isomorphic storage.** SQLite + in-memory catalog + local-dir
  artifacts, each behind an interface named and shaped after E2B's, so the swap to
  Postgres / Redis / object storage is a one-implementation change, not a redesign.

Cross-cutting engineering decisions (locked here so they aren't re-litigated per stage):

- **Module layout.** The host side becomes one Go module, `microsandbox/services`,
  rooted at `services/` (keeps the repo root tidy and groups the host services like
  E2B's `packages/`). It holds `cmd/{api,orchestrator,client-proxy}`, `pkg/{…}`, and
  `proto/`. `daemon/` stays its own module (`microsandbox/daemon`, untouched). The old
  `control-plane/` module is **dissolved** — its files move into `pkg/` and
  `cmd/orchestrator` — and deleted. A repo-root `go.work` (`use (./services ./daemon)`)
  ties them together for editor/`go build ./...` ergonomics; each module still builds
  standalone.
- **gRPC tooling.** Protobuf with `protoc-gen-go` + `protoc-gen-go-grpc`, pinned as Go
  tool dependencies (`go tool`, Go 1.24+) so `go generate ./services/proto` is
  reproducible without a system protoc install ritual. Generated Go lands in
  `services/pkg/grpc/<svc>` (mirroring E2B's `packages/shared/pkg/grpc`).
- **SQLite tooling.** `modernc.org/sqlite` (cgo-free) + `sqlc generate` from
  `services/pkg/store/{schema.sql,queries.sql}` → typed Go. Keeps every binary static.
- **The wire protocol (`protocol.py`) and `envd`'s routes stay byte-stable.** The
  data-plane request/response bytes do not change; only *who proxies them* moves
  (api → client-proxy). This is the same discipline every prior stage used.

## 5. The staged roadmap

Each stage is one Conventional-Commit-sized unit, split into independently verifiable
sub-steps, tests green at every step (the Python e2e suite is the parity oracle, exactly
as in Stage 7).

### Stage 8 — split `control-plane` into `api` (REST) + `orchestrator` (gRPC), add a metadata store
Stand up E2B's #1 seam. `orchestrator` becomes a gRPC `SandboxService` wrapping the
existing Firecracker lifecycle + warm pool, and also hosts the per-node data proxy
(the vsock bridge). `api` becomes the REST front that persists sandbox/template rows to
SQLite and calls the orchestrator over gRPC; for now it temporarily reverse-proxies the
data path to the orchestrator's proxy so the SDK keeps one base URL. `control-plane/` is
dissolved into `pkg/` + `cmd/`. **Detail: `docs/STAGE8_DESIGN.md`.**

### Stage 9 — `client-proxy` + sandbox `catalog`; sink the data plane off `api`
Introduce `pkg/catalog` (in-memory, E2B-shaped) — `api` writes `sandbox→node` on Create.
Stand up `client-proxy` as the edge data proxy: read `X-Sandbox-Id`, look up the catalog,
reverse-proxy to that node's orchestrator proxy → vsock → `envd`. The SDK splits its data
calls to `client-proxy`; `api`'s temporary passthrough is removed, leaving `api`
lifecycle-only (E2B-faithful). Header-mode routing mirrors E2B's fallback for hosts
without `<port>-<id>` DNS.

### Stage 10 — `TemplateService` (the template builder) inside `orchestrator` + `pkg/storage`
`pkg/storage` `StorageProvider` (+ `Local`), artifacts re-laid-out under `{buildID}/`.
`template-manager.proto` `TemplateService` in the orchestrator: `TemplateCreate` kicks an
**async** build goroutine (wrapping `docker build → build-rootfs → build-snapshot`) and
returns immediately; the api polls `TemplateBuildStatus` — E2B's exact "accept sync,
build async, poll status" model. The SDK gains a template-build API. (Single-recipe
build now; E2B's layered-step cache is a noted later enhancement.)

### Done after the decomposition (were deferred)
- **Stage 11 — `envd` → E2B form.** ✅ Rewrote the daemon as ConnectRPC `Process` +
  `Filesystem`; moved Jupyter/`run_code()` into a separate `code-interpreter` service on
  its own port; SDK gained Connect clients. (Reversed D2.)
- **Stage 12 — TAP/netns networking.** ✅ Per-sandbox TAP + network namespace; `envd` on
  TCP `:49983` + code-interpreter on `:49999`; `client-proxy` routes by real
  `<port>-<sandboxID>` hostnames; user-port exposure (`sandbox.get_host(port)`); vsock
  retired. (Reversed D1.) **Scope note:** UFFD lazy restore and the storage swaps the
  roadmap had grouped here were *deferred to their own later stages* — Stage 12 was
  networking-only (see `docs/STAGE12_DESIGN.md`).
- **Stage 13 — UFFD lazy snapshot restore.** ✅ `services/pkg/uffd` page-fault handler serves
  guest RAM from the memfile over `userfaultfd`; wired into `fc.Restore` behind `--uffd`
  (default off). Measured no single-box speedup (restore ~0.54s UFFD vs ~0.57s File, within
  noise; warm pool ~11–25ms either way; e2e 37/37 on both backends), so `File` stays the
  default — the win is the mechanism + a now-pluggable page source. See `docs/STAGE13_DESIGN.md`.

### Still deferred
- **The storage swaps go live:** SQLite→Postgres, in-mem→Redis, Local→object storage. UFFD
  (Stage 13) already made the memfile page source pluggable — the precondition for sourcing
  snapshot memory from object storage / a peer node rather than a local file.
- **Later — production fidelity.** Auth (`X-API-Key`→team), multi-host scheduling (real
  node discovery + `placement.BestOfK`), a TypeScript SDK, per-template resource limits
  and start/ready commands.

## 6. Repo layout after Stage 10

```
microsandbox/
  go.work                      # use (./services ./daemon)
  services/                    # NEW host module: microsandbox/services
    go.mod
    proto/
      orchestrator/orchestrator.proto        # SandboxService
      templatemanager/template-manager.proto # TemplateService
    cmd/
      api/main.go              # REST lifecycle + templates; placement; persist; gRPC client
      orchestrator/main.go     # gRPC SandboxService+TemplateService; FC; pool; data proxy
      client-proxy/main.go     # edge data proxy; catalog → orchestrator proxy
    pkg/
      fc/        # Firecracker lifecycle      (← control-plane/microvm.go)
      pool/      # warm pool                  (← control-plane/pool.go)
      proxy/     # vsock bridge + host parse   (← control-plane/proxy.go)
      template/  # name→artifacts resolution   (← control-plane/template.go)
      store/     # SQLite + sqlc
      catalog/   # sandbox→node (in-mem)
      storage/   # StorageProvider (Local)
      build/     # template build pipeline
      grpc/      # generated gRPC stubs
  daemon/                      # envd (UNCHANGED this round)
  src/microsandbox/            # Python SDK (client.py talks to api + client-proxy)
  scripts/                     # build-{services,rootfs,snapshot,template}.sh + dev-up.sh
  vendor/                      # firecracker / vmlinux / rootfs / snapshot / built binaries
  docs/                        # this file + per-stage design docs
```

The Python SDK stays at `src/microsandbox/` (no packaging churn); only its base URLs and
a templates API change across the stages.

## 7. Keeping tests green & the working cadence

- **The Python e2e suite is the oracle**, as in Stage 7. `tests/conftest.py` currently
  builds+runs one `vendor/control-plane`; it evolves to build+run the trio
  (`api` + `orchestrator`, then `client-proxy`) via a session fixture / `dev-up.sh`. The
  byte-stable protocol means a green suite is proof the decomposition changed *only*
  topology, not behavior.
- **Go units stay KVM-free**: proto round-trips, `pkg/store`, `pkg/catalog`, `pkg/pool`,
  `pkg/proxy` host-parsing — all testable without a VM, mirroring today's
  `go test ./control-plane`.
- **New dependencies are called out per stage** (gRPC codegen + runtime, `sqlc` +
  `modernc.org/sqlite`) so the move off "stdlib-only host side" is a conscious decision,
  not drift.
- **Cadence (unchanged):** split each stage into independently verifiable sub-steps,
  keep tests green at every step, give an honest 🔴/🟡/🟢 self-review before committing,
  commit only on the user's explicit go-ahead (one single-line Conventional Commit per
  stage), and push to `origin/main` immediately after.
- **Safety note carried forward:** this remains a learning implementation, not
  security-audited. Stage 12 added networking and so **narrowed the old "fully offline"
  property** — each sandbox is now **inbound-reachable, outbound-denied by default** (DNAT,
  no MASQUERADE). The docs no longer say "fully offline" and must keep saying it is not safe
  to expose to untrusted input.
