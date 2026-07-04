# CLAUDE.md

> This file is **Claude Code's project memory**. It is loaded automatically at the
> start of every session in this repo. Keep the project's long-term conventions,
> architectural decisions, and current state here — not scattered across chats.

## What this project is

`microsandbox` is a **learning-oriented** code-execution sandbox modeled on
[E2B](https://github.com/e2b-dev/E2B). The point is to **understand the
principles**, not to ship a product. The code aims to be clear, well-commented,
and easy to evolve.

Every sandbox is a **Firecracker microVM**: its own guest kernel behind the KVM
boundary, a per-sandbox **TAP/netns NIC** carrying the data path over **TCP** (Stage 12
retired the original vsock channel), and a stateful Jupyter kernel inside the VM. The project was originally built up in stages (host subprocess → Docker
container → resident container → microVM) to learn each isolation technique; those
earlier backends were scaffolding and have since been removed, leaving only the
Firecracker path. **The staged journey is preserved in the git history** — see the
`archive/stages-0-3` tag (not the current tree) for how the earlier stages worked.

## Core architecture (keep it stable)

The core layers — see `docs/ARCHITECTURE.md` for the full design:

1. **client (SDK)** — `src/microsandbox/client.py`. What the user faces:
   `Sandbox().run_code(...)`. A thin **pure-HTTP** client: it drives the **api**
   (`POST`/`DELETE /sandboxes`) and runs code through it (`/sandboxes/{id}/...`);
   it holds no vsock code anymore.
2. **protocol (client↔daemon contract)** — through Stage 10 this was `protocol.py`, kept
   **byte-stable** as the isolation evolved (subprocess → microVM) so the SDK barely
   changed — the project's defining discipline. **Stage 11 deliberately ended it:** the
   client↔daemon wire is now **ConnectRPC** (`src/microsandbox/connect.py` +
   `daemon/proto/*.proto`), so the e2e suite is now a **behavioral** parity oracle, not
   byte-for-byte. `protocol.py` stays as the SDK's result types
   (Execution/OutputEvent/EventType) + reference for the old SSE wire.
3. **in-VM daemon** — `daemon/` (Go), E2B's `envd`. Runs **inside the VM** on **two TCP
   ports** over the VM's NIC (Stage 12 flipped these from vsock): `envd` (ConnectRPC
   `FilesystemService` + `ProcessService` + a plain `/health`) on `:49983` and a separate
   `code-interpreter` (ConnectRPC streaming `Execute`, driving a stateful Python kernel via a
   Jupyter Kernel Gateway) on `:49999`. It replaced the Python `server.py` / `backend.py`
   (kept in `src/` as reference).
4. **control plane** — `services/` (Go module `microsandbox/services`), split into
   E2B-shaped services (Stages 8–9). **`cmd/api`** is the **lifecycle-only** public REST
   front (owns a Postgres metadata store, `pkg/store` — SQLite still selectable; Stage 14b).
   Since **Stage 16** it **authenticates** every request (except `/health`) with an
   `X-API-Key` resolving to a **team** (keys stored hashed in `pkg/store`; seeded by
   `--seed-api-keys`); sandboxes/builds are **team-owned** so list/delete are team-scoped. It
   calls **`cmd/orchestrator`** over
   **gRPC** (`SandboxService`). The orchestrator owns the microVM fleet + warm pool
   (`pkg/{fc,pool,network,template}`) and a header-routed **data proxy** (`pkg/proxy`) that
   bridges the data path to the daemon over the VM's NIC (TCP; Stage 12). **`cmd/client-proxy`**
   (Stage 9) is the edge data proxy: it reads a shared routing **catalog** (`pkg/catalog`,
   sandbox→`{node, access-token}` in **Redis** since Stage 14a, written by the api on create) and routes each data
   request by its `<port>-<id>` `Host` header
   (Stage 12; the port selects the in-VM service or a user port) to the orchestrator's proxy. Since **Stage 16** it
   **gates the in-VM control ports** (envd `:49983`, code-interpreter `:49999`) on the
   per-sandbox **access token** (`X-Access-Token`), leaving user-exposed ports public. So the SDK talks to **two** endpoints — the api (lifecycle) and client-proxy
   (data, learned from the create response). The orchestrator also hosts a
   **`TemplateService`** (Stage 10): the api's `POST /templates` kicks an async template
   build there (`pkg/build` wrapping the build scripts, `pkg/storage` publishing the artifacts to
   **S3 object storage** — MinIO locally — since Stage 15; the orchestrator materializes rootfs/snapfile
   and streams the memfile from the bucket over UFFD). Since **Stage 17** the memfile is stored
   **compacted** (non-zero blocks only) behind a per-block index (`pkg/storage/header`, mirroring E2B's
   `pkg/storage/header`); the boot path resolves each faulting offset through the mapping and serves
   zero/gap pages without a fetch. Since **Stage 18** that header carries E2B's **copy-on-write owner**, so a
   template built `from` a base stores its **rootfs as a diff** (`{B}/rootfs.ext4` = only its changed blocks +
   a flattened `rootfs.ext4.header`); the boot path **assembles** the full rootfs by reading each run from its
   owning build's object (`MaterializeLayered`), falling back to a whole-object download when there is no header.
   Stage 4 first extracted all this as a single `control-plane/` binary; Stage 8 dissolved it
   into `services/`; Stage 9 sank the data path off the api. See `docs/STAGE10_DESIGN.md` +
   `docs/STAGE9_DESIGN.md` + `docs/STAGE8_DESIGN.md` +
   `docs/E2B_ALIGNMENT_ROADMAP.md` (Stage 4: `docs/STAGE4_DESIGN.md`).

