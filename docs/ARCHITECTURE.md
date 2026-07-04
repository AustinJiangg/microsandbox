# Architecture

This document explains the design of `microsandbox` and the boundaries that kept
it evolvable.

## Global data flow

```
   your program ── sandbox.run_code(...) ──▶ client (SDK, client.py, pure HTTP)
        │                                          │
   lifecycle:                                 data: ConnectRPC envd / code-interpreter
   POST/DELETE/GET /sandboxes                 (Host: <port>-<id> header)
        ▼                                          ▼
   api (services/cmd/api)                     client-proxy (services/cmd/client-proxy)
   • REST front, lifecycle-only               • edge data proxy; reads the routing
   • auth: X-API-Key→team, team-scoped (S16)    catalog (Redis; pkg/catalog)
   • Postgres metadata store (pkg/store)      • parse Host <port>-<id> → catalog →
   • on create: gRPC Create, mint token,        that node's data proxy (id + port)
     write {node,token} into the catalog      • gate control ports on X-Access-Token (S16)
        │                                          │
        │ gRPC SandboxService                      │ HTTP (X-Sandbox-Id + X-Sandbox-Port)
        └──────────────▶ orchestrator ◀────────────┘
                         (services/cmd/orchestrator)
                         • owns the microVM (pkg/fc) + warm pool (pkg/pool) + net slot (pkg/network)
                         • data proxy (pkg/proxy): dial <slot-ip>:<port> over TCP
                                │ HTTP over the VM's NIC (TCP; Stage 12 retired vsock)
                                ▼
                         daemon (daemon/, Go envd) → stateful Jupyter kernel
                                ↑ guest kernel + KVM boundary
```

