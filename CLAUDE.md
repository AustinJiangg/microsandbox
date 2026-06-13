# CLAUDE.md

> This file is **Claude Code's project memory**. It is loaded automatically at the
> start of every session in this repo. Keep the project's long-term conventions,
> architectural decisions, and current progress here — not scattered across chats.

## What this project is

`microsandbox` is a **learning-oriented** code-execution sandbox whose goal is to
approach the core implementation of [E2B](https://github.com/e2b-dev/E2B) from
scratch, stage by stage. The point is to **understand the principles**, not to
ship a product. The code aims to be clear, well-commented, and easy to evolve
incrementally.

## Core architecture (keep it stable)

Three decoupled layers — see `docs/ARCHITECTURE.md` for the full design:

1. **client (SDK)** — `src/microsandbox/client.py`. What the user faces:
   `Sandbox().run_code(...)`. Modeled on the E2B SDK.
2. **protocol (wire protocol)** — `src/microsandbox/protocol.py`. The contract
   between client and daemon. **This is the most important boundary; keep it
   backward-compatible as it evolves.**
3. **daemon + backend** — `server.py` / `backend.py`. The daemon listens inside
   the sandbox; the backend is the isolation layer that actually runs the code.

**Key principle**: when swapping isolation (subprocess → container → microVM), add
a new `ExecutionBackend`/transport implementation and swap the daemon's default —
**keep the client and protocol as untouched as possible**.

## Roadmap (progress lives in `docs/ROADMAP.md`)

- Stages 0–3 are **done**: host subprocess → Docker container → resident
  in-container agent + stateful REPL (Stage 2) → Firecracker microVM with vsock
  transport, resource limits, and snapshot restore (Stage 3). See
  `docs/STAGE2_DESIGN.md` and `docs/STAGE3_DESIGN.md` for the per-stage designs.
- **Stage 4 (productization: sandbox pool, templates, auth, …) is next** — not
  started yet.

## Development conventions

- Python ≥ 3.11. Stages 0/1 have **zero runtime dependencies** (stdlib only). New
  dependencies are introduced only in the stage that needs them, with a stated
  reason (Stage 2b first added `ipykernel` + `jupyter_client` as a `[kernel]`
  extra, lazily imported; Stage 3 added no Python deps — it shells out to the
  `firecracker` binary like Stage 1 shells out to `docker`).
- **Language: English only.** All docs, code comments, docstrings, and commit
  messages are in English. Comments explain **why**, not what — especially noting
  "which future stage will replace this".
- Every stage must keep `tests/` all green. The tests are the safety net for
  cross-stage refactors.
- **Safety rule**: the isolation in Stages 0/1/2 is not enough to run untrusted
  code; **never** imply in docs or code that they can accept arbitrary external
  input. Strong (kernel-level) isolation starts at Stage 3 (microVM), but even
  that is a learning implementation, not security-audited.

## Common commands

```bash
pip install -e ".[dev]"                          # install (dev mode)
python examples/quickstart.py                    # run the example (auto start/stop daemon)
pytest                                           # run tests (auto-parametrized; skips backends whose deps are missing)
pytest tests/test_sandbox.py::test_timeout -v    # run a single test

docker pull python:3.12-slim                     # Stage 1 prerequisite (one-time)
docker build -t microsandbox-agent .             # Stage 2b prerequisite: build the agent image (one-time)

# Stage 3 prerequisites (one-time): firecracker binary + kernel in vendor/, then build rootfs/snapshot
scripts/build-rootfs.sh                           # export the ext4 rootfs from the agent image
scripts/build-snapshot.sh                         # build the warm snapshot for millisecond restore

# Stage 2b: stateful REPL (variables persist across run_code; needs the agent image)
python -c 'from microsandbox import Sandbox; s=Sandbox(backend="kernel"); s.run_code("x=41"); print(s.run_code("print(x+1)").stdout); s.close()'

# Clean up stray containers (normally none; only after a kill -9 leaves residue):
docker ps -a --filter name=microsandbox- -q | xargs -r docker rm -f
```

## Working notes for Claude

- Before changing the isolation layer, read `docs/ARCHITECTURE.md` to confirm the
  boundaries, then act.
- When entering a new stage, update `docs/ROADMAP.md` checkboxes first and sync
  the "You are here" marker at the top of the roadmap / this file.
- When adding a new isolation backend, follow the `ExecutionBackend` abstraction;
  don't leak isolation details into the client.
- **Cadence**: split each stage into independently verifiable sub-steps, keep
  tests green at every step, give an honest self-review (🔴/🟡/🟢) before
  committing, and commit only on the user's explicit go-ahead (one commit per
  stage, English Conventional Commits, concise). **After every commit, push to
  `origin/main` immediately** (no separate ask needed).
