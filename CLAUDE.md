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
boundary, control channel over **vsock**, and a stateful Jupyter kernel inside the
VM. The project was originally built up in stages (host subprocess → Docker
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
3. **in-VM daemon** — `daemon/` (Go), E2B's `envd`. Runs **inside the VM** on **two vsock
   ports** (Stage 11): `envd` (ConnectRPC `FilesystemService` + `ProcessService` + a plain
   `/health`) and a separate `code-interpreter` (ConnectRPC streaming `Execute`, driving a
   stateful Python kernel via a Jupyter Kernel Gateway). It replaced the Python `server.py`
   / `backend.py` (kept in `src/` as reference).
4. **control plane** — `services/` (Go module `microsandbox/services`), split into
   E2B-shaped services (Stages 8–9). **`cmd/api`** is the **lifecycle-only** public REST
   front (owns a SQLite metadata store, `pkg/store`); it calls **`cmd/orchestrator`** over
   **gRPC** (`SandboxService`). The orchestrator owns the microVM fleet + warm pool
   (`pkg/{fc,pool,template}`) and a header-routed vsock **data proxy** (`pkg/proxy`) that
   bridges the data path to the daemon. **`cmd/client-proxy`** (Stage 9) is the edge data
   proxy: it owns a routing **catalog** (`pkg/catalog`, sandbox→node, written by the api on
   create) and routes each data request by its `X-Sandbox-Id` header to the orchestrator's
   proxy. So the SDK talks to **two** endpoints — the api (lifecycle) and client-proxy
   (data, learned from the create response). The orchestrator also hosts a
   **`TemplateService`** (Stage 10): the api's `POST /templates` kicks an async template
   build there (`pkg/build` wrapping the build scripts, `pkg/storage` placing the artifacts).
   Stage 4 first extracted all this as a single `control-plane/` binary; Stage 8 dissolved it
   into `services/`; Stage 9 sank the data path off the api. See `docs/STAGE10_DESIGN.md` +
   `docs/STAGE9_DESIGN.md` + `docs/STAGE8_DESIGN.md` +
   `docs/E2B_ALIGNMENT_ROADMAP.md` (Stage 4: `docs/STAGE4_DESIGN.md`).

**Key principle**: isolation strength comes from *where the daemon runs* and *how
the client connects* (client/transport concerns), not from the backend. The
backend (`ExecutionBackend` → `JupyterKernelBackend`) only decides *how code
runs*. Keep these axes separate, and keep the client/protocol boundary clean.

## Current state & possible next steps

- **Done**: the Firecracker microVM works end to end — cold start ~0.94s, vsock
  control channel, machine-config resource limits, no guest NIC (the sandbox code
  is fully offline while still manageable), and snapshot restore (~30ms to ready).
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
- **Possible next** (per `docs/E2B_ALIGNMENT_ROADMAP.md`): deferred — Stage 12 TAP/netns
  networking + real `<port>-<id>` hostnames (then SQLite→Postgres, in-mem→Redis, Local→object
  storage go live); then auth, multi-host scheduling, a TypeScript SDK.

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
  (`go test ./services/...` — vsock bridge, pool, templates, the metadata store, the
  catalog + client-proxy routing, no VM/KVM needed); the Python end-to-end / stateful /
  snapshot / metadata tests run on real VMs (driven through the api + client-proxy +
  orchestrator) and auto-skip when go / firecracker / `/dev/kvm` / the vendor artifacts
  are missing.
- **Safety rule**: the microVM is the first isolation strong enough to *discuss*
  untrusted code, but it is a learning implementation, **not security-audited** —
  never imply in docs or code that it is safe to expose as a service or feed
  arbitrary external input.

## Common commands

```bash
pip install -e ".[dev]"                          # install (dev mode)
pytest                                           # run tests (VM cases auto-skip without go/firecracker/kvm; the fixture builds+runs api + client-proxy + orchestrator)
go test ./services/...                           # host-side unit tests: vsock bridge, pool, templates, metadata store, catalog + client-proxy (no VM/KVM)
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
  `cmd/{api,client-proxy,orchestrator}` are the binaries,
  `pkg/{fc,pool,proxy,template,store,catalog,storage,build}` the libraries, `proto/` the gRPC
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
