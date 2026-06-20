# Architecture

This document explains the design of `microsandbox` and the boundaries that kept
it evolvable.

## Global data flow

```
   your program ── sandbox.run_code(...) ──▶ client (SDK, client.py, pure HTTP)
        │                                          │
   lifecycle:                                 data: POST /execute, /files/*, /commands
   POST/DELETE/GET /sandboxes                 (X-Sandbox-Id header)
        ▼                                          ▼
   api (services/cmd/api)                     client-proxy (services/cmd/client-proxy)
   • REST front, lifecycle-only               • edge data proxy; owns the routing
   • SQLite metadata store (pkg/store)          catalog (pkg/catalog)
   • on create: gRPC Create, then registers   • X-Sandbox-Id → catalog → that node's
     the sandbox's route in the catalog         data proxy
        │                                          │
        │ gRPC SandboxService                      │ HTTP/SSE (keeps X-Sandbox-Id)
        └──────────────▶ orchestrator ◀────────────┘
                         (services/cmd/orchestrator)
                         • owns the microVM (pkg/fc) + warm pool (pkg/pool)
                         • header-routed vsock data proxy (pkg/proxy)
                                │ HTTP/SSE over vsock
                                ▼
                         daemon (daemon/, Go envd) → stateful Jupyter kernel
                                ↑ guest kernel + KVM boundary
```

