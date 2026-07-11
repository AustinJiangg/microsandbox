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
> Decision D1)**, **13 (UFFD lazy snapshot restore behind `--uffd`; `File` stays the
> default)**, **14 (the storage swaps go live: catalog → Redis, store → Postgres)**, and **15 (the
> last storage swap: `Local → object storage`, MinIO/S3 — rootfs/snapfile materialized, memfile
> streamed over UFFD), **16 (the first production-fidelity stage: auth — `X-API-Key`→team,
> team-scoped resources, and a per-sandbox data-plane access token)**, and **17 (the first
> storage-mechanism-depth item: the streamed memfile stored compacted behind a per-block
> `pkg/storage/header` index — zero/gap pages served without a fetch)** are **done** — see their design
> docs and the "Done" list in `CLAUDE.md`. **Stage 18** (storage depth (2): COW **layered rootfs** builds — the
> header's per-entry build owner + `MergeMappings`, a rootfs diff over a base, assembled at boot) is also done,
> and **Stage 19** closed its one gap (**block-layout preservation**): a layered child is built by mutating a
> **copy of the base's rootfs in place** (`debugfs`) instead of re-mkfs, so the `derived` diff dropped from
> 278.8 MiB (2.07×) to **28 KiB (0.0047%)** — ~the genuine delta.
> The remainder (production fidelity — multi-host / a TS SDK; plus the rest of the storage-mechanism depth —
> NBD-served rootfs over the same header, **memfile COW** via live-VM re-snapshot (**Stage 20**), a cross-node
> cache; and auth depth — a key-management API, token expiry/rotation, TLS) is the **deferred** forward plan.
> (Note: memfile/rootfs **compression** **is** an optional E2B mechanism — Stage 20 research (`e2b-dev/infra` @ main)
> found V4/V5 headers with zstd/lz4 in 2 MiB frames, raw V3 still supported, orthogonal to COW — but we still store
> raw, so it stays deferred optional depth, not a required fidelity gap. See `docs/STAGE20_DESIGN.md` §2.)

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
   store: Postgres (pkg/store; SQLite selectable)  ·  catalog: Redis (pkg/catalog)  ·  artifacts: local dir (pkg/storage)
        store + catalog swapped to Postgres / Redis in Stage 14; artifacts → object storage is Stage 15
```

Component → where it lives in this repo:

| E2B component | This repo | Notes |
|---|---|---|
| `api` | `services/cmd/api` | REST lifecycle + templates; persists to `pkg/store`; gRPC client to orchestrator; trivial single-node placement |
| `orchestrator` | `services/cmd/orchestrator` | gRPC `SandboxService`+`TemplateService`; owns `pkg/fc`, `pkg/pool`; hosts the per-node data proxy |
| template builder | inside orchestrator (`pkg/build` + `TemplateService`) | wraps today's `scripts/build-*.sh`; async build + status polling, exactly like E2B |
| `client-proxy` | `services/cmd/client-proxy` | edge data proxy; **header-mode** routing (no `<port>-<id>` DNS/TLS on one box) |
| `envd` | `daemon/` | **unchanged this round** — still the Stage-7 Go daemon (HTTP `/health /execute /files/* /commands` over vsock) |
| Postgres | `pkg/store` (Postgres via `jackc/pgx`, default; SQLite via `modernc.org/sqlite` selectable; Stage 14b) | both drivers pure Go (static binary); a `Store` interface, not `sqlc` — plain database/sql |
| Redis catalog | `pkg/catalog` (Redis via `go-redis`; Stage 14a) | mirrors E2B's `sandbox-catalog`; the api writes, client-proxy reads |
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
- **Stage 14 — the storage swaps go live (catalog → Redis, store → Postgres).** ✅ `pkg/catalog`
  gained a `Redis` impl (the api `SET`s the route on create, client-proxy `GET`s it on each data
  request), which let the api→client-proxy internal control RPC be **deleted**; `pkg/store` became
  an interface with `sqlite` + `postgres` (pure-Go `jackc/pgx`) impls, `Open(dsn)` dispatching by
  scheme, defaulting to Postgres. A repo-root `docker-compose.yml` provisions both. On one box this
  is fidelity (a cross-process catalog, a concurrent restart-surviving store, the multi-host
  precondition), **not** speed; e2e 37/37 on Postgres + Redis. Object storage was split out to
  Stage 15. See `docs/STAGE14_DESIGN.md`.

- **Stage 15 — `Local → object storage` (the last storage swap).** ✅ Template artifacts live in
  **S3** (MinIO locally; pure-Go `minio-go`), keyed by an immutable `{buildID}/…` prefix + an
  `aliases/<name>` pointer. The Stage-13 UFFD page source became **pluggable** (`uffd.PageSource`:
  `MmapSource` + a chunked `bucketSource`); the orchestrator **materializes** rootfs/snapfile to their
  baked local paths and **streams the memfile** page-by-page from the bucket over UFFD (the Stage-13
  payoff). `--storage s3` is the default; `local-fs` is the escape hatch. Honest: not a single-box
  speedup (per-page would blow the health timeout, so reads are chunked); the win is the seam + an
  end-to-end-pluggable page source. e2e 37/37 in s3 mode. See `docs/STAGE15_DESIGN.md` (its §11
  itemizes the deferred mechanism depth, verified against `e2b-dev/infra`).

### Stage 16 — auth (`X-API-Key`→team, team-scoped resources, a data-plane access token) ✅
The first production-fidelity stage; gives the system **identity**. The api authenticates every
request (except `/health`) with an `X-API-Key` resolving to a **team** (keys stored hashed in
`pkg/store`'s new `teams`/`api_keys` tables, seeded by `--seed-api-keys`); sandboxes/builds are
**team-owned** so list/delete/build-status are team-scoped (another team's id is 404, not 403).
The data plane gets a **per-sandbox access token** minted by the api at create, carried in the
catalog (`catalog.Route{Node, Token}`) and returned to the SDK; client-proxy gates the in-VM
**control ports** (envd `:49983`, code-interpreter `:49999`) on `X-Access-Token` while user-exposed
ports stay public. **Honest scope:** keys seeded by flag (no key-management API), token bearer-only
(no expiry/rotation/signing), plaintext on loopback — the auth *seam*, not a hardened gateway; still
not security-audited. e2e **43/43** (37 prior + 6 auth). **Detail: `docs/STAGE16_DESIGN.md`.**

### Stage 17 — storage depth (1): a `.header`-indexed, compacted memfile ✅
The first of the deferred storage-mechanism-depth items, and the first storage stage that **stores and
fetches strictly less** rather than only adding fidelity. Added `pkg/storage/header` (mirroring E2B's
`pkg/storage/header`): a `Metadata` + ordered `Mapping` of present (non-zero) runs; the builder/seeder
**compact** the stored memfile (only non-zero blocks, via `storage.PublishMemfile`) and write a
`{buildID}/memfile.header`, and the boot path resolves each faulting logical offset through the mapping —
serving zero/gap pages with **no fetch** (`uffd.NewMappedSource`, storage-free `Extent`), else falling
back to the Stage-15 full-object stream for an unindexed bucket. Single-build simplification: E2B's
per-entry `BuildId` (the COW owner) and chunk compression are deferred. **Measured:** the 512.0 MiB
default memfile → 228.6 MiB compacted (2.2×), 6,560-byte header; e2e 43/43 in s3 mode; no isolated
single-box latency win (net setup dominates). See `docs/STAGE17_DESIGN.md`.

### Stage 18 — storage depth (2): COW layered rootfs builds ✅
E2B's **copy-on-write layering**, banked on the rootfs. `pkg/storage/header` gained the COW algebra (a per-entry
**build owner** as a header-local build-table index — zero new deps; `MergeMappings`/`NormalizeMappings`/`BuildDiff`/
`Locate`; format v2 alongside the v1 memfile); `pkg/storage` got `PublishRootfsDiff` (store only the child's changed
blocks + `{B}/rootfs.ext4.header`) and `MaterializeLayered` (assemble the full rootfs from each run's owning build,
whole-object fallback when there is no header); the build/boot/API/SDK path carries an optional `base`/`from`.
**Honest:** the mechanism is faithful and boots a real layered VM (e2e 44/44), but the size win is **bounded (~2.07×,
576 → 278.8 MiB), not the lab ~40×** — `docker build … RUN` + `docker export | mkfs.ext4 -d` reshuffles the ext4
block layout when a layer is added (a same-image re-mkfs differs only ~3%; a `RUN`-layer child differs ~48%), so the
size-pin (Decision 8) is necessary but **not sufficient**: **block-layout preservation** (E2B mutates a persisted
block device in place) is the missing piece. See `docs/STAGE18_DESIGN.md`.

### Stage 19 — storage depth (3): layout-preserving layered rootfs (the COW payoff) ✅
Closes Stage 18's one gap — **block-layout preservation** — so the COW machinery actually pays off. A layered
child's rootfs is no longer re-`mkfs.ext4 -d`'d from a fresh `docker export` (which reshuffles the ext4 layout when
a layer is added); instead `scripts/build-rootfs-layered.sh` **copies the base template's rootfs image and applies
only the child's file delta in place via `debugfs`** (unprivileged, no mount/loop — the single-box analogue of
E2B's in-place block-device layer). `pkg/build` wires it for `base`-set builds (materialize the base → layered
builder → unchanged `PublishRootfsDiff`), retiring the Stage-18 size-pin. Header / merge / `MaterializeLayered` /
boot / API / SDK all **unchanged**. **Measured:** the same `derived` (default + one `RUN`) now stores a **28 KiB**
rootfs diff over the 576 MiB base (**0.0047%**, vs Stage 18's 278.8 MiB / 48% — ~10,000× smaller), asserted in the
e2e via a Go probe (`msb-rootfs-stat`). e2e **44/44** in s3 mode. See `docs/STAGE19_DESIGN.md`.

### Stage 22 — E2B's layered-snapshot producer (in-guest command → one re-snapshot → two COW diffs) ✅
The live-VM memfile producer now matches E2B: a layered build with a snapshot resumes the base over a
writable rootfs overlay, runs the layer's command **in the guest** (envd `Run`), `sync`s, and takes one Full
re-snapshot from which both the memfile COW diff (`BuildDiff` on the dump) and the rootfs COW diff (the
overlay's dirtied blocks, `ExportToDiff`) are derived — the child's RAM and disk are one consistent state.
**The long blocker was a Firecracker regression, not our code:** re-snapshotting a UFFD-restored VM's
writable virtio devices yields an inconsistent `(memfile, vmstate)` pair that **v1.16.0** rejects on restore.
E2B runs plain upstream **v1.10.1** (its `v1.10.1_1fcdaec08` = `<tag>_<commit>`, no patch), which lacks the
regression; **pinning `vendor/firecracker` to v1.10.1 closes it** (+ UFFD `page_size_kib` compat, a
layered-child stale-vmstate refresh, `io_engine Async` + load-paused-then-resume to match E2B). Real-VM e2e
**45/45** in `--nbd` s3 mode. See `docs/STAGE22_DESIGN.md` (§16 resolution; §13–15 the investigation).

### Stage 23 — multi-host scheduling: a node registry + `placement.BestOfK` ✅
The first "production fidelity across hosts" item: the api stops assuming **one** orchestrator and holds a
**fleet**, picking a node per create with E2B's **power-of-K-choices** placement. **api-side only** — the data
path was already multi-host since Stage 14a (the catalog stores a per-sandbox `Route{Node}`), so no proto /
data-path / `envd` / rootfs change. `services/pkg/placement` (`Node` + `BestOfK` + `Registry`) specializes
E2B's CPU-weighted `Score` to our homogeneous 1-vCPU sandboxes → `(inProgress + cachedCount) / capacity`,
with `cachedCount = len(List())` refreshed by a ~1s background poll (E2B's `Metrics()`) and `inProgress` a
per-node reserve counter (E2B's `InProgress()`) — so the existing `SandboxService.List` is the load signal and
**no metrics RPC / proto edit** was needed. The api takes a static `--nodes grpc@proxy,…` flag (empty → the
single legacy node, backward-compatible); `handleCreate` picks + routes Create/catalog to the chosen node,
`handleDestroy` routes Delete to the holding node; **failover** excludes a node-fault Create error and retries
(request-fault `InvalidArgument` returned immediately → 400 preserved, single-node `Internal` → 500 preserved),
plus **in-progress load balancing**. **Honest scope:** fidelity, not speed; node discovery is a static flag
(Nomad deferred); the multi-node behavior is verified by an in-process fake-orchestrator integration test
(deterministic spread, failover), **not** two real orchestrators (which would test single-box resource
partitioning, not E2B scheduling). Go units green (incl. `-race`); real-VM single-node lifecycle e2e **13/13**.
See `docs/STAGE23_DESIGN.md`.

### Stage 24 — real (dynamic) node discovery: a `Discovery` source + a reconciling registry ✅
Stage 23's fleet was **static** (a `--nodes` flag parsed once into a fixed slice); this stage makes it
**dynamic**, mirroring E2B's `discovery.Discovery` interface + `keepInSync` reconcile loop
(`packages/api/internal/orchestrator`). **api-side + orchestrator-startup only** — no proto / data-path /
`envd` / rootfs change. `pkg/placement.Registry` becomes a **reconciled map** (was a fixed slice): a `Discovery`
interface (`ListNodes`) + an api-injected `NodeFactory` (dials gRPC, keeping the package dial-free), a
`reconcile()` that adds discovered-absent nodes and evicts present-undiscovered ones (closing their conn), and a
`StaticDiscovery` wrapping `--nodes` (the static path is now just one `Discovery` impl, identical to Stage 23).
The **real dynamic backend** is a **Redis service registry**: the orchestrator's `Registrar` heartbeats
`msb:node:<id> → {grpc,proxy}` with a TTL (`SET … EX 3s` ~1s, `DEL` on graceful stop) and `RedisDiscovery`
`SCAN`s them, so a crashed node's key **TTL-expires** out (Consul/Nomad service-registration analogue; TTL is
the health signal, no metrics RPC). Wired via orchestrator `--register`/`--redis-addr` + api
`--node-discovery static|redis` (default static, backward-compatible) + a `GET /nodes` fleet-view endpoint.
**Honest scope:** genuinely dynamic on one box (join by registering, leave by dying — no api restart, fully
observable), but the backend is Redis-TTL not Nomad/Consul (swapping one in is another `Discovery` impl), and
rebalancing / per-node build placement / graceful drain stay deferred. Go units green (incl. `-race` + live-Redis
round-trip & TTL tests); real-VM dynamic-discovery e2e **2/2** (boot-via-discovery + TTL eviction), static
lifecycle **13/13** (no regression), `test_microvm` **6/6** in discovery mode. See `docs/STAGE24_DESIGN.md`.

### Still deferred
- **More storage-mechanism depth (deeper E2B fidelity behind the same seam).** Verified against
  `e2b-dev/infra`, and building on the Stage-17/18/19 `pkg/storage/header` + COW algebra: E2B serves the **rootfs
  lazily over a userspace NBD block device** (not materialized/assembled whole, as we do — over the *same* layered
  header), layers the **memfile** too via live-VM re-snapshot (**Stage 20** — a build-time memfile diff is
  meaningless: two independent boot snapshots differ everywhere), and shares chunks via a **cross-node cache**. Each
  deepens the *mechanism* behind the `StorageProvider` / `PageSource` / `header` interfaces without changing the seam
  (`docs/STAGE15_DESIGN.md` §11, `docs/STAGE17_DESIGN.md` §10, `docs/STAGE18_DESIGN.md` §10–11). (**Block-layout
  preservation** for the rootfs diff — the gap that capped Stage 18's size win — was **done in Stage 19**.)
  > **Correction (superseded by Stage 20 research):** an earlier Stage-18 audit called E2B's "chunked +
  > **compressed**" storage a myth and concluded E2B stores only raw blocks. That read a partial tree. Current
  > `e2b-dev/infra` @ main **does** optionally compress — V4/V5 header formats store 2 MiB frames, optionally
  > zstd/lz4 (`shared/pkg/storage/compress_encode.go`, per-build `FrameTable`), flag-gated, with raw V3 still
  > supported. Compression is **orthogonal to COW** and off its critical path; we still store raw (our v1/v2
  > headers ≈ E2B's V3), so it is **deferred optional E2B depth**, not "not-E2B." See `docs/STAGE20_DESIGN.md` §2.
- **Later — production fidelity.** Auth landed in Stage 16; multi-host **placement** landed in Stage 23
  (`placement.BestOfK`) and **real (dynamic) node discovery** in Stage 24 (a Redis service registry behind
  E2B's `Discovery` seam — orchestrators self-register with a heartbeat/TTL, the api reconciles). What remains:
  **rebalancing** already-placed sandboxes off a joined/draining node, **graceful drain** (a node saying "no
  new placements" vs "gone"), **per-node build placement** (template builds still route to one designated
  node), a real Nomad/Consul `Discovery` impl, a TypeScript SDK, per-template resource limits and start/ready
  commands, plus auth depth (a key-management API, token expiry/rotation, TLS).

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