**Key principle**: isolation strength comes from *where the daemon runs* and *how
the client connects* (client/transport concerns), not from the backend. The
backend (`ExecutionBackend` → `JupyterKernelBackend`) only decides *how code
runs*. Keep these axes separate, and keep the client/protocol boundary clean.

## Current state & possible next steps

- **Done**: the Firecracker microVM works end to end — cold start ~0.94s, the data path
  over a per-sandbox TAP/netns NIC (TCP; **inbound-reachable, outbound-denied by default** —
  DNAT, no MASQUERADE), machine-config resource limits, and snapshot restore (~30ms to ready,
  unpooled ~0.7-0.9s on WSL2 from per-sandbox net setup; the warm pool is the ms path).
  See `docs/MICROVM_DESIGN.md` for the design + measured records.
- **Done (Stage 4 — Go control plane)**: the VM lifecycle lives in a standalone Go
  service (`control-plane/`). 4a moved spawn/restore/destroy there; 4b moved the
  vsock proxy + health probe there too, so the SDK is now a thin **pure-HTTP**
  client (no vsock left in Python) and the control plane delivers a sandbox only
  once it is healthy. The vsock-bridge unit tests are in Go now. See
  `docs/STAGE4_DESIGN.md`.
- **Done (Stage 5 — warm pool)**: one base snapshot now forks into N second-scale
  sandboxes. 5a gives each restored VM its own vsock uds via Firecracker v1.16.0's
  `vsock_override` (no snapshot rebuild), lifting the single-instance limit; 5b adds
  a background pool (`--pool-size K`) that pre-warms K VMs and hands one out per
  `from_snapshot` create in **~1ms** (vs ~30ms restore, ~0.94s cold). The pool is
  control-plane-internal — the protocol and SDK are unchanged; its semantics are
  unit-tested without KVM (`control-plane/pool_test.go`). See `docs/STAGE5_DESIGN.md`.
