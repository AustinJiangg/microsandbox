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
   `Sandbox().run_code(...)`. As of Stage 4 it is a thin **pure-HTTP** client: it
   drives the control plane (`POST`/`DELETE /sandboxes`) and runs code through it
   (`/sandboxes/{id}/...`); it holds no vsock code anymore.
2. **protocol (wire protocol)** — `src/microsandbox/protocol.py`. The contract
   between client and daemon. **This is the most important boundary; it stayed
   byte-stable as the isolation evolved from subprocess to microVM — keep it that
   way.**
3. **in-VM daemon** — `daemon/` (Go), E2B's `envd`. Runs **inside the VM**, listens on
   vsock, serves the protocol, and drives a stateful Python kernel via a Jupyter Kernel
   Gateway (Stage 7). It replaced the Python `server.py` / `backend.py` (kept in `src/`
   as reference).
4. **control plane** — `control-plane/` (Go), built to `vendor/control-plane`. Owns
   the microVM fleet: spawn / restore / destroy (ported from the SDK's old
   `_spawn_microvm` / `_restore_microvm` / `close`). New in Stage 4; the wire
   protocol stayed untouched. See `docs/STAGE4_DESIGN.md`.

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
- **Possible next**: more **E2B alignment** — auth, multi-host scheduling, a TypeScript
  SDK, and per-template resource limits / start commands.

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
- Keep `tests/` all green. The vsock-bridge unit tests now live in Go
  (`go test ./control-plane`, no VM/KVM needed); the Python end-to-end / stateful
  / snapshot tests run on real VMs (driven through the control plane) and auto-skip
  when go / firecracker / `/dev/kvm` / the vendor artifacts are missing.
- **Safety rule**: the microVM is the first isolation strong enough to *discuss*
  untrusted code, but it is a learning implementation, **not security-audited** —
  never imply in docs or code that it is safe to expose as a service or feed
  arbitrary external input.

## Common commands

```bash
pip install -e ".[dev]"                          # install (dev mode)
pytest                                           # run tests (VM cases auto-skip without go/firecracker/kvm; the fixture builds+runs the control plane)
go test ./control-plane                          # control-plane unit tests: vsock bridge, pool, templates (no VM/KVM)
go test ./daemon                                 # in-VM daemon unit tests: handlers + kernel-message translation (no VM/KVM)
pytest tests/test_microvm.py::test_runs_in_microvm -v   # one real-VM end-to-end case

# One-time microVM setup (see docs/MICROVM_DESIGN.md §7):
sudo usermod -aG kvm "$USER"                     # then `wsl --shutdown` and reopen, to open /dev/kvm without sudo
docker build -t microsandbox-agent .             # the agent image the rootfs is exported from
scripts/build-rootfs.sh                          # export the ext4 rootfs from the agent image (no root)
scripts/build-snapshot.sh                        # build the warm snapshot for millisecond restore
scripts/build-template.sh <name>                 # build a named custom image -> vendor/templates/<name>/ (Stage 6; then Sandbox(template="<name>"))
scripts/build-control-plane.sh                   # build the Go control plane to vendor/control-plane (Stage 4)

# Minimal end-to-end smoke (Stage 4: start the control plane first; needs the vendor artifacts):
./vendor/control-plane &                         # owns the microVM fleet over HTTP (--pool-size K warms K default VMs; --pool name=K warms a template, Stage 5/6)
python -c 'from microsandbox import Sandbox; s=Sandbox(); s.run_code("x=41"); print(s.run_code("print(x+1)").stdout); s.close()'
kill %1                                           # stop the control plane

# After editing the in-VM daemon (daemon/*.go), rebuild the rootfs (+ snapshot) so the
# VM picks up the change -- the rootfs bakes in the compiled daemon binary at build time:
scripts/build-rootfs.sh && scripts/build-snapshot.sh
```

## Working notes for Claude

- Before changing the isolation/transport layer, read `docs/ARCHITECTURE.md` to
  confirm the boundaries, then act.
- The in-VM daemon is `daemon/` (Go), baked into `vendor/rootfs.ext4` as a static
  binary at build time, so changes to it only take effect after `scripts/build-rootfs.sh`
  (and `build-snapshot.sh` for the snapshot path). Host-side changes (`client.py`) take
  effect immediately. (`src/microsandbox/server.py` / `backend.py` are the retired Python
  daemon, kept as reference — editing them does nothing unless you wire them back.)
- **Cadence**: split work into independently verifiable sub-steps, keep tests
  green at every step, give an honest self-review (🔴/🟡/🟢) before committing, and
  commit only on the user's explicit go-ahead. Commit messages are a **single-line**
  English Conventional Commit (`type(scope): summary (stage N)`, no body) + the
  `Co-Authored-By` trailer. **After every commit, push to `origin/main` immediately**
  (no separate ask needed).
```