The SDK is a thin **pure-HTTP** client. It sends **lifecycle** to the **api** and the
**data path** to **client-proxy** (it learns that URL from the create response). Stage 8
split the old single control plane into the **api** (`services/cmd/api`, public REST +
SQLite metadata store) calling the per-machine **orchestrator**
(`services/cmd/orchestrator`) over **gRPC** for the VM lifecycle. **Stage 9** added the
**client-proxy** (`services/cmd/client-proxy`), E2B's edge data proxy: it owns a routing
**catalog** (`pkg/catalog`, sandbox → node) that the api writes on create, and routes each
data request by its `X-Sandbox-Id` header to the right orchestrator's vsock data proxy,
which bridges to the in-VM daemon. The api is now **lifecycle-only** — the data bytes never
pass through it. As of **Stage 11** the in-VM daemon is **two ConnectRPC services** (E2B's
`envd` + a separate `code-interpreter`) on two vsock ports; the data leg is the Connect
protocol over HTTP/1.1, riding the same vsock bridge the earlier HTTP/SSE did. Through Stage
10 that wire was byte-stable (the project's defining discipline); Stage 11 deliberately moved
it to ConnectRPC, so the e2e suite is now a **behavioral** parity oracle (see below). See
`docs/STAGE11_DESIGN.md` / `docs/STAGE9_DESIGN.md` / `docs/STAGE8_DESIGN.md` and
`docs/E2B_ALIGNMENT_ROADMAP.md`.

## Responsibilities of the components

### 1. the client↔daemon contract: `protocol.py` (history) → ConnectRPC (now)

Through Stage 10 `protocol.py` *was* the wire: `ExecuteRequest` / `OutputEvent` /
`Execution`, streamed as SSE. **Why it mattered**: the isolation layer was swapped many
times (subprocess → container → microVM), but because this protocol stayed **byte-stable**,
the client barely changed — the pivot that let the project evolve incrementally, with a
byte-for-byte e2e parity oracle. E2B likewise decouples its SDK from the runtime via a stable
client↔envd protocol.

**Stage 11 deliberately ended the byte-stable discipline**: the wire is now **ConnectRPC**
(`src/microsandbox/connect.py` hand-rolls a Connect-JSON client; `daemon/proto/*.proto`
defines the services), so the e2e suite became a **behavioral** parity oracle. `protocol.py`
remains as the SDK's result types (`Execution` / `OutputEvent` / `EventType`) and as
reference for the retired SSE wire.

### 2. client.py — the SDK

The only layer a user touches directly. Provides the `Sandbox` class,
`run_code()`, the file/shell namespaces, and streaming callbacks. As of Stage 4 it
is a thin **pure-HTTP** client and no longer creates the VM itself; Stage 9 split its
two faces:

- on construction it asks the **api** to spawn/restore a VM (`POST /sandboxes`), which
  returns only once the VM is healthy, plus the `data_url` to reach it.
- `run_code` / files / commands speak **ConnectRPC** to **client-proxy** (`data_url`) with an
  `X-Sandbox-Id` header (Stage 11): `run_code` is the code-interpreter's server-streaming
  `Execute`; files/commands are envd's unary `Filesystem` / `Process` RPCs.
- `close()` asks the **api** to destroy the VM (`DELETE /sandboxes/{id}`).

It holds no vsock or firecracker code anymore — that lives in the orchestrator (4).

### 3. the in-VM daemon (`daemon/`, Go) + the kernel

A static **Go binary** (`daemon/`, baked into the rootfs), matching E2B's `envd`. It runs
**inside the VM** on **two vsock ports** (Stage 11):

- **`envd`** — ConnectRPC `FilesystemService` (read/write/list on the VM's own filesystem)
  + `ProcessService` (run a command via `sh -c`) + a plain `GET /health`.
- **`code-interpreter`** — a ConnectRPC server-streaming `Execute` that drives the kernel.

Firecracker multiplexes both onto the one vsock UDS (`CONNECT <port>`); the orchestrator
routes `/codeinterpreter.*` to the second port. It replaced `src/microsandbox/server.py` +
`backend.py` (kept in `src/` as reference). See `docs/STAGE7_DESIGN.md` (the Go rewrite) and
`docs/STAGE11_DESIGN.md` (the ConnectRPC split).

How it runs code: the code-interpreter does **not** run Python itself. Like E2B, it launches
a stateful Python kernel as a child and drives it over a **Jupyter Kernel Gateway**'s HTTP +
WebSocket kernels API (`POST /api/kernels`, then the `channels` WebSocket), translating the
kernel's iopub messages (stream / execute_result / error / status) into the stream of
`OutputEvent`s. A long-lived kernel holds a Python namespace, so variables persist across
`run_code` calls — exactly how E2B's code interpreter behaves. (envd's Filesystem / Process
are plain Go: the daemon's own filesystem and `sh -c`.)

**Two orthogonal axes.** It's worth separating them, because conflating them is
confusing:
- *Isolation / topology* — where the daemon runs and how the client connects.
  Here: a Firecracker microVM, reached over vsock. This is a **client/transport**
  concern.
- *Execution model* — how the daemon runs code once it has a request. Here: a
  stateful Jupyter kernel. This is the **backend** concern.

The microVM is therefore **not** a kind of backend; it's the isolation the
orchestrator creates, inside which the daemon happens to run the kernel backend.

### 4. control plane — `services/` (Go): api + client-proxy + orchestrator

New in Stage 4 as one `control-plane/` binary; Stage 8 split it into a `services/`
module that mirrors E2B's seams, Stage 9 added the edge data proxy, and Stage 10 added
the template builder:

- **`cmd/api`** — the public REST front, now **lifecycle-only** for sandboxes
  (`POST` / `DELETE` / `GET /sandboxes`) plus the template build API (`POST /templates`,
  `GET /templates/builds/{id}`). It owns a SQLite **metadata store** (`pkg/store`:
  sandboxes + builds) and calls the orchestrator over **gRPC**. On create it registers the
  sandbox's data-path route in client-proxy's catalog (and rolls the VM back if that
  fails), then hands the SDK the `data_url`. The data bytes never pass through it.
- **`cmd/client-proxy`** — E2B's **edge data proxy** (Stage 9). It owns the routing
  **catalog** (`pkg/catalog`, sandbox → node; in-memory now, Redis-shaped for later), runs
  a public data port and an internal control port (the api writes routes there), and
  reverse-proxies each data request — routed by its `X-Sandbox-Id` header — to that node's
  orchestrator data proxy. Flushing every write keeps `/execute`'s SSE live.
- **`cmd/orchestrator`** — the per-machine VM service. A gRPC `SandboxService`
  (Create / Delete / List) over `pkg/fc` (spawn / restore / destroy firecracker) and
  `pkg/pool` (the warm pool), plus a **data proxy** (`pkg/proxy`): a request carrying the
  `X-Sandbox-Id` header is bridged to the in-VM daemon over vsock. It pipes bytes, so
  `protocol.py` stays the single source of truth, and it runs the `/health` probe so a
  sandbox is healthy by the time Create returns ("ready on delivery"). It also serves a gRPC
  **`TemplateService`** (Stage 10): `TemplateCreate` kicks an async build (`pkg/build`
  wrapping `docker build` → `build-rootfs.sh` → `build-snapshot.sh`, placing artifacts via
  `pkg/storage`); the api polls `TemplateBuildStatus`. Like E2B, the builder lives here
  because it needs the same docker + KVM + firecracker the VM fleet does.

Corresponds to E2B's `infra` (api + client-proxy + orchestrator). Keeping these boundaries
is what lets the SDK stay a thin client, the data path move off the api, and the
orchestrator be swapped or scaled independently. See `docs/STAGE10_DESIGN.md`,
`docs/STAGE9_DESIGN.md` and `docs/STAGE8_DESIGN.md`.

### Transport (vsock)

The **orchestrator** (`services/pkg/proxy`) carries HTTP/SSE over Firecracker's
vsock UDS. Firecracker multiplexes the guest's vsock onto a single host Unix-domain
socket; after a text handshake (`CONNECT <port>` → `OK <hostport>`) the byte stream
is wired to the daemon listening on that vsock port inside the guest. Go's
`net/http` can't dial that raw handshake, so the orchestrator hand-rolls it in a
`RoundTripper` wrapped by a reverse proxy. The protocol bytes are identical to what
earlier stages carried over TCP; only the pipe differs. (Before Stage 4 this lived
in the SDK as `_VsockTransport`; it moved into the control plane so the SDK could
become pure HTTP.)

A side benefit over a TCP port mapping: vsock is orthogonal to "does the guest
have a NIC", so the VM can be **fully offline** (no virtio-net at all) yet still
manageable over vsock.

### The microVM's isolation mechanisms

How the orchestrator (`pkg/fc`) configures isolation, and what each piece buys:

| config | mechanism | what it gives |
|--------|-----------|---------------|
| `firecracker` + `/dev/kvm` | hardware virtualization (KVM) | the guest runs its **own Linux kernel**; escaping means breaking the guest kernel *and then* KVM |
| `boot_args: ... root=/dev/vda ro` | read-only ext4 root | the root filesystem can't be tampered with |
| `/init` mounts `tmpfs /tmp` | ramdisk | the only writable area (matches the read-only root) |
| `machine-config: vcpu_count / mem_size_mib` | VM resource quota | the guest sees a whole machine with 1 vCPU / ~512MB — a *VM* quota, not a cgroup quota |
| no `network-interfaces` in the config | no virtio-net | the sandbox code is fully offline; management still flows over vsock |

A lifecycle detail: killing the `firecracker` process destroys the entire VM (its
memory and device state vanish with the process), so the orchestrator's `Destroy`
is just terminate-the-process + remove the working directory — much simpler than
tearing down a container.

## How it evolved (preserved in git history)

The project was built up in stages, each adding one isolation technique on top of
the same protocol: host subprocess → one-shot Docker container → resident
in-container agent (the "ownership inversion") → stateful Jupyter kernel →
Firecracker microVM (TCP → vsock) → a Go control-plane split (the SDK became a thin
HTTP client) → a Go in-VM daemon (envd-equivalent, driving the kernel via a Jupyter
gateway) → an api + orchestrator gRPC split (Stage 8, mirroring E2B's services) → a
client-proxy + routing catalog (Stage 9, sinking the data plane off the api) → an async
template builder (Stage 10, E2B's TemplateService) → the in-VM daemon rewritten as ConnectRPC
`envd` + a separate `code-interpreter` (Stage 11, E2B's real in-VM shape). Through Stage 10
every step followed one discipline: **add a new backend/transport implementation, keep the
protocol byte-stable, and keep the changes out of the client as much as possible** — proven
each time by a byte-for-byte e2e oracle. **Stage 11 deliberately broke the byte-stable rule**
(the wire became ConnectRPC), trading it for a *behavioral* e2e oracle to reach E2B's actual
envd shape. Once the microVM landed, the earlier backends were removed as scaffolding — the
staged code lives on in the git history if you want to study the progression.

## Mapping to E2B (read its source alongside, once you're done)

| This project | E2B equivalent | Notes |
|--------------|----------------|-------|
| client.py | E2B SDK (python/js) | user interface; pure-HTTP for lifecycle, ConnectRPC for the data path |
| connect.py + daemon/proto | envd's ConnectRPC protocol | the client↔daemon wire (Stage 11; was protocol.py's SSE) |
| daemon/ `envd` (Go) | `envd` | in-sandbox agent: ConnectRPC Filesystem + Process (Stage 11; was server.py) |
| daemon/ `code-interpreter` (Go) | `code-interpreter` | ConnectRPC streaming Execute driving the kernel, on its own vsock port (Stage 11) |
| Jupyter Kernel Gateway + kernel | the in-sandbox kernel | the stateful Python kernel the code-interpreter drives over HTTP/WS |
| services/cmd/api (Go) | `api` | public REST front, lifecycle-only; owns the metadata store; picks a node, calls the orchestrator over gRPC, writes the catalog |
| services/cmd/client-proxy (Go) | `client-proxy` | edge data proxy; routes the data path by `X-Sandbox-Id` via the catalog (Stage 9; E2B keys on `<port>-<id>` hostnames) |
| services/cmd/orchestrator (Go) | `orchestrator` | per-machine VM fleet + warm pool + vsock data proxy + health + the template builder |
| services/pkg/build + TemplateService | `template-manager` (in E2B's orchestrator) | async template builds, polled for status (Stage 10) |
| services/pkg/store (SQLite) | the api's Postgres | durable sandbox + build metadata (E2B uses Postgres + sqlc) |
| services/pkg/catalog (in-memory) | the `sandbox-catalog` (Redis) | sandbox → node routing the client-proxy reads (E2B uses Redis) |
| services/pkg/storage (Local dir) | object storage (GCS/S3) | where template artifacts live (E2B keys by build id; we publish in place) |
| (firecracker config in services/pkg/fc) | Firecracker orchestration / jailer | microVM creation and isolation |
