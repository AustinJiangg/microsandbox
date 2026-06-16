# Architecture

This document explains the design of `microsandbox` and the boundaries that kept
it evolvable.

## Global data flow

```
   your program
      │  sandbox.run_code("print(1)")
      ▼
┌───────────────┐ ① HTTP/TCP        ┌────────────────────────┐ ② HTTP/SSE over vsock ┌────────────────────────┐
│  client (SDK) │ ─────────────────▶│  control plane (Go)     │ ────────────────────▶ │   daemon (in the VM)    │
│  client.py    │  POST .../{id}/... │  control-plane/         │                       │   server.py            │
│  (pure HTTP)  │ ◀─────────────────│  • owns the microVM      │ ◀──────────────────── │   ┌──────────────────┐ │
└───────────────┘   SSE forwarded    │    (microvm.go)         │   SSE                 │   │ backend           │ │
      ▲                              │  • vsock bridge          │                       │   │ backend.py        │ │
      │ Execution(stdout/stderr/...)  │    (proxy.go)           │                       │   │ (Jupyter kernel)  │ │
   aggregated result                 └────────────────────────┘                        │   └──────────────────┘ │
                                                                                        └────────────────────────┘
                                                                                          ↑ guest kernel + KVM boundary
```

As of Stage 4 the SDK is a thin **pure-HTTP** client (① over TCP). It drives the
**control plane** (`control-plane/`, Go), which owns the microVM and transparently
proxies each request to the in-VM daemon over **vsock** (②, Firecracker's
virtio-vsock multiplexed onto a host Unix-domain socket). The HTTP/SSE bytes on the
vsock leg are exactly what travelled over TCP in the project's earlier stages — only
the pipe changed. That byte-stable protocol is the whole point (see below); see
`docs/STAGE4_DESIGN.md` for the split. As of **Stage 7** the in-VM daemon (the right
box) is a **Go binary** (envd-equivalent), not the Python `server.py`; it drives a
Python kernel via a Jupyter Kernel Gateway — see `docs/STAGE7_DESIGN.md`.

## Responsibilities of the components

### 1. protocol.py — the contract (most important)

Defines what travels between client and daemon:
- `ExecuteRequest`: the code to run, the language, the timeout.
- `OutputEvent`: one streamed output (stdout / stderr / error / end).
- `Execution`: the final result object the client builds by aggregating events.

**Why it's pulled out separately**: the isolation layer was swapped several times
while the project grew (subprocess → container → microVM), but because this
protocol stayed byte-stable, the client barely changed. This is the pivot that
let the project evolve incrementally. E2B likewise decouples its SDK from the
underlying runtime via a stable client↔envd protocol.

### 2. client.py — the SDK

The only layer a user touches directly. Provides the `Sandbox` class,
`run_code()`, the file/shell namespaces, and streaming callbacks. As of Stage 4 it
is a thin **pure-HTTP** client and no longer creates the VM itself:

- on construction it asks the control plane to spawn/restore a VM
  (`POST /sandboxes`), which returns only once the VM is healthy.
- `run_code` / files / commands POST through the control plane
  (`/sandboxes/{id}/execute`, `/files/*`, `/commands`); the `/execute` SSE streams
  straight back.
- `close()` asks the control plane to destroy the VM (`DELETE /sandboxes/{id}`).

It holds no vsock or firecracker code anymore — that moved to the control plane (4).

### 3. the in-VM daemon (`daemon/`, Go) + the kernel

As of **Stage 7** the in-VM daemon is a **Go binary** (`daemon/`, built static and
baked into the rootfs), matching E2B's `envd`. It runs **inside the VM**, listens on
`AF_VSOCK`, serves the same HTTP/SSE protocol, and streams output back — byte-for-byte
what the Python daemon did (the existing e2e suite is the parity proof). It replaced
`src/microsandbox/server.py` + `backend.py`, which stay in `src/` as the reference the
port was built from. See `docs/STAGE7_DESIGN.md`.

How it runs code: the daemon does **not** run Python itself. Like envd, it launches a
stateful Python kernel as a child and drives it over a **Jupyter Kernel Gateway**'s
HTTP + WebSocket kernels API (`POST /api/kernels`, then the `channels` WebSocket),
translating the kernel's iopub messages (stream / execute_result / error / status)
into our `OutputEvent`s. A long-lived kernel holds a Python namespace, so variables
persist across `run_code` calls — exactly how E2B's code interpreter behaves. (The
files/commands endpoints are plain Go: the daemon's own filesystem and `sh -c`.)

**Two orthogonal axes.** It's worth separating them, because conflating them is
confusing:
- *Isolation / topology* — where the daemon runs and how the client connects.
  Here: a Firecracker microVM, reached over vsock. This is a **client/transport**
  concern.
