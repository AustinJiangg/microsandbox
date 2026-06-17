# microsandbox

A **from-scratch, learning-oriented code sandbox** modeled on
[E2B](https://github.com/e2b-dev/E2B). The goal is not to ship a product, but to
understand *how an AI code sandbox is actually built*.

Every `Sandbox` is a **Firecracker microVM**: its own guest Linux kernel behind
the KVM hardware-virtualization boundary (the strongest isolation), with the
control channel carried over **vsock** and a stateful Jupyter kernel running
inside the VM (variables persist across `run_code`, like E2B).

> **History.** This project was built up in stages — host subprocess → Docker
> container → resident in-container agent → microVM — as a way to understand each
> isolation technique. Those earlier backends were learning scaffolding; now that
> the microVM works they have been removed, leaving only the Firecracker path. The
> full staged journey is preserved in the git history (tag `archive/stages-0-3`).

## Quick start

One-time setup (see `docs/MICROVM_DESIGN.md` §7 for details):

```bash
pip install -e ".[dev]"

# 1) Let your user open /dev/kvm without sudo, then restart WSL to apply the group.
sudo usermod -aG kvm "$USER"        # then: wsl --shutdown (from Windows), reopen the terminal

# 2) Put the firecracker binary + a vsock-capable guest kernel under vendor/
#    (vendor/firecracker, vendor/vmlinux — see the design doc for sources).

# 3) Build the VM rootfs (docker is only a one-time build tool here) and a warm snapshot.
docker build -t microsandbox-agent .   # the agent image the rootfs is exported from
scripts/build-rootfs.sh                # export an ext4 rootfs from that image (no root needed)
scripts/build-snapshot.sh              # optional: a warm snapshot for millisecond restore

# 4) Build and start the Go host services (Stage 8): the orchestrator owns the microVM
#    fleet (gRPC SandboxService), the api is the REST front the SDK talks to. dev-up
#    builds + runs both; the SDK's base_url is http://127.0.0.1:8080. Leave it running.
scripts/dev-up.sh &

python examples/quickstart.py
```

Usage feels like E2B:

```python
from microsandbox import Sandbox

# (Start the services first: scripts/dev-up.sh -- the orchestrator owns the VM lifecycle, the api is the REST front.)
# Cold start a microVM (~0.94s: firecracker + guest kernel boot + daemon on vsock).
with Sandbox() as sandbox:
    ex = sandbox.run_code("print('hello from the microVM')")
    print(ex.stdout)        # hello from the microVM
    print(ex.success)       # True

    # The in-VM kernel is stateful: variables persist across run_code.
    sandbox.run_code("x = 41")
    print(sandbox.run_code("print(x + 1)").stdout)   # 42

    # Stream output as it arrives.
    sandbox.run_code(
        "for i in range(3): print(i)",
        on_stdout=lambda chunk: print("live:", chunk.strip()),
    )

    # File / shell API (modeled on E2B's sandbox.files / sandbox.commands).
    sandbox.files.write("/tmp/data.txt", "42")    # the VM root is read-only; only /tmp is writable
    print(sandbox.files.read("/tmp/data.txt"))    # 42
    print(sandbox.commands.run("ls /tmp").stdout) # data.txt

# Restore from a warm snapshot instead: ready in ~30ms (skips kernel boot + kernel cold start).
with Sandbox(from_snapshot=True) as sandbox:
    print(sandbox.run_code("print(6 * 7)").stdout)    # 42

# Boot a custom image (Stage 6 templates): build a template once, then select it by name.
#   scripts/build-template.sh example   # templates/example/Dockerfile -> vendor/templates/example/
# (swap the marker line in that Dockerfile for `RUN pip install ...` to make a real env.)
with Sandbox(template="example") as sandbox:
    print(sandbox.files.read("/etc/microsandbox-template"))   # hello from the example template
```

## Project structure

```
microsandbox/
├── CLAUDE.md                  # Claude Code project memory (conventions, pointers)
├── README.md
├── pyproject.toml
├── Dockerfile                 # the agent image (Jupyter kernel runtime) the rootfs is exported from
├── docs/
│   ├── ARCHITECTURE.md        # the three-layer design (client / protocol / daemon+backend)
│   ├── MICROVM_DESIGN.md      # the microVM design (Firecracker, vsock, snapshots)
│   ├── STAGE4_DESIGN.md       # Stage 4: extracting the Go control plane
│   ├── STAGE5_DESIGN.md       # Stage 5: the warm pool
│   ├── STAGE6_DESIGN.md       # Stage 6: named templates (custom images)
│   ├── STAGE7_DESIGN.md       # Stage 7: the Go in-VM daemon (envd)
│   ├── STAGE8_DESIGN.md       # Stage 8: split the control plane into api + orchestrator (gRPC)
│   └── E2B_ALIGNMENT_ROADMAP.md  # the post-Stage-7 roadmap toward E2B's component architecture
├── src/microsandbox/
│   ├── protocol.py            # client↔daemon wire protocol (the stable boundary)
│   ├── client.py              # SDK: Sandbox / run_code -- a thin pure-HTTP client to the api
│   ├── server.py              # the retired Python in-VM daemon (Stage 7 replaced it; kept as reference)
│   └── backend.py             # the retired Python kernel backend (reference)
├── daemon/                    # the Go in-VM daemon (Stage 7, E2B's envd): vsock HTTP/SSE; drives the kernel via a Jupyter gateway
├── services/                  # the Go host control plane (Stage 8, E2B's "infra"), module microsandbox/services
│   ├── cmd/api/               #   public REST front + SQLite metadata store; calls the orchestrator over gRPC
│   ├── cmd/orchestrator/      #   owns the microVM fleet + warm pool (gRPC SandboxService) + the vsock data proxy
│   ├── pkg/                   #   fc / pool / proxy / template / store, + grpc/ (generated stubs)
│   └── proto/                 #   the gRPC contract (orchestrator.proto)
├── scripts/
│   ├── build-rootfs.sh        # export an ext4 rootfs from the agent image (no root needed)
│   ├── build-snapshot.sh      # build a warm Firecracker snapshot for millisecond restore
│   ├── build-template.sh      # build a named custom image (Stage 6): Dockerfile -> rootfs (+ snapshot)
│   ├── build-services.sh      # build the Go host services (api + orchestrator) to vendor/
│   ├── gen-proto.sh           # regenerate the gRPC stubs from services/proto (needs protoc)
│   └── dev-up.sh              # build + run orchestrator + api locally (SDK base_url = http://127.0.0.1:8080)
├── templates/                 # template recipes (Stage 6): templates/<name>/Dockerfile (built artifacts -> vendor/templates/)
├── examples/quickstart.py
└── tests/                     # end-to-end / stateful / snapshot / metadata tests on real VMs (host-side unit tests are in services/)
```

## How it works (one paragraph)

The SDK (`client.py`) asks the **api** (`services/cmd/api`, Go) for a sandbox over
HTTP; the api calls the **orchestrator** (`services/cmd/orchestrator`) over **gRPC**,
which writes a declarative Firecracker config and starts the `firecracker` process — a
microVM with its own guest kernel and an ext4 rootfs — and records the sandbox in the
api's SQLite metadata store. Inside the VM, PID 1 (`/init`) execs the **Go daemon**
(`daemon/`, E2B's `envd`), which listens on **vsock**. The SDK then POSTs
`/sandboxes/{id}/execute` to the api, which (for now) reverse-proxies it to the
orchestrator's data proxy, which bridges it to the VM over Firecracker's vsock
Unix-domain socket (a `CONNECT <port>` handshake, then plain HTTP/SSE) and streams the
response straight back; the daemon drives a long-lived **Python Jupyter kernel** via a
Jupyter Kernel Gateway. The SDK itself is pure HTTP. The wire protocol (`protocol.py`)
is the stable boundary — it never changed as the isolation evolved from subprocess to
microVM to a control-plane split, a Go daemon, and the api/orchestrator gRPC split. See
`docs/ARCHITECTURE.md`, `docs/E2B_ALIGNMENT_ROADMAP.md` and the stage design docs
(`docs/STAGE4`–`STAGE8_DESIGN.md`).

## ⚠️ Safety note

The microVM gives each sandbox **its own guest kernel behind a KVM boundary** —
the first isolation in the project strong enough to *seriously discuss* untrusted
code — but this is a **learning implementation, not security-audited**. Real
defense-in-depth (jailer, seccomp-bpf, network policy, rate limiting, escape
monitoring) is out of scope. **This project is for local learning only**; do not
expose it as a service or feed it arbitrary external input.
