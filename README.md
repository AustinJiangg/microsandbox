# microsandbox

A **from-scratch, learning-oriented code sandbox that incrementally approaches
[E2B](https://github.com/e2b-dev/E2B)**.

The goal is not to ship a product, but to understand *how an AI code sandbox is
actually built*. The project evolves in stages, from the simplest host
subprocess all the way to a Firecracker microVM. **Stages 0–3 are done** (up to
microVM isolation with snapshot restore); Stage 4 (productization) is next.

## Quick start

```bash
pip install -e ".[dev]"
docker pull python:3.12-slim   # base image for the Stage 1 container backend (one-time)
python examples/quickstart.py
```

It feels like this (modeled on the E2B SDK):

```python
from microsandbox import Sandbox

with Sandbox() as sandbox:
    ex = sandbox.run_code("print('hello from the sandbox')")
    print(ex.stdout)        # hello from the sandbox
    print(ex.success)       # True

    # stream output as it arrives
    sandbox.run_code(
        "for i in range(3): print(i)",
        on_stdout=lambda chunk: print("live:", chunk.strip()),
    )

# Stage 1: switch to Docker container isolation in one line; run_code is unchanged
with Sandbox(backend="docker") as sandbox:
    ex = sandbox.run_code("import platform; print(platform.node())")
    print(ex.stdout)        # the container ID, not your hostname

# Stage 2b: a resident Jupyter kernel — variables persist across run_code (a real stateful REPL)
# build the agent image first: docker build -t microsandbox-agent .
with Sandbox(backend="kernel") as sandbox:
    sandbox.run_code("x = 41")
    print(sandbox.run_code("print(x + 1)").stdout)   # 42 — the 2nd call sees the 1st call's variable

# Stage 2c: file / shell API (modeled on E2B's sandbox.files / sandbox.commands)
with Sandbox(backend="container") as sandbox:
    sandbox.files.write("/tmp/data.txt", "42")        # the resident container is writable only under /tmp
    print(sandbox.files.read("/tmp/data.txt"))        # 42
    print(sandbox.commands.run("ls /tmp").stdout)     # data.txt

# Stage 3: a Firecracker microVM — strongest isolation (its own guest kernel + KVM boundary),
# control channel over vsock. Needs vendor/ artifacts first: scripts/build-rootfs.sh (one-time).
with Sandbox(backend="microvm") as sandbox:
    print(sandbox.run_code("print(1 + 1)").stdout)    # 2, from inside a real VM (~0.94s cold start)

# Stage 3c: snapshot restore — millisecond cold start (needs scripts/build-snapshot.sh)
with Sandbox(backend="microvm", from_snapshot=True) as sandbox:
    print(sandbox.run_code("print(6 * 7)").stdout)    # 42, ready in ~30ms with a warm kernel
```

## Project structure

```
microsandbox/
├── CLAUDE.md                  # Claude Code project memory (conventions, progress, pointers)
├── README.md
├── pyproject.toml
├── Dockerfile                 # Stage 2b agent image (Jupyter kernel runtime)
├── docs/
│   ├── ARCHITECTURE.md        # the three-layer design and cross-stage evolution strategy
│   ├── ROADMAP.md             # staged roadmap (goals and steps per stage)
│   ├── STAGE2_DESIGN.md       # Stage 2 design journal (resident agent + stateful REPL)
│   └── STAGE3_DESIGN.md       # Stage 3 design journal (microVM, vsock, snapshots)
├── src/microsandbox/
│   ├── protocol.py            # client↔daemon protocol (the most important stable boundary)
│   ├── client.py              # SDK: Sandbox / run_code / transports
│   ├── server.py              # daemon: HTTP + SSE (corresponds to E2B's envd)
│   └── backend.py             # execution backends (the isolation layer, swapped per stage)
├── scripts/
│   ├── build-rootfs.sh        # export an ext4 rootfs from the agent image (Stage 3b)
│   └── build-snapshot.sh      # build a warm Firecracker snapshot (Stage 3c)
├── examples/quickstart.py
└── tests/                     # parametrized end-to-end tests + isolation / microVM / snapshot tests
```

## Evolution path

| Stage | Isolation | Status |
|-------|-----------|--------|
| 0 | Host subprocess | ✅ |
| 1 | Docker container | ✅ |
| 2 | Resident in-container agent + stateful REPL + file/shell API | ✅ |
| 3 | Firecracker microVM (vsock transport, resource limits, snapshot restore) | ✅ |
| 4 | Productization (pooling / templates / auth) | ⬜ next |

See `docs/ROADMAP.md` for details.

## ⚠️ Safety note

The default `local` backend (host subprocess) has **almost no isolation**: code
can read your local files, network, and environment variables. The Stage 1
`docker` backend adds filesystem/network isolation and resource limits, but the
container **shares the host kernel and has a non-trivial escape surface**, so it
is still not enough to run fully untrusted code. The Stage 3 `microvm` backend
gives each sandbox **its own guest kernel behind a KVM boundary** — the first
isolation strong enough to seriously discuss untrusted code — but this is a
**learning implementation, not security-audited**. **This project is for local
learning only**; do not expose it as a service or feed it arbitrary input.