- *Execution model* — how the daemon runs code once it has a request. Here: a
  stateful Jupyter kernel. This is the **backend** concern.

The microVM is therefore **not** a kind of backend; it's the isolation the control
plane creates, inside which the daemon happens to run the kernel backend.

### 4. control plane — `control-plane/` (Go)

New in Stage 4. A standalone HTTP service that owns the microVM fleet, built to
`vendor/control-plane`:

- `microvm.go` — spawn (cold) / restore (snapshot) / destroy firecracker
  (`POST`/`DELETE /sandboxes`), ported from what the SDK used to do.
- `proxy.go` — a transparent reverse proxy: `ANY /sandboxes/{id}/<rest>` is bridged
  to the in-VM daemon at `/<rest>` over vsock. It pipes bytes, so it stays
  protocol-agnostic and `protocol.py` remains the single source of truth. It also
  runs the `/health` probe, so a sandbox is healthy by the time `POST /sandboxes`
  returns ("ready on delivery").

Corresponds to E2B's `infra` (orchestrator + edge). Keeping it separate is what lets
the SDK be a thin client that could be re-implemented in any language.

### Transport (vsock)

The **control plane** (`control-plane/proxy.go`) carries HTTP/SSE over Firecracker's
vsock UDS. Firecracker multiplexes the guest's vsock onto a single host Unix-domain
socket; after a text handshake (`CONNECT <port>` → `OK <hostport>`) the byte stream
is wired to the daemon listening on that vsock port inside the guest. Go's
`net/http` can't dial that raw handshake, so the control plane hand-rolls it in a
`RoundTripper` wrapped by a reverse proxy. The protocol bytes are identical to what
earlier stages carried over TCP; only the pipe differs. (Before Stage 4 this lived
in the SDK as `_VsockTransport`; it moved here so the SDK could become pure HTTP.)

A side benefit over a TCP port mapping: vsock is orthogonal to "does the guest
have a NIC", so the VM can be **fully offline** (no virtio-net at all) yet still
manageable over vsock.

### The microVM's isolation mechanisms

How the control plane (`microvm.go`) configures isolation, and what each piece buys:

| config | mechanism | what it gives |
|--------|-----------|---------------|
| `firecracker` + `/dev/kvm` | hardware virtualization (KVM) | the guest runs its **own Linux kernel**; escaping means breaking the guest kernel *and then* KVM |
| `boot_args: ... root=/dev/vda ro` | read-only ext4 root | the root filesystem can't be tampered with |
| `/init` mounts `tmpfs /tmp` | ramdisk | the only writable area (matches the read-only root) |
| `machine-config: vcpu_count / mem_size_mib` | VM resource quota | the guest sees a whole machine with 1 vCPU / ~512MB — a *VM* quota, not a cgroup quota |
| no `network-interfaces` in the config | no virtio-net | the sandbox code is fully offline; management still flows over vsock |

A lifecycle detail: killing the `firecracker` process destroys the entire VM (its
memory and device state vanish with the process), so the control plane's `destroy`
is just terminate-the-process + remove the working directory — much simpler than
tearing down a container.

## How it evolved (preserved in git history)

The project was built up in stages, each adding one isolation technique on top of
the same protocol: host subprocess → one-shot Docker container → resident
in-container agent (the "ownership inversion") → stateful Jupyter kernel →
Firecracker microVM (TCP → vsock) → a Go control-plane split (the SDK became a thin
HTTP client) → a Go in-VM daemon (envd-equivalent, driving the kernel via a Jupyter
gateway). Every step followed one discipline: **add a new backend/transport
implementation, keep the protocol byte-stable, and keep the changes out of the
client as much as possible.** Once the microVM landed, the earlier backends were
removed as scaffolding — the staged code lives on in the git history if you want to
study the progression.

## Mapping to E2B (read its source alongside, once you're done)

| This project | E2B equivalent | Notes |
|--------------|----------------|-------|
| client.py | E2B SDK (python/js) | user interface; a thin pure-HTTP client |
| protocol.py | envd's gRPC/HTTP protocol | communication contract |
| daemon/ (Go) | `envd` | resident agent inside the sandbox (Stage 7; was server.py) |
| Jupyter Kernel Gateway + kernel | the in-sandbox code interpreter | the stateful Python kernel the Go daemon drives over HTTP/WS |
| control-plane/ (Go) | `infra` (orchestrator + edge) | owns the VM fleet: spawn/restore/destroy + vsock proxy + health |
| (firecracker config in microvm.go) | Firecracker orchestration / jailer | microVM creation and isolation |
