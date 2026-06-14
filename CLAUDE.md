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
   `Sandbox().run_code(...)`. As of Stage 4 it no longer creates the VM itself: it
   asks the control plane over HTTP (`POST`/`DELETE /sandboxes`), then (in Stage 4a)
   still connects to that VM over the vsock transport (`_VsockTransport`).
2. **protocol (wire protocol)** — `src/microsandbox/protocol.py`. The contract
   between client and daemon. **This is the most important boundary; it stayed
   byte-stable as the isolation evolved from subprocess to microVM — keep it that
   way.**
3. **daemon + backend** — `server.py` / `backend.py`. The daemon runs **inside the
   VM**, listening on vsock; `JupyterKernelBackend` is the stateful kernel that
   actually runs the code.
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
- **In progress (Stage 4 — Go control plane)**: 4a is done — VM lifecycle moved out
  of the SDK into a standalone Go service (`control-plane/`); the SDK now drives it
  over HTTP and still reaches the VM over vsock itself. Next is 4b: move the vsock
  proxy + health probe into the control plane so the SDK becomes pure HTTP. See
  `docs/STAGE4_DESIGN.md`.
- **Possible next**: a warm pool (one base snapshot forked into N second-scale
  sandboxes — needs a per-VM vsock uds override), plus further productization
  (templates, auth, multi-host scheduling, a TypeScript SDK).

## Development conventions

- Python ≥ 3.11. Runtime deps are introduced only where needed, with a stated
  reason: `ipykernel` + `jupyter_client` (the `[kernel]` extra) power the in-VM
  Jupyter kernel and are pre-installed in the agent image (lazily imported in
  `backend.py`). The host side shells out to the `firecracker` binary (like it
  shells out to `docker` to build the rootfs) — no Python VM library.
- **Language: English only.** All docs, code comments, docstrings, and commit
  messages are in English. Comments explain **why**, not what.
- Keep `tests/` all green. The vsock-transport unit tests run anywhere; the
  end-to-end / stateful / snapshot tests run on real VMs and auto-skip when
  firecracker / `/dev/kvm` / the vendor artifacts are missing.
- **Safety rule**: the microVM is the first isolation strong enough to *discuss*
  untrusted code, but it is a learning implementation, **not security-audited** —
  never imply in docs or code that it is safe to expose as a service or feed
  arbitrary external input.

## Common commands

```bash
pip install -e ".[dev]"                          # install (dev mode)
pytest                                           # run tests (VM cases auto-skip without go/firecracker/kvm; the fixture builds+runs the control plane)
pytest tests/test_transport.py -q                # the vsock unit tests (no VM/KVM/go needed)
pytest tests/test_microvm.py::test_runs_in_microvm -v   # one real-VM end-to-end case

# One-time microVM setup (see docs/MICROVM_DESIGN.md §7):
sudo usermod -aG kvm "$USER"                     # then `wsl --shutdown` and reopen, to open /dev/kvm without sudo
docker build -t microsandbox-agent .             # the agent image the rootfs is exported from
scripts/build-rootfs.sh                          # export the ext4 rootfs from the agent image (no root)
scripts/build-snapshot.sh                        # build the warm snapshot for millisecond restore
scripts/build-control-plane.sh                   # build the Go control plane to vendor/control-plane (Stage 4)

# Minimal end-to-end smoke (Stage 4: start the control plane first; needs the vendor artifacts):
./vendor/control-plane &                         # owns the microVM fleet; the SDK talks to it over HTTP
python -c 'from microsandbox import Sandbox; s=Sandbox(); s.run_code("x=41"); print(s.run_code("print(x+1)").stdout); s.close()'
kill %1                                           # stop the control plane

# After editing in-VM code (server.py / backend.py), rebuild the rootfs (+ snapshot)
# so the VM picks up the change -- the rootfs bakes in a copy of src/ at build time:
scripts/build-rootfs.sh && scripts/build-snapshot.sh
```

## Working notes for Claude

- Before changing the isolation/transport layer, read `docs/ARCHITECTURE.md` to
  confirm the boundaries, then act.
- `server.py` and `backend.py` run **inside the VM**. The running `vendor/rootfs.ext4`
  contains a *copy* of `src/` taken at build time, so changes to in-VM code only
  take effect after `scripts/build-rootfs.sh` (and `build-snapshot.sh` for the
  snapshot path). Host-side changes (`client.py`) take effect immediately.
- **Cadence**: split work into independently verifiable sub-steps, keep tests
  green at every step, give an honest self-review (🔴/🟡/🟢) before committing, and
  commit only on the user's explicit go-ahead (English Conventional Commits,
  concise). **After every commit, push to `origin/main` immediately** (no separate
  ask needed).
```
