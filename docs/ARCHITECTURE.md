# Architecture

This document explains the design of `microsandbox` and why it can support
"approaching E2B in stages".

## Global data flow

```
   your program
      │  sandbox.run_code("print(1)")
      ▼
┌───────────────┐   HTTP POST /execute        ┌────────────────────────┐
│  client (SDK) │ ───────────────────────────▶│   daemon                │
│  client.py    │                              │   server.py            │
│               │ ◀─────────────────────────── │                        │
└───────────────┘   SSE stream: OutputEvent... │   ┌──────────────────┐ │
      ▲                                         │   │ backend           │ │
      │ Execution(stdout/stderr/exit_code)      │   │ backend.py        │ │
   aggregated result                            │   └──────────────────┘ │
                                                └────────────────────────┘
                                                  ↑ the isolation boundary
                                                    hardens here, stage by stage
```

The transport under that arrow starts as TCP and becomes vsock at Stage 3, but
the HTTP/SSE bytes flowing over it stay the same (see "Transport abstraction").

## Responsibilities of the three components

### 1. protocol.py — the contract (most important)

Defines what travels between client and daemon:
- `ExecuteRequest`: the code to run, the language, the timeout.
- `OutputEvent`: one streamed output (stdout / stderr / error / end).
- `Execution`: the final result object the client builds by aggregating events.

**Why it's pulled out separately**: the isolation layer is swapped several times
across Stages 0→3, but as long as this protocol stays stable, the client barely
changes. This is the pivot that lets the whole project evolve incrementally. E2B
likewise decouples its SDK from the underlying runtime via a stable client↔envd
protocol.

### 2. client.py — the SDK

The only layer a user touches directly. Provides the `Sandbox` class,
`run_code()`, and streaming callbacks.

- Stage 0/1: `Sandbox` spawns a daemon subprocess locally (`spawn_local`). Stage 1
  just adds a `Sandbox(backend="docker")` pass-through switch; the client itself
  knows nothing about docker.
- Stage 2+: the spawn logic becomes "ask for a sandbox (container / VM), get its
  address, then connect to it" — but the `run_code` API stays the same for users.
- Stage 3 adds a **transport abstraction** (below): the client also owns the VM
  lifecycle (`_spawn_microvm` / `_restore_microvm`), mirroring how Stage 2 owns
  the resident container.

### 3. server.py + backend.py — the daemon and the isolation layer

- `server.py` (daemon): a resident process running **inside** the sandbox,
  listening over HTTP, handing requests to the backend, and streaming output back
  via SSE. Corresponds to E2B's `envd`. It listens on TCP, or on `AF_VSOCK` when
  `--transport vsock` (Stage 3, inside the microVM).
- `backend.py` (backend): where code actually runs — **where isolation strength
  lives**. Decoupled via the abstract base class `ExecutionBackend`:

```
ExecutionBackend (abstract)
├── LocalSubprocessBackend    # Stage 0: a host subprocess (also reused inside containers/VMs)
├── DockerBackend             # Stage 1: one throwaway container per execution
└── JupyterKernelBackend      # Stage 2b: a resident Jupyter kernel (stateful REPL)
```

Note: the Stage 3 microVM is **not** a new backend. Strong isolation comes from
*where the daemon runs* (host → resident container → microVM) and *how the client
connects* (TCP → vsock), which are client/server concerns. Inside the VM, the
daemon reuses an existing backend (`LocalSubprocessBackend` or, by default,
`JupyterKernelBackend`).

### Transport abstraction (new in Stage 3)

Stage 3 factors out a `Transport` in the client/server so that "what to say" (the
protocol) is decoupled from "how to connect":

- `_TcpTransport` — HTTP over TCP (Stages 0–2), wrapping the existing `urllib`
  path with byte-for-byte identical behavior.
- `_VsockTransport` — HTTP over vsock for the microVM: it connects Firecracker's
  vsock Unix-domain socket, does the `CONNECT <port>` handshake, then speaks a
  minimal hand-written HTTP/1.1 over the raw socket.

The protocol bytes are identical on both; only the pipe differs.

### DockerBackend's isolation mechanisms (Stage 1)

Each `docker run` flag maps to a kernel isolation mechanism:

| flag | kernel mechanism | what it blocks |
|------|------------------|----------------|
| (the container itself) | mount namespace | the container has its own root filesystem; host files are invisible |
| `--network none` | network namespace | no NIC and no routes — fully offline |
| `--memory` / `--cpus` | cgroups | memory/CPU caps; over-memory gets OOM-killed (exit 137) |
| `--pids-limit` | pids cgroup | fork bombs |
| `--read-only` + `--tmpfs /tmp` | read-only mount + ramdisk | tampering with the container filesystem (only /tmp stays writable) |

A key lifecycle detail: **killing the `docker run` client process does not kill
the container** — it is just the "remote control" connected to the docker daemon;
the container process is owned by the daemon. So timeout cleanup must go through
`docker rm -f <name>`, with an idempotent fallback in `finally` (the happy path's
`--rm` already removes it).

Note: the container **shares the host kernel**, so the escape surface is still
non-trivial — Stage 1 isolation is not enough to run fully untrusted code;
kernel-level strong isolation waits for the Stage 3 microVM.

## Cross-stage evolution strategy

| Stage | Isolation layer | Main files changed | Does the client change? |
|-------|-----------------|--------------------|--------------------------|
| 0 | Host subprocess | backend.py | — |
| 1 | Docker container | add DockerBackend (daemon still on host) | only a `backend=` pass-through |
| 2 | Resident in-container agent + stateful REPL | daemon packaged into the image (envd-ification); backend uses a persistent kernel | connection address becomes the container |
| 3 | Firecracker microVM | add a Transport abstraction (TCP→vsock) + client owns the VM lifecycle; in-VM daemon reuses an existing backend | connection becomes vsock; client boots/restores the VM |
| 4 | Productization | add a control plane, pooling, auth | add a create/connect flow |

Every step follows the same discipline: **add a new backend/transport
implementation, keep the protocol stable, and keep the changes out of the client
as much as possible.** `tests/` is the safety net and must stay all green after
each step.

## Mapping to E2B (read its source alongside, once you're done)

| This project | E2B equivalent | Notes |
|--------------|----------------|-------|
| client.py | E2B SDK (python/js) | user interface |
| protocol.py | envd's gRPC/HTTP protocol | communication contract |
| server.py (daemon) | `envd` | resident agent inside the sandbox |
| backend.py | Firecracker orchestration + sandbox runtime | isolation and execution |
| Stage 4 control plane | orchestrator / API | sandbox lifecycle management |
