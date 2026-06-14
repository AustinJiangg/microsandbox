# Architecture

This document explains the design of `microsandbox` and the boundaries that kept
it evolvable.

## Global data flow

```
   your program
      │  sandbox.run_code("print(1)")
      ▼
┌───────────────┐   HTTP POST /execute (over vsock)  ┌────────────────────────┐
│  client (SDK) │ ──────────────────────────────────▶│   daemon (in the VM)    │
│  client.py    │                                     │   server.py            │
│               │ ◀────────────────────────────────── │                        │
└───────────────┘   SSE stream: OutputEvent...        │   ┌──────────────────┐ │
      ▲                                                │   │ backend           │ │
      │ Execution(stdout/stderr/exit_code)             │   │ backend.py        │ │
   aggregated result                                   │   │ (Jupyter kernel)  │ │
                                                       │   └──────────────────┘ │
                                                       └────────────────────────┘
                                                         ↑ guest kernel + KVM boundary
```

The transport under that arrow is **vsock** (Firecracker's virtio-vsock,
multiplexed onto a host Unix-domain socket). The HTTP/SSE bytes flowing over it
are exactly what travelled over TCP in the project's earlier stages — only the
pipe changed. That stability is the whole point (see below).

## Responsibilities of the three components

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
`run_code()`, the file/shell namespaces, and streaming callbacks. It also **owns
the microVM lifecycle**:

- `_spawn_microvm` writes a declarative Firecracker config and starts the
  `firecracker` process (cold start, ~0.94s).
- `_restore_microvm` restores from a warm snapshot via Firecracker's REST API
  (~30ms to ready).
- `_VsockTransport` connects into the VM over Firecracker's vsock UDS and speaks a
  minimal hand-written HTTP/1.1 (the `CONNECT <port>` handshake, then the same
  HTTP/SSE the protocol always used).
- `close()` kills the firecracker process (destroying the VM) and cleans up the
  per-VM working directory.

### 3. server.py + backend.py — the daemon and the execution layer

- `server.py` (daemon): a resident process running **inside the VM**, listening on
  `AF_VSOCK`, handing requests to the backend, and streaming output back via SSE.
  Corresponds to E2B's `envd`. Apart from "which kind of socket to listen on", its
  request handling is transport-agnostic.
- `backend.py` (backend): where code actually runs. Decoupled via the abstract
  base class `ExecutionBackend`, with one implementation:

```
ExecutionBackend (abstract)
└── JupyterKernelBackend    # a resident Jupyter kernel inside the VM (stateful REPL)
```

A long-lived kernel holds a Python namespace, so variables persist across
`run_code` calls — exactly how E2B's code interpreter behaves.

**Two orthogonal axes.** It's worth separating them, because conflating them is
confusing:
- *Isolation / topology* — where the daemon runs and how the client connects.
  Here: a Firecracker microVM, reached over vsock. This is a **client/transport**
  concern.
- *Execution model* — how the daemon runs code once it has a request. Here: a
  stateful Jupyter kernel. This is the **backend** concern.

The microVM is therefore **not** a kind of backend; it's the isolation the client
creates, inside which the daemon happens to run the kernel backend.

### Transport (vsock)

`_VsockTransport` carries HTTP/SSE over Firecracker's vsock UDS. Firecracker
multiplexes the guest's vsock onto a single host Unix-domain socket; after a text
handshake (`CONNECT <port>` → `OK <hostport>`) the byte stream is wired to the
daemon listening on that vsock port inside the guest. `urllib` can't do that
handshake, so the client hand-writes a minimal HTTP/1.1 over the raw socket. The
protocol bytes are identical to what earlier stages carried over TCP; only the
pipe differs.

A side benefit over a TCP port mapping: vsock is orthogonal to "does the guest
have a NIC", so the VM can be **fully offline** (no virtio-net at all) yet still
manageable over vsock.

### The microVM's isolation mechanisms

How `client._spawn_microvm` configures isolation, and what each piece buys:

| config | mechanism | what it gives |
|--------|-----------|---------------|
| `firecracker` + `/dev/kvm` | hardware virtualization (KVM) | the guest runs its **own Linux kernel**; escaping means breaking the guest kernel *and then* KVM |
| `boot_args: ... root=/dev/vda ro` | read-only ext4 root | the root filesystem can't be tampered with |
| `/init` mounts `tmpfs /tmp` | ramdisk | the only writable area (matches the read-only root) |
| `machine-config: vcpu_count / mem_size_mib` | VM resource quota | the guest sees a whole machine with 1 vCPU / ~512MB — a *VM* quota, not a cgroup quota |
| no `network-interfaces` in the config | no virtio-net | the sandbox code is fully offline; management still flows over vsock |

A lifecycle detail: killing the `firecracker` process destroys the entire VM (its
memory and device state vanish with the process), so cleanup is just
`proc.terminate()` + remove the working directory — much simpler than tearing down
a container.

## How it evolved (preserved in git history)

The project was built up in stages, each adding one isolation technique on top of
the same protocol: host subprocess → one-shot Docker container → resident
in-container agent (the "ownership inversion") → stateful Jupyter kernel →
Firecracker microVM (TCP → vsock). Every step followed one discipline: **add a
new backend/transport implementation, keep the protocol byte-stable, and keep the
changes out of the client as much as possible.** Once the microVM landed, the
earlier backends were removed as scaffolding — the staged code lives on in the git
history if you want to study the progression.

## Mapping to E2B (read its source alongside, once you're done)

| This project | E2B equivalent | Notes |
|--------------|----------------|-------|
| client.py | E2B SDK (python/js) | user interface + sandbox lifecycle |
| protocol.py | envd's gRPC/HTTP protocol | communication contract |
| server.py (daemon) | `envd` | resident agent inside the sandbox |
| backend.py | the in-sandbox code interpreter | the stateful kernel that runs code |
| (firecracker config in client.py) | Firecracker orchestration / jailer | microVM creation and isolation |
| a future control plane | orchestrator / API | sandbox pooling & lifecycle management |