The SDK is a thin **pure-HTTP** client. It sends **lifecycle** to the **api** and the
**data path** to **client-proxy** (it learns that URL from the create response). Stage 8
split the old single control plane into the **api** (`services/cmd/api`, public REST +
Postgres metadata store) calling the per-machine **orchestrator**
(`services/cmd/orchestrator`) over **gRPC** for the VM lifecycle. **Stage 9** added the
**client-proxy** (`services/cmd/client-proxy`), E2B's edge data proxy: it reads a routing
**catalog** (`pkg/catalog`, sandbox → node in a shared **Redis** since Stage 14a) that the api
writes on create, and routes each data request by parsing its `<port>-<id>` `Host` header
(Stage 12) to the right orchestrator's data proxy, which dials the VM's NIC over TCP. The api is now
**lifecycle-only** — the data bytes never pass through it. As of **Stage 11** the in-VM
daemon is **two ConnectRPC services** (E2B's `envd` + a separate `code-interpreter`), and
**Stage 12** put them on **two TCP ports** over the VM's NIC (`:49983` / `:49999`) reached by
real `<port>-<id>` hostnames — retiring vsock. Through Stage 10 the wire was byte-stable
(the project's defining discipline); Stage 11 deliberately moved it to ConnectRPC, so the
e2e suite is now a **behavioral** parity oracle (see below). **Stage 16** added **identity**:
the api authenticates every lifecycle request with an `X-API-Key` resolving to a **team**
(resources are team-scoped), and client-proxy gates the in-VM **control ports** on a
**per-sandbox access token** (user-exposed ports stay public). See `docs/STAGE16_DESIGN.md` /
`docs/STAGE12_DESIGN.md` /
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
  returns only once the VM is healthy, plus the `data_url` to reach it and (Stage 16) the
  per-sandbox **access token**. Lifecycle calls carry the `X-API-Key` (from `api_key=` /
  `MICROSANDBOX_API_KEY`); no key → the api answers 401 (no silent default, matching E2B).
- `run_code` / files / commands speak **ConnectRPC** to **client-proxy** (`data_url`) with a
  `<port>-<id>` `Host` header (Stage 12; the port picks the in-VM service) and an
  `X-Access-Token` (Stage 16; gates the control ports): `run_code` is the
  code-interpreter's server-streaming `Execute`; files/commands are envd's unary `Filesystem`
  / `Process` RPCs. The SDK also exposes `get_host(port)` to reach any user port (public, no token).
- `close()` asks the **api** to destroy the VM (`DELETE /sandboxes/{id}`).

It holds no transport or firecracker code anymore — that lives in the orchestrator (4).

### 3. the in-VM daemon (`daemon/`, Go) + the kernel

A static **Go binary** (`daemon/`, baked into the rootfs), matching E2B's `envd`. It runs
**inside the VM** on **two TCP ports** over the VM's NIC (Stage 12; vsock before):

- **`envd`** (`:49983`) — ConnectRPC `FilesystemService` (read/write/list on the VM's own
  filesystem) + `ProcessService` (run a command via `sh -c`) + a plain `GET /health`.
- **`code-interpreter`** (`:49999`) — a ConnectRPC server-streaming `Execute` driving the kernel.

The orchestrator dials whichever port the `<port>-<id>` hostname named (Stage 12 replaced
Stage 11's vsock ports + `/codeinterpreter.*` path-prefix routing). It replaced
`src/microsandbox/server.py` + `backend.py` (kept in `src/` as reference). See
`docs/STAGE7_DESIGN.md` (the Go rewrite), `docs/STAGE11_DESIGN.md` (the ConnectRPC split),
and `docs/STAGE12_DESIGN.md` (the TCP/NIC flip).

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
  Here: a Firecracker microVM, reached over TCP via its own TAP/netns NIC (vsock
  before Stage 12). This is a **client/transport** concern.
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
  `GET /templates/builds/{id}`). Since **Stage 16** a `withAuth` middleware authenticates every
  route except `/health` with an `X-API-Key` resolving to a **team** (keys hashed in `pkg/store`,
  seeded by `--seed-api-keys`); resources are **team-owned**, so list/delete/build-status are
  team-scoped (another team's id is 404, and the ownership check precedes any VM teardown). It owns a
  **metadata store** (`pkg/store`: sandboxes + builds + teams/api_keys; **Postgres** by default
  since Stage 14b, SQLite still selectable via a `sqlite://`
  DSN) and calls the orchestrator over **gRPC**. On create it mints the per-sandbox access token,
  writes the sandbox's route (`{node, token}`) directly to the shared **Redis** catalog (and rolls
  the VM back if that fails), then hands the SDK the `data_url` + token. The data bytes never pass through it.
- **`cmd/client-proxy`** — E2B's **edge data proxy** (Stage 9). It reads the routing
  **catalog** (`pkg/catalog`, sandbox → `{node, access-token}`; a shared **Redis** since Stage 14a — the api
  writes routes there directly, which retired the old internal control port), runs a public
  data port, and reverse-proxies each data request — routed by its `<port>-<id>` `Host` header
  (Stage 12; the port selects the in-VM service or a user port) — to that node's orchestrator
  data proxy. Since **Stage 16** it gates the in-VM **control ports** (envd `:49983`,
  code-interpreter `:49999`) on the per-sandbox token (`X-Access-Token`, constant-time compare),
  while user-exposed ports stay public. Flushing every write keeps the code-interpreter's streamed `Execute` live.
- **`cmd/orchestrator`** — the per-machine VM service. A gRPC `SandboxService`
  (Create / Delete / List) over `pkg/fc` (spawn / restore / destroy firecracker) and
  `pkg/pool` (the warm pool) + `pkg/network` (the per-sandbox TAP/netns slot), plus a
  **data proxy** (`pkg/proxy`): a request carrying the `X-Sandbox-Id` + `X-Sandbox-Port`
  headers is bridged to the in-VM daemon over the VM's NIC (TCP; Stage 12 retired vsock). It
  pipes bytes, and it runs the `/health` probe so a sandbox is healthy by the time Create
  returns ("ready on delivery"). It also serves a gRPC
  **`TemplateService`** (Stage 10): `TemplateCreate` kicks an async build (`pkg/build`
  wrapping `docker build` → `build-rootfs.sh` (or `build-rootfs-layered.sh` for a COW `base` build, Stage 19) → `build-snapshot.sh`, publishing artifacts via
  `pkg/storage` to S3 object storage since Stage 15); the api polls `TemplateBuildStatus`. Like E2B, the builder lives here
  because it needs the same docker + KVM + firecracker the VM fleet does.

Corresponds to E2B's `infra` (api + client-proxy + orchestrator). Keeping these boundaries
is what lets the SDK stay a thin client, the data path move off the api, and the
orchestrator be swapped or scaled independently. See `docs/STAGE10_DESIGN.md`,
`docs/STAGE9_DESIGN.md` and `docs/STAGE8_DESIGN.md`.

### Transport (TCP over a per-sandbox NIC)

The **orchestrator** (`services/pkg/proxy`) reverse-proxies plain HTTP to the in-VM
daemon over the **VM's NIC**. Each sandbox gets a network slot (`services/pkg/network`):
a virtio-net interface backed by a host **TAP** inside the sandbox's **own netns**, a
fixed guest IP set from the `ip=` boot arg, and a veth pair + **iptables DNAT** giving the
slot a routable host address. The orchestrator dials `<slot-ip>:<port>` (the port carried
in the `<port>-<id>` hostname picks envd `:49983`, code-interpreter `:49999`, or any user
port); `FlushInterval -1` keeps the code-interpreter's streamed `Execute` live. The
`Transport` sets `Proxy: nil` so a host `HTTP_PROXY` can't intercept the slot address.

> **History.** Stages 3–11 carried this same channel over **vsock** (a hand-rolled
> `CONNECT <port>` handshake in a `RoundTripper`; before Stage 4 it lived in the SDK as
> `_VsockTransport`). **Stage 12 replaced vsock with the TCP/TAP path** above and removed
> the vsock device — see `docs/MICROVM_DESIGN.md` §6 and `docs/STAGE12_DESIGN.md`.

The trade-off this changed: vsock kept the VM **fully offline** (no NIC at all) yet
manageable. The TCP path gives every sandbox a real network identity — so its ports can be
**exposed** — but to preserve most of that isolation the slot installs **DNAT only, no
MASQUERADE**: the sandbox is **inbound-reachable but cannot phone home (outbound-denied by
default)**. This narrows the isolation; it remains a learning implementation, not
security-audited, never safe for untrusted input.

### The microVM's isolation mechanisms

How the orchestrator (`pkg/fc`) configures isolation, and what each piece buys:

| config | mechanism | what it gives |
|--------|-----------|---------------|
| `firecracker` + `/dev/kvm` | hardware virtualization (KVM) | the guest runs its **own Linux kernel**; escaping means breaking the guest kernel *and then* KVM |
| `boot_args: ... root=/dev/vda ro` | read-only ext4 root | the root filesystem can't be tampered with |
| `/init` mounts `tmpfs /tmp` | ramdisk | the only writable area (matches the read-only root) |
| `machine-config: vcpu_count / mem_size_mib` | VM resource quota | the guest sees a whole machine with 1 vCPU / ~512MB — a *VM* quota, not a cgroup quota |
| `network-interfaces` (TAP in a per-sandbox netns) + **DNAT, no MASQUERADE** | virtio-net, inbound-only | the sandbox is reachable (ports can be exposed) but **outbound-denied by default** — it cannot phone home (Stage 12; was "fully offline over vsock" before) |

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
`envd` + a separate `code-interpreter` (Stage 11, E2B's real in-VM shape) → per-sandbox
TAP/netns networking, the data path flipped vsock → TCP routed by `<port>-<id>` hostnames,
user-port exposure, and vsock retired (Stage 12, reversing the vsock-first decision) → UFFD
lazy snapshot restore behind `--uffd` (Stage 13) → the routing catalog and metadata store
swapped onto Redis + Postgres (Stage 14) → template artifacts moved to S3 object storage
(Stage 15) → auth: `X-API-Key`→team + a per-sandbox data-plane access token (Stage 16, the
first production-fidelity stage) → the streamed memfile stored compacted behind a per-block
`pkg/storage/header` index (Stage 17, the first storage-mechanism-depth item) → that header
carrying a copy-on-write **build owner** so a template `from` a base stores its **rootfs as a diff**,
assembled at boot (Stage 18, COW layered builds) → the layered child produced by a **layout-preserving in-place
edit** of a copy of the base's rootfs (`debugfs`, no re-mkfs) so that diff is ~its genuine delta (Stage 19: the
same `derived` dropped from 278.8 MiB to **28 KiB**). Through Stage 10
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
| daemon/ `code-interpreter` (Go) | `code-interpreter` | ConnectRPC streaming Execute driving the kernel, on its own TCP port `:49999` (Stage 11 vsock port → Stage 12 TCP) |
| Jupyter Kernel Gateway + kernel | the in-sandbox kernel | the stateful Python kernel the code-interpreter drives over HTTP/WS |
| services/cmd/api (Go) | `api` | public REST front, lifecycle-only; **authenticates `X-API-Key`→team and scopes resources to it** (Stage 16); owns the metadata store; picks a node, calls the orchestrator over gRPC, writes the catalog (with the access token) |
| services/cmd/client-proxy (Go) | `client-proxy` | edge data proxy; routes the data path by parsing the `<port>-<id>` `Host` header via the catalog (Stage 12; matches E2B's hostname keying); **gates the control ports on the per-sandbox `X-Access-Token`** (Stage 16) |
| services/cmd/orchestrator (Go) | `orchestrator` | per-machine VM fleet + warm pool + per-sandbox network slot (`pkg/network`) + TCP data proxy + health + the template builder |
| services/pkg/build + TemplateService | `template-manager` (in E2B's orchestrator) | async template builds, polled for status (Stage 10) |
| services/pkg/store (Postgres; SQLite selectable) | the api's Postgres | durable sandbox + build metadata, team-scoped, + teams/api_keys (keys hashed) for auth (E2B uses Postgres + sqlc; Stage 14b / 16) |
| services/pkg/catalog (Redis) | the `sandbox-catalog` (Redis) | sandbox → `{node, access-token}` routing the client-proxy reads (E2B uses Redis; Stage 14a / 16) |
| services/pkg/storage (S3 via minio-go; Local dir = test double) | object storage (GCS/S3) | where template artifacts live, buildID-keyed + an `aliases/<name>` pointer (Stage 15); rootfs/snapfile materialized locally, memfile streamed page-by-page over UFFD. Stage 17: the memfile is stored **compacted** (non-zero blocks only) behind a per-block index (`pkg/storage/header`); the boot path serves zero/gap pages without a fetch. Stage 18: that header gained a COW **build owner**, so a layered template stores its **rootfs as a diff** over its base (`MaterializeLayered` assembles it at boot). Stage 19: the layered child is produced by an in-place `debugfs` edit of a copy of the base's rootfs (not re-mkfs), so the diff is ~the genuine delta (`derived`: 278.8 MiB → 28 KiB). (E2B serves the rootfs lazily over NBD — deferred; it **does** optionally compress — V4/V5 zstd/lz4 2 MiB frames, orthogonal to COW, deferred here; corrected in Stage 20, see `docs/STAGE20_DESIGN.md` §2) |
| (firecracker config in services/pkg/fc) | Firecracker orchestration / jailer | microVM creation and isolation |
| services/pkg/network | E2B's per-sandbox TAP/netns + DNAT | the per-sandbox network slot: TAP in its own netns, veth, DNAT (inbound only, no MASQUERADE) (Stage 12) |