- **Done (Stage 6 — templates)**: the one baked-in image generalizes into **named
  custom images** (E2B's headline feature). A template is a `(rootfs, snapshot)` pair
  under `vendor/templates/<name>/`, built from its own Dockerfile via
  `scripts/build-template.sh`; the reserved name `default` maps to the legacy `vendor/`
  paths, so nothing prior changed. 6a wired the registry + build pipeline (the control
  plane resolves a name to artifacts; repaired + parameterized `build-snapshot.sh`); 6b
  added the optional `template` field to `POST /sandboxes` and `Sandbox(template=...)`
  (absent = default, backward-compatible); 6c made the warm pool **per-template**
  (`--pool name=K`). Name validation + pool config are unit-tested without KVM
  (`control-plane/template_test.go`, `pools_test.go`). See `docs/STAGE6_DESIGN.md`.
- **Done (Stage 7 — Go in-VM daemon / envd)**: the Python in-VM daemon (`server.py` +
  `backend.py`) is rewritten as a static **Go binary** (`daemon/`), matching E2B's
  `envd`. 7a ported health/files/commands (vsock + stdlib `net/http`); 7b did `/execute`
  by driving a stateful Python kernel over a **Jupyter Kernel Gateway** HTTP+WebSocket
  API (E2B's actual approach, not raw ZMQ); 7c flipped the rootfs (`build-rootfs.sh`
  builds+injects the binary, `/init` execs it, the Dockerfile ships the kernel gateway).
  Protocol/SDK/control-plane unchanged; the **whole Python e2e suite passes against the
  Go daemon** (byte-stable parity). The Python daemon stays in `src/` as reference. See
  `docs/STAGE7_DESIGN.md`.
- **Done (Stage 8 — control plane split into `api` + `orchestrator`)**: the monolithic
  `control-plane/` binary is dissolved into a `services/` Go module mirroring E2B's seams.
  8a relocated the fleet logic into leaf packages (`pkg/{fc,pool,proxy,template}`); 8b
  introduced the **gRPC `SandboxService`** boundary — a REST **`api`** in front of a
  per-machine **`orchestrator`** (gRPC + a header-routed vsock data proxy), the api
  reverse-proxying the data path to it for now; 8c gave the api a **SQLite metadata
  store** (`pkg/store`, cgo-free `modernc.org/sqlite`). Protocol + SDK stayed byte-stable —
  the whole Python e2e suite passes (32/32). See `docs/STAGE8_DESIGN.md` +
  `docs/E2B_ALIGNMENT_ROADMAP.md`.
- **Done (Stage 9 — `client-proxy` + sandbox catalog)**: the data plane is sunk off the
  api, which is now **lifecycle-only**. 9a added `pkg/catalog` (in-memory, Redis-shaped
  `sandbox→node`) and stood up **`cmd/client-proxy`** (a public data port + an internal
  control port the api writes routes to), with the api registering each route on create
  (load-bearing — a failure rolls the VM back); 9b flipped the SDK's data path to
  client-proxy (api `POST /sandboxes` returns `data_url`; data goes there with an
  `X-Sandbox-Id` header); 9c removed the api's temporary passthrough. Protocol + SDK surface
  stayed byte-stable — the whole Python e2e suite passes (33/33). See `docs/STAGE9_DESIGN.md`.
- **Done (Stage 10 — `TemplateService` + `pkg/storage`)**: building a custom template is now
  an async, programmatic operation (E2B's "accept sync, build async, poll status"). 10a added
  `pkg/storage` (`StorageProvider` + `Local`) and `pkg/build` (`Builder` wrapping `docker build`
  → `build-rootfs.sh` → `build-snapshot.sh`, with an injectable exec for KVM-free tests); 10b
  added the **gRPC `TemplateService`** in the orchestrator (`TemplateCreate` kicks a build
  goroutine, `TemplateBuildStatus` polled); 10c gave the api `POST /templates` + `GET
  /templates/builds/{id}` (a `builds` table in `pkg/store`) and the SDK `build_template(...)`.
  Artifacts are published **in place** at `vendor/templates/<name>/` because the snapshot bakes
  in its rootfs's absolute path (so no build-id staging). The whole Python e2e suite passes
  (34/34). See `docs/STAGE10_DESIGN.md`.
- **Done (Stage 11 — `envd` → ConnectRPC + a separate `code-interpreter`)**: the in-VM daemon
  now matches E2B's shape, and this **ended the byte-stable-protocol discipline** (the wire is
  ConnectRPC; the e2e is now a *behavioral* oracle). 11a stood up `envd`'s Connect
  `FilesystemService`/`ProcessService` alongside the HTTP endpoints (+ `connect-go`, codegen
  into `daemon/genpb/`); 11b moved the kernel into a `code-interpreter` Connect service on its
  **own vsock port** (orchestrator routes `/codeinterpreter.*` to it; `fc.CodeInterpreterVsockPort`)
  and flipped `run_code` to Connect server-streaming (`src/microsandbox/connect.py`, a hand-rolled
  Connect-JSON client, **no new Python dep**); 11c flipped files/commands to envd Connect unary,
  removed the daemon's HTTP endpoints, and retired `protocol.py`'s SSE wire. e2e 36/36
  (behavioral). See `docs/STAGE11_DESIGN.md`.
- **Done (Stage 12 — per-sandbox TAP/netns networking; data path flipped to TCP; vsock retired)**:
  every sandbox now has a real network identity — a virtio-net NIC backed by a host **TAP** in
  its **own netns**, a fixed guest IP, and a per-slot routable host address via veth + **DNAT
  (no MASQUERADE)**, so it is **inbound-reachable but outbound-denied by default** (`pkg/network`).
  12a gave cold-start + restored/pooled VMs a slot and baked `eth0` into the snapshot; 12b flipped
  the data path from vsock to **TCP routed by `<port>-<id>` hostnames** (`client-proxy` parses the
  `Host` header → orchestrator dials `<slot-ip>:<port>`; the port selects envd `:49983` /
  code-interpreter `:49999`); 12c added **user-port exposure** (`sandbox.get_host(port)` reaches any
  guest port, e.g. a server on `:8000` at `8000-<id>`; `tests/test_ports.py`) and **retired vsock
  entirely** (daemon TCP-only, `mdlayher/vsock` + `vsock_override` removed). This **reverses
  roadmap Decision D1** and is the single most security-relevant change in the project's history;
  the "fully offline" claim is reworded to "inbound-reachable, outbound-denied by default" — still
  a learning implementation, **not security-audited**, never safe to expose to untrusted input. e2e
  37/37 (behavioral, pure TCP). See `docs/STAGE12_DESIGN.md`.
- **Done (Stage 13 — UFFD lazy snapshot restore)**: restore can now serve guest RAM over a
  **`userfaultfd`** handler we own instead of firecracker's `File` backend. 13a added
  `services/pkg/uffd` (a pure-Go page-fault handler — the kernel ABI derived via `_IOWR`,
  `SCM_RIGHTS` fd reception, the fault→memfile-offset math, an epoll `UFFDIO_COPY`/`UFFDIO_ZEROPAGE`
  serve loop; the tree's only `ioctl`/`unsafe`/`mmap` code, `//go:build linux`, KVM-free unit
  tests); 13b wired it into `fc.Restore` behind an orchestrator **`--uffd`** flag (handler started
  before `/snapshot/load`, held on `MicroVM`, stopped in `Destroy`); 13c confirmed the warm pool
  under UFFD + finalized docs. **Honest result: not a single-box speedup** — unpooled restore
  ~0.54s (UFFD) vs ~0.57s (File), within noise (per-sandbox `ip` setup dominates); warm-pool
  hand-out ~11–25ms either way; **e2e 37/37 on both backends**. So **`File` stays the default,
  `--uffd` is opt-in** — the win banked is the `userfaultfd` mechanism and a now-pluggable page
  source (the precondition for the storage swaps). See `docs/STAGE13_DESIGN.md`.
- **Done (Stage 14 — the storage swaps go live: catalog → Redis, store → Postgres)**: two of
  the three state seams Decision D3 built behind E2B-shaped interfaces now use the same *kind*
  of backend E2B does. 14a backed the routing **catalog** with a shared **Redis** (`pkg/catalog`
  gained `redis.go`; the api `SET`s `sandbox:<id>→node` on create and client-proxy `GET`s it on
  each data request) and **deleted the api→client-proxy internal control RPC** — a shim that only
  existed because the map lived in one process; `InMemory` survives as a unit-test double. 14b made
  **`pkg/store` an interface** with two impls (`sqlite.go` + a new `postgres.go` over pure-Go
  `jackc/pgx/v5`), `Open(dsn)` dispatching by URL scheme; the api now **defaults to Postgres**
  (`--store-dsn`, SQLite still selectable via `sqlite://`). A repo-root `docker-compose.yml` plus
  the conftest/dev-up fixtures provision `postgres:16-alpine` + `redis:7-alpine`. **Honest: on one
  box this is fidelity, not speed** (each adds a socket hop the in-process map / local file didn't
  pay) — the payoff is a genuinely cross-process catalog, a store with real concurrency that
  survives restarts, and the precondition for multi-host. Go units stay hermetic (the Redis /
  Postgres variants skip without `REDIS_ADDR` / `MSB_TEST_PG_DSN`); real-machine e2e **37/37** on
  Postgres + Redis (behavioral parity — the swap moved state location, not semantics). The third
  seam (`Local → object storage`) is split to **Stage 15**. See `docs/STAGE14_DESIGN.md`.
- **Done (Stage 15 — the last storage swap: `Local → object storage`, MinIO/S3)**: template artifacts
  now live in an **S3 object store** (the running default) — the third state seam Decision D3 built
  behind an interface, and the one that is **not isomorphic** (a Firecracker snapshot bakes in its
  rootfs's absolute path, so artifacts can't merely be opened from a bucket). 15a made the Stage-13
  UFFD page source **pluggable** (`uffd.PageSource` + `MmapSource`, pure refactor; e2e 37/37 unchanged);
  15b reshaped `pkg/storage` into E2B's blob `StorageProvider` (`Upload`/`Open`/`OpenReaderAt`/`Exists`,
  `s3.go` over pure-Go `minio-go` + `Local` as the test double), added a **chunked bucket page source**
  (`uffd.NewChunkedSource`, 1 MiB), made `pkg/build` **upload** to immutable `{buildID}/…` + flip an
  `aliases/<name>` pointer, and wired the orchestrator (`--storage s3` default / `local-fs`) to
  **materialize rootfs + snapfile** to their baked local paths and **stream the memfile page-by-page
  from the bucket via UFFD** (the Stage-13 payoff); the default template is seeded by `cmd/msb-seed`,
  compose gained `minio`. **Honest: on one box this is fidelity, not speed** (per-page would blow the
  health timeout, so reads are chunked) — the win is real object storage + a page source pluggable end
  to end (mmap ↔ bucket), the precondition for multi-host / peer-sourced memory. e2e **37/37 in s3
  mode** (memfile streamed from MinIO; `local-fs` + `local-fs --uffd` escape hatches green too). The
  fidelity gaps deliberately deferred — **NBD-streamed rootfs** (E2B serves the rootfs lazily too, not
  materialized), chunk/header, COW layer diffs (the header+merge done in Stage 18; rootfs), a cross-node
  cache — are itemized in `docs/STAGE15_DESIGN.md` §11. (Compression was listed here too; current E2B **does**
  optionally compress — V4/V5 headers, zstd/lz4 in 2 MiB frames, raw V3 still supported — but it is orthogonal to
  COW and we still store raw, so it stays deferred optional depth. See `docs/STAGE20_DESIGN.md` §2.)
  See `docs/STAGE15_DESIGN.md`.
- **Done (Stage 16 — auth: `X-API-Key`→team + a data-plane access token)**: the first
  production-fidelity stage gives the system **identity** (every prior stage was "one box, no
  auth"). 16a made the api **authenticate** every request (except `/health`) with an `X-API-Key`
  resolving to a **team** (a `withAuth` middleware: sha256 the key → `ResolveAPIKey` → team in
  ctx; 401 on miss, 500 on a store failure — distinct), seeded a dev key via `--seed-api-keys`
  (`key=team` list, default `msb_dev_key=default`), and made `pkg/store` **team-aware** (new
  `teams`/`api_keys` tables — keys stored **hashed** — plus an idempotent `team_id` migration on
  `sandboxes`/`builds`; `Open` runs the ALTER so an existing DB upgrades); resources are
  **team-owned** so list/delete/build-status are team-scoped (another team's id is **404**, not
  403 — no existence leak; the ownership check precedes any VM teardown). 16b gave the data plane
  a **per-sandbox access token**: the api mints `sbx_`+128-bit on create, stores it in the catalog
  (`catalog.Route{Node, Token}` — Redis JSON, with a legacy bare-node fallback that fails closed),
  and returns it; client-proxy **gates the in-VM control ports** (envd `:49983`, code-interpreter
  `:49999`) on `X-Access-Token` (constant-time compare; empty token never authorises), while
  **user-exposed ports stay public** (the exposure feature; `test_ports.py` stays green); the SDK
  reads `api_key=`/`MICROSANDBOX_API_KEY` and sends both headers (no silent default, matching
  E2B). **Honest scope:** keys are seeded by flag (no key-management API), the token is bearer-only
  (no expiry/rotation/signing), and everything is still plaintext on loopback — auth here is the
  *seam*, not a hardened gateway; **still not security-audited, never safe for untrusted input.**
  Go units green (incl. live Postgres + Redis); real-machine e2e **43/43** (37 prior + 6 auth).
  See `docs/STAGE16_DESIGN.md`.
- **Done (Stage 17 — storage depth (1): a `.header`-indexed, compacted memfile)**: the first of the
  deferred storage-mechanism-depth items (`docs/STAGE15_DESIGN.md` §11), and the first storage stage that
  **stores and fetches strictly less** rather than only adding fidelity. 17a added `pkg/storage/header`
  (mirroring E2B's `pkg/storage/header`): a fixed-width `Metadata` + an ordered `Mapping` of present
  (non-zero) runs, `Serialize`/`Deserialize`, and `Build`/`BuildFile` that scan a memfile in 4 KiB blocks
  into `(mapping, compacted-bytes)` by dropping all-zero blocks — pure, KVM-free, hand-serialized
  little-endian (no new dep; E2B's per-entry `BuildId`/`BaseBuildId` dropped until COW). 17b wired it:
  `pkg/build` + `cmd/msb-seed` upload via a shared `storage.PublishMemfile` (compacted `{buildID}/memfile`
  + `{buildID}/memfile.header`); `pkg/uffd` gained `NewMappedSource` + a plain `Extent` (stays
  storage-free) that serves a gap as zeros **with no fetch** and present runs through the chunk cache at
  the remapped physical offset; `prepareRestore` probes the header (`storage.OpenMemfileHeader`) → mapped
  source, else falls back to the Stage-15 full-object `chunkedSource` (an old, unindexed bucket still
  boots — covered by a unit test). **Measured:** the 512.0 MiB default memfile → **228.6 MiB** compacted
  (44.6%, **2.2×**) across 272 runs, a 6,560-byte header, ~0.52 s one-time build scan; e2e **43/43** in s3
  mode (the compacted memfile streamed over UFFD via the mapping). **Honest:** not a single-box latency
  win (restore-to-ready is dominated by per-sandbox net setup, unchanged); the win is real compaction +
  zero-page fetch elision, and the `header` mechanism is the shared substrate the deferred NBD rootfs /
  COW layers / chunk cache all consume. See `docs/STAGE17_DESIGN.md`.
- **Done (Stage 18 — storage depth (2): COW layered rootfs builds)**: E2B's real **copy-on-write layering**,
  banked on the **rootfs** (a build-time block diff is meaningful there; the memfile needs live-VM
  re-snapshotting → Stage 20). 18a grew `pkg/storage/header` into the full COW algebra — a per-entry **build
  owner** as a header-local **build-table index** (not a uuid → zero new deps), `Metadata`
  `BuildId`/`BaseBuildId`/`Generation`, **format v2** alongside the Stage-17 v1 memfile, and
  `CreateMapping`/`MergeMappings`/`NormalizeMappings`/`BuildDiff`/`Locate`. 18b added the `pkg/storage` mechanism
  (`PublishRootfsDiff` — diff the child vs the assembled base, upload only changed non-zero blocks +
  `{B}/rootfs.ext4.header`; `MaterializeLayered` — assemble the full rootfs by reading each run from its **owning
  build's** object; no header → the Stage-15 whole-object download). 18c wired the producer + boot path
  (`build-rootfs.sh` learned a **fixed-size** arg; `pkg/build.Build` gained `base` → size-pin +
  `PublishRootfsDiff`; orchestrator `prepareSpawn`/`prepareRestore` use `MaterializeLayered`; proto `base` field
  **hand-edited** into the committed stub since protoc is absent, wire round-trip unit-tested). 18d wired the
  surface (api `POST /templates` `from`, SDK `build_template(base=…)`) + a real-VM e2e
  (`test_layered_template_via_api`: build `derived` over `default`, boot, content carried, code runs).
  **Honest headline:** the mechanism is faithful and boots, but the **size win is bounded (~2.07×, 576 → 278.8
  MiB), not the lab ~40×** — `docker build … RUN` + `docker export | mkfs.ext4 -d` **reshuffles the ext4 block
  layout** when a layer is added (measured: two fresh mkfs of the *same* image differ ~3%, a `RUN`-layer child
  differs from the base ~48%), so ~half the content reads as "changed". The size-pin (Decision 8) is
  **necessary but not sufficient**; the missing piece is **block-layout preservation** (E2B mutates a persisted
  block device in place; we re-create the FS each build) — a known divergence, *not* a defect in the merge
  algebra (correct for any layout). e2e **44/44** in s3 mode. **Stage 19 (next bullet) closed the block-layout
  gap.** See `docs/STAGE18_DESIGN.md`.
- **Done (Stage 19 — storage depth (3): layout-preserving layered rootfs, the COW payoff)**: closes Stage 18's one
  gap (**block-layout preservation**) so the COW machinery actually pays off. A layered child's rootfs is no longer
  re-`mkfs.ext4 -d`'d from a fresh `docker export` (which reshuffles the ext4 layout when a layer is added); instead
  `scripts/build-rootfs-layered.sh` (19a) **copies the base template's rootfs image and applies only the child's
  file delta in place via `debugfs`** (unprivileged, no mount/loop/root — the single-box analogue of E2B's in-place
  block-device layer; the delta is `docker export` child-vs-`FROM` trees diffed by `rsync -c` → debugfs
  `write`/`rm`/`mkdir`/`symlink`). 19b wired it into `pkg/build` for `base`-set builds (materialize the base via
  `MaterializeLayered` → run the layered builder → unchanged `PublishRootfsDiff`), **retiring the Stage-18 size-pin**
  (dropped the now-orphaned `RootfsLogicalSize`) and parsing the recipe's first `FROM` (`firstFromImage`, Decision
  3). 19c re-ran the real-VM e2e + added a **measured size assertion**: a Go probe `services/cmd/msb-rootfs-stat`
  reports the bucket's stored-vs-logical bytes and the e2e asserts `stored < full/50`. Header / merge /
  `MaterializeLayered` / boot / api / SDK all **unchanged**. **Measured: the same `derived` (default + one `RUN`) now
  stores a 28 KiB rootfs diff over the 576 MiB base — 0.0047%, down from Stage 18's 278.8 MiB / 48%, ~10,000×
  smaller (≈ the genuine delta).** e2e **44/44** in s3 mode. See `docs/STAGE19_DESIGN.md`.
- **Done (Stage 20 — storage depth (4): COW layered memfile via live-VM re-snapshot)**: the **last artifact
  E2B layers that we stored per-build in full** (the memfile / guest RAM) is now a **COW diff over the base**,
  served lazily over UFFD by the same `pkg/storage/header` algebra as the rootfs. **20a** (`34d0a8f`) the
  multi-owner page source (`uffd.NewLayeredSource` — a fault resolves to its owning build's object, zero-owner
  → zeros). **20b** (`99abb8c`) the KVM-free algebra (`storage.PublishMemfileDiff` = materialize the base's full
  memfile, `header.BuildDiff` the child against it, `MergeMappings` → a flattened v2 header, upload only changed
  non-zero blocks; `MaterializeMemfileFull` the diff-time expander) + the layered read wiring (`prepareRestore`
  routes a v2 memfile through the multi-owner source, v1 stays on `NewMappedSource` — zero regression). **20c-1**
  (`058757d`) the **live-VM producer**: `fc.MicroVM.Snapshot` (pause + Full snapshot-create, previously
  shell-only) + the orchestrator's `LayeredSnapshot` (injected into `pkg/build` as a `Snapshotter`) — a layered
  build with a snapshot no longer runs `build-snapshot.sh`; instead it **resumes the base self-consistently**
  (`restoreHealthy(baseTmpl)`), re-snapshots, and stores the child's memfile as a COW diff + records the baked
  rootfs path (`fc.RootfsBacking.BakedPath`, bound at restore over the **base's** path since the child's vmstate
  is a re-snapshot of the base). **The producer fork was decided here** (see `docs/STAGE20_DESIGN.md` D5,
  revised): Stage 21's research showed E2B resumes the *parent* self-consistently + runs the layer's command
  in-guest (NOT grafting base RAM onto a child rootfs); full fidelity needs an in-guest command subsystem +
  writable overlay we lack, so we took the **E2B-closest reachable option — self-consistent base resume, no
  grafting, no in-guest command**. **Honest consequence:** the child's RAM is the base's warm RAM plus only the
  resume→health→re-snapshot delta — a *maximal* COW win, but no per-child warm working set. A layered snapshot is
  **restorable only under `--nbd`** (the child's rootfs is served at the base's baked path via the per-VM bind;
  the producer enforces it). **Not a single-box speedup** (net setup dominates restore). **20c-2** (this stage's
  tail): the real-VM e2e `test_layered_snapshot_via_api` (build `derived_snap` over `default` with a snapshot,
  restore over `--nbd`, assert boot + child disk content + code runs) + a `msb-memfile-stat` probe asserting the
  stored memfile diff is a small fraction of the base's compacted memfile — **written, pending a KVM run in
  `--nbd` s3 mode** (`MSB_ORCH_FLAGS=--nbd`). Go units green. See `docs/STAGE20_DESIGN.md`.
- **Done (Stage 21 — NBD-served rootfs: lazy block streaming)**: the last "materialized whole" artifact is now
  streamed lazily — the rootfs is served to the guest over a **kernel NBD device (`/dev/nbdX`)** whose blocks
  our userspace server resolves through the same `pkg/storage/header` COW mapping + chunked bucket reader as
  the memfile (the disk-side analogue of the Stage-13/15 UFFD memfile), instead of assembling the whole rootfs
  to a baked local path at boot. **21a** `services/pkg/nbd` (device pool `modprobe nbd` + sysfs free scan; a
  hand-rolled `Dispatch` server for the 28-byte NBD protocol; the kernel bind over **netlink multiconn** via
  the one new runtime dep `github.com/Merovius/nbd/nbdnl`, Decision D1 — verified on real hardware by a gated
  `TestBindRealDeviceRoundTrip`). **21b** `services/pkg/block` — E2B's COW block stack: a read-only layered
  base (reusing `uffd.NewLayeredSource`) under a per-VM writable `Overlay`/`Cache` with `ExportToDiff` (built
  + unit-tested, the Stage-20 producer's input). **21c** wiring: `fc.RootfsBacking` (Spawn points the drive at
  the device; **Restore `mount --bind`s the device over the snapshot's existing baked rootfs path inside a
  per-VM mount namespace** — so no constant path and **no snapshot rebuild**, and N concurrent restores stay
  isolated); orchestrator `--nbd` flag → an `nbd.Pool` + `buildRootfsBacking` (lazy base → `block.NewReadOnly`
  → `nbd.Bind`), `prepareSpawn`/`prepareRestore` skip `MaterializeLayered`. **Two honest scope decisions:**
  (1) **served read-only** — the writable overlay (D2) is deferred to Stage 20 because our
  cold-boot-to-snapshot model (unlike E2B's resume-and-re-snapshot) needs the producer to keep the snapshot
  consistent with guest writes; RO is provably consistent and needs no snapshot change. (2) **not a
  single-box speedup** — unpooled restore is *slower* (~3.5s vs ~1.6s) because the guest faults its working
  set over NBD on first access; the warm pool hides it. Real-VM e2e **44/44** in `--nbd` s3 mode (cold-start +
  restore + concurrent restores + layered template all boot over NBD). See `docs/STAGE21_DESIGN.md`.
- **Possible next** (per `docs/E2B_ALIGNMENT_ROADMAP.md`): **deepen the Stage-20 memfile producer to full
  E2B fidelity** — an **in-guest command-execution subsystem** (start/ready commands) so a layer runs its
  actual command in the guest, dirtying a per-child warm working set, plus the **writable-overlay `rw` rootfs**
  (Stage 21b, built + wired + waiting) so that run's disk writes and RAM are captured by one self-consistent
  re-snapshot (E2B's two-diffs-from-one-run model, replacing our docker+debugfs rootfs). Then production
  fidelity — multi-host scheduling over the now-shared catalog/store/bucket (`placement.BestOfK`), a TypeScript
  SDK; a cross-node chunk cache; and auth depth (a key-management API, token expiry/rotation, TLS). Note:
  memfile/rootfs **compression IS an optional E2B mechanism** (V4/V5 headers, zstd/lz4 in 2 MiB frames, raw V3
  still supported, orthogonal to COW); we still store raw, so it stays deferred optional depth.

## Development conventions

- Python ≥ 3.11. Runtime deps are introduced only where needed, with a stated
  reason: the agent image ships `ipykernel` + the **Jupyter Kernel Gateway**, which the
  Go in-VM daemon launches and drives over HTTP/WebSocket to run a stateful Python
  kernel (Stage 7; the `[kernel]` extra + `backend.py`'s `jupyter_client` belong to the
  retired Python daemon, kept as reference). The host side shells out to the
  `firecracker` binary (like it shells out to `docker` to build the rootfs) — no Python
  VM library.
- **Language: English only.** All docs, code comments, docstrings, and commit
  messages are in English. Comments explain **why**, not what.
- Keep `tests/` all green. The host-side unit tests now live in Go
  (`go test ./services/...` — TCP data proxy, network slot derivation, pool, templates, the
  metadata store (incl. teams/keys + team scoping), the catalog + client-proxy routing (incl.
  the per-sandbox token gate), no VM/KVM needed; the Redis / Postgres
  catalog + store variants self-skip unless `REDIS_ADDR` / `MSB_TEST_PG_DSN` point at a live
  server, so a bare `go test` stays hermetic). The Python end-to-end / stateful / snapshot /
  metadata / ports / auth tests run on real VMs (driven through the api + client-proxy + orchestrator)
  and auto-skip when go / firecracker / `/dev/kvm` / the per-sandbox-network privilege / the
  vendor artifacts are missing; since Stages 14–15 the fixture also brings up Postgres + Redis +
  MinIO (`docker compose`, with a `docker run` fallback) and seeds the template artifacts into the
  bucket, so on a box with **no docker** the VM group now fails loudly rather than silently running
  on in-process state. Since Stage 16 the fixture also exports `MICROSANDBOX_API_KEY=msb_dev_key`
  (the api seeds it) so existing tests authenticate unchanged. The orchestrator defaults to `--storage s3`; `MSB_ORCH_FLAGS="--storage
  local-fs"` reverts the VM e2e to reading artifacts from `vendor/` directly, and `MSB_ORCH_FLAGS="--nbd"` runs the
  VM e2e with the rootfs served over NBD (Stage 21; needs the `nbd` kernel module + root, so it self-skips
  otherwise). The Go NBD unit tests are hermetic; the real-device bind is a gated test (`MSB_TEST_NBD=1` + root).
- **Safety rule**: the microVM is the first isolation strong enough to *discuss*
  untrusted code, but it is a learning implementation, **not security-audited** —
  never imply in docs or code that it is safe to expose as a service or feed
  arbitrary external input. Since Stage 12 the sandbox is no longer "fully offline":
  it is **inbound-reachable** (so its ports can be exposed) and **outbound-denied by
  default** (DNAT only, no MASQUERADE) — this *narrows* the isolation, so the
  not-safe-for-untrusted-input rule matters more, not less. Stage 16 added a **lock on the
  control plane** (`X-API-Key`→team) and a **token on the data plane**, but these are learning
  seams over **plaintext loopback** — not a hardened gateway, and they do **not** change the
  not-safe-for-untrusted-input rule.

## Common commands

```bash
pip install -e ".[dev]"                          # install (dev mode)
docker compose up -d                             # Stages 14-15: bring up the shared state (postgres + redis + minio) the control plane uses; conftest + dev-up also start it on demand (with a `docker run` fallback for engines without the compose plugin)
pytest                                           # run tests (VM cases auto-skip without go/firecracker/kvm; the fixture builds+runs api + client-proxy + orchestrator, provisions postgres + redis + minio, and seeds template artifacts into the bucket; orchestrator defaults to --storage s3, MSB_ORCH_FLAGS=--storage local-fs reverts to local artifacts)
go test ./services/...                           # host-side unit tests: TCP data proxy, network slot derivation, pool, templates, metadata store, catalog + client-proxy (no VM/KVM; the redis/postgres variants skip unless REDIS_ADDR / MSB_TEST_PG_DSN are set)
go test ./daemon                                 # in-VM daemon unit tests: handlers + kernel-message translation (no VM/KVM)
pytest tests/test_microvm.py::test_runs_in_microvm -v   # one real-VM end-to-end case

# One-time microVM setup (see docs/MICROVM_DESIGN.md §7):
sudo usermod -aG kvm "$USER"                     # then `wsl --shutdown` and reopen, to open /dev/kvm without sudo
docker build -t microsandbox-agent .             # the agent image the rootfs is exported from
scripts/build-rootfs.sh                          # export the ext4 rootfs from the agent image (no root)
scripts/build-snapshot.sh                        # build the warm snapshot for millisecond restore
scripts/build-template.sh <name>                 # build a named custom image -> vendor/templates/<name>/ (Stage 6; then Sandbox(template="<name>"))
scripts/build-services.sh                        # build the Go host services (api + client-proxy + orchestrator) to vendor/ (Stage 8-9)
scripts/gen-proto.sh                              # regenerate the gRPC stubs from services/proto (only when a .proto changes; needs protoc)

# Minimal end-to-end smoke (Stages 8-9: start orchestrator + client-proxy + api first; needs the vendor artifacts):
scripts/dev-up.sh &                              # builds + runs all three; SDK base_url = http://127.0.0.1:8080 (pass --pool-size K / --pool name=K to warm VMs)
export MICROSANDBOX_API_KEY=msb_dev_key          # Stage 16: the api authenticates X-API-Key->team; the api seeds this dev key by default
python -c 'from microsandbox import Sandbox; s=Sandbox(); s.run_code("x=41"); print(s.run_code("print(x+1)").stdout); s.close()'
kill %1                                           # stop the services (dev-up traps the signal and stops all three)

# After editing the in-VM daemon (daemon/*.go), rebuild the rootfs (+ snapshot) so the VM
# picks up the change -- the rootfs bakes in the compiled daemon binary at build time. Also
# rebuild ANY built template rootfs (they bake the same daemon; the e2e fixture only builds a
# rootfs when absent, so a stale one silently runs the OLD daemon). If a daemon/proto changed,
# rerun scripts/gen-proto.sh first (needs protoc + protoc-gen-connect-go).
scripts/build-rootfs.sh && scripts/build-snapshot.sh           # the default image
scripts/build-template.sh example --no-snapshot                 # + each built template under vendor/templates/
```

## Working notes for Claude

- Before changing the isolation/transport layer, read `docs/ARCHITECTURE.md` to
  confirm the boundaries, then act.
- The host control plane lives in `services/` (Go module `microsandbox/services`):
  `cmd/{api,client-proxy,orchestrator,msb-seed,msb-rootfs-stat,msb-memfile-stat}` are the binaries (`msb-seed`
  publishes the baked default/script-built templates into the object store; `msb-{rootfs,memfile}-stat` are the
  e2e COW-win probes for the layered rootfs (Stage 19) / memfile (Stage 20) — all dev/test glue),
  `pkg/{fc,pool,network,proxy,template,store,catalog,storage,build,uffd,nbd,block}` the libraries (`nbd` = the
  Stage-21 NBD device pool + userspace server; `block` = the COW block stack served over it), `proto/` the gRPC
  contract (`orchestrator` + `templatemanager`; generated stubs in `pkg/grpc/`, committed — rerun
  `scripts/gen-proto.sh` only when a `.proto` changes, which needs `protoc`). Host-side
  changes take effect at the next `scripts/build-services.sh`; no rootfs rebuild needed
  (that is only for the daemon).
- The in-VM daemon is `daemon/` (Go), baked into `vendor/rootfs.ext4` as a static
  binary at build time, so changes to it only take effect after `scripts/build-rootfs.sh`
  (and `build-snapshot.sh` for the snapshot path). Host-side changes (`client.py`) take
  effect immediately. (`src/microsandbox/server.py` / `backend.py` are the retired Python
  daemon, kept as reference — editing them does nothing unless you wire them back.)
- **Cadence**: split work into independently verifiable sub-steps, keep tests
  green at every step, give an honest self-review (🔴/🟡/🟢) before committing, and
  commit only on the user's explicit go-ahead. Commit messages are a **single-line**
  English Conventional Commit, kept minimal: **`type: summary`** — no `(scope)`, no
  `(stage N)` suffix, no body — plus the `Co-Authored-By` trailer. **After every commit,
  push to `origin/main` immediately** (no separate ask needed).
```
