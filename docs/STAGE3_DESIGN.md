# Firecracker microVM design (formerly "Stage 3")

> **Note.** This is the design journal for the microVM — the project's only
> isolation form today. It was originally "Stage 3", built on top of earlier stages
> (host subprocess → Docker container → resident container → stateful kernel) that
> have since been removed as scaffolding; the journal still references them because
> that's the context it was designed in. Those stages live on in the git history.
>
> This is the stage where the project's **isolation strength changed qualitatively
> for the first time** — upgrading from "a container that shares the host kernel" to
> "a microVM with its own guest kernel", with the escape surface dropping sharply.
> Read it alongside `docs/ARCHITECTURE.md`. Architecturally the microVM is highly
> isomorphic to the earlier resident-container stage (another "ownership
> inversion"); the only truly new boundary is **the transport changing from TCP to
> vsock**.

The ROADMAP gives Stage 3 one line: **"Slow down and understand this stage by hand—don't run on vibes alone."** This document exists precisely to
make every step understandable rather than copy-pasted.

---

## 1. What this stage solves: a qualitative change in isolation strength

The Stage 1/2 container and the host **share the same Linux kernel**. The container's isolation relies entirely on the kernel's namespaces +
cgroups—once the kernel itself has a vulnerability (container-escape CVEs appear every year), untrusted code can punch through to the host.
That's why CLAUDE.md's safety rule has always said: **the isolation in Stage 0/1/2 is not enough to run fully untrusted code.**

Stage 3 swaps the container out for a **Firecracker microVM**:

```
        Stage 2 (container)                    Stage 3 (microVM)
┌─────────────────────────┐         ┌─────────────────────────────┐
│  sandbox code            │         │  sandbox code                │
│  ───────────────         │         │  ───────────────             │
│  container = namespace+cgroup │     │  guest userspace             │
│         ↓ shared kernel   │         │  ───────────────             │
│  host Linux kernel ◄── escape │     │  guest's own Linux kernel    │ ← the sandbox has its own kernel
│        surface large      │         │  ───────────────             │
└─────────────────────────┘         │  KVM (/dev/kvm) hardware-virt boundary │ ← escaping means first breaking KVM
                                     │  ───────────────             │
                                     │  host Linux kernel           │
                                     └─────────────────────────────┘
```

- What runs inside the sandbox is **a real, independent Linux kernel**, brought up by Firecracker via KVM within the hardware-virtualization
  boundary. For untrusted code to escape, it must first break the guest kernel and then break KVM—an order of magnitude harder than breaking a
  shared kernel's namespaces. This is why E2B / AWS Lambda chose Firecracker.
- Firecracker is a "microVM": it cuts out a traditional VM's BIOS, PCI, USB, and a whole pile of emulated devices, keeping only a tiny few like
  virtio-net / virtio-block / virtio-vsock, so its **cold start can be in the low-hundreds-of-milliseconds range** and its
  memory overhead is tiny. This is exactly what lets it do "one VM per request".

> The safety rule still holds but the tone can change: Stage 3 is the **first time** the project gets isolation "strong enough to seriously discuss
> running untrusted code". But this is a **learning implementation, not security-audited**, so the docs must still honestly note "don't directly
> use it to accept arbitrary input from the outside"—doing that for real needs E2B/Fly.io-grade defense in depth (seccomp-bpf,
> jailer, network policy, rate limiting, escape monitoring…), which is productization, out of scope for this stage.

---

## 2. Isomorphic to Stage 2: another "ownership inversion", just with a VM replacing the container

The soul of Stage 2 is "the daemon moves out of the host into a **resident container**, and the responsibility of creating the isolated environment moves up
from the backend to the client". **Stage 3 swaps "container" for "microVM" in that sentence, reusing it almost verbatim.**

```
host                                sandbox microVM (long-lived, one per Sandbox)
┌──────────────────┐               ┌──────────────────────────────────┐
│ client           │   HTTP/SSE    │  daemon (server.py) ← still envd    │
│  start firecracker │   over vsock  │     │  --transport vsock           │
│  connect vsock UDS │ ────────────► │     ▼                              │
│  health probe     │ ◄──────────── │  JupyterKernelBackend (stateful)    │
│  kill firecracker │               │     └── variables persist across run_code │
└──────────────────┘               └──────────────────────────────────┘
   ▲ the guest's own kernel + the KVM boundary sit right on this vertical line
```

Against the earlier resident-container stage's responsibility migration, Stage 3 still changes the same three places,
and **`server.py`'s business logic and `protocol.py`'s wire bytes still don't change one line**:

| Code location | What Stage 2 does now | What Stage 3 changes it to |
|----------|------------------|----------------|
| `client.py:_spawn_resident_container` | `docker run -d` starts a resident container | add `_spawn_microvm`: start a Firecracker microVM (see §4.4) |
| `client.py:_wait_until_healthy` | poll `http://127.0.0.1:port/health` | poll `/health`, but over the **vsock transport** (see §4.1) |
| `client.py:close` | `docker rm -f <container name>` | kill the firecracker process + clean up the working directory |
| `client.py` transport layer | `urllib` over TCP throughout | **add a `Transport` abstraction**: the TCP path is unchanged, a new vsock path is added (see §4.1, §5) |
| `server.py:serve` | `asyncio.start_server(host, port)` (TCP) | add `--transport vsock`: listen on a socket using `AF_VSOCK` (see §4.1) |
| `protocol.py` | `/execute` `/files/*` `/commands` | **bytes unchanged**—the same HTTP/SSE content runs as-is over vsock |
| rootfs / kernel | use the docker image (`microsandbox-agent`) | **export the agent image into an ext4 rootfs** + pair it with a guest kernel (see §4.2/4.3) |

**Key observation**: the only new things in Stage 3 fall into two categories—① **swapping the transport to vsock** (touches the client's transport
layer + the server's listening method, but the protocol bytes don't change); ② **swapping the "container image" for the "kernel + rootfs"
materials a VM needs**. The daemon's and backend's execution logic, and the protocol's contract, all reuse the Stage 2 work.

---

## 3. Split into three sub-steps (each keeps the existing 42 tests all green)

Stage 3 is even heavier than Stage 2 (it adds VM-material building + a privileged environment), so all the more reason to split small and walk slowly.

### 3a — vsock transport abstraction (no VM needed, pure refactor + a new transport implementation) ← first step, the safest

**Goal**: factor out a `Transport` layer from the places in the client and server that "hardcode TCP", decoupling the protocol bytes from the
transport method. This step **doesn't touch Firecracker at all and doesn't need /dev/kvm**, so it can run directly in the current environment and
guarantees the existing 42 tests stay all green without a single edit.

- `client.py`: introduce a `Transport` abstraction with two implementations:
  - `TcpTransport`—wrap the current `urllib`/`_DIRECT_OPENER` logic as-is,
    **behaviorally byte-for-byte unchanged** (so the tests for the four topologies local/docker/container/kernel stay all green as before).
  - `VsockTransport`—connect to Firecracker's vsock UDS, do the `CONNECT <port>` handshake,
    then hand-write a minimal HTTP/1.1 client over the raw socket (see §4.1). This step only writes it; it doesn't wire it to a VM yet.
  - `_stream` / `_post_json` / `_wait_until_healthy` are changed to send and receive through `transport`.
- `server.py`: `serve()` gains `--transport {tcp,vsock}`. `tcp` uses the current
  `asyncio.start_server(host, port)`; `vsock` creates a listening socket with `socket.AF_VSOCK` and then
  `asyncio.start_server(sock=...)`. **handle/dispatch/backend all stay untouched.**

**Acceptance**: the existing 42 tests are all green (proving the TCP path's behavior didn't change); plus new **pure unit tests** for
`VsockTransport`'s HTTP frame encode/decode (no VM dependency, feed it bytes to verify request assembly / response+SSE parsing).

> Why do this step first: it **decouples** the two highest-risk things in Stage 3—"changing the most sacred client/protocol boundary" and "getting
> Firecracker working". 3a is a safe refactor backed by tests; once it's done, going to touch the VM, you have a safety margin.
> This is the same playbook as Stage 2's "2a first proves the relocation didn't change the code".

### 3b — build the microVM materials + launch Firecracker, end-to-end run_code

**Goal**: actually get the daemon running inside a Firecracker microVM, with the client connecting in over vsock to `run_code` and get the result back.

- **Material building** (`scripts/build-rootfs.sh`, see §4.2/4.3):
  1. Kernel: download a Firecracker-compatible `vmlinux` (with the virtio-vsock driver).
  2. rootfs: `docker export` the agent image's filesystem → inject our `src/microsandbox` and
     a minimal `init` → package it into an ext4 image with `mkfs.ext4 -d` (**no root needed**, see §6).
- `client.py` adds `backend="microvm"`: start firecracker (`--config-file`), poll vsock
  `/health`, and on `close` kill the process + clean the working directory (mirroring `_spawn_resident_container`).
- The in-VM daemon uses `--transport vsock`. **First get the shortest path working with `--backend local`** (stateless subprocess),
  confirm the VM+vsock+daemon chain is OK, then switch to `--backend kernel` (stateful; the agent image
  already has ipykernel).
- **This step requires you to do a one-time privileged setup first** (add the kvm group + download firecracker/vmlinux, see §7).

**Acceptance**: `Sandbox(backend="microvm").run_code("print(1+1)")` gets `2` back from a **real VM**;
after switching to the kernel backend, variables persist across `run_code`; after `close` there's no leftover firecracker process.

### 3c — cold start measurement, resource limits, (stretch) snapshot/warm-up

- **Cold start measurement** (an explicit acceptance item in the ROADMAP): record the time from `firecracker` starting the process to `/health`
  being ready, write it into the docs; compare it against Stage 1/2's container startup.
- **Resource limits** go through Firecracker's `machine-config` (`vcpu_count` / `mem_size_mib`); compare against
  Stage 2's `--memory/--cpus` to understand the difference between "VM quota" and "cgroup quota".
- **Network**: the MVP uses vsock as the control channel and **the guest has no NIC configured at all**—cleaner than Stage 2's "must open a management
  port, which drags outbound network open with it" (see §5's "isolation actually gets stronger"). Needing outbound network is a later/Stage 4 thing.
- **(Stretch, can slide to Stage 4) snapshot / restore**: Firecracker's snapshot can save the "booted-and-ready"
  VM state to disk; on restore it skips the kernel boot, achieving a **millisecond-scale cold start**; then layer a warm pool on top. This part overlaps with
  Stage 4's "sandbox pool", pick by interest.

---

## 4. Key technical points in detail (with real code anchors)

### 4.1 vsock: how the host and the VM talk

vsock (virtio-vsock) is a socket designed for "host↔VM"; its address is `(CID, port)` rather than `(IP,
port)`: we set the guest's CID to 3, and the host is fixed at 2. Firecracker **multiplexes vsock onto a single
Unix domain socket (UDS) on the host**, with a text-based handshake protocol:

- **host → guest (the direction our client wants)**: the client connects to the UDS on the host (e.g.
  `/tmp/microsandbox-vm-xxxx/fc.vsock`), sends a line `CONNECT <port>\n` (e.g. `CONNECT 1024`),
  Firecracker replies `OK <hostport>\n`, and from then on this byte stream is wired through to the process **listening on `AF_VSOCK`
  port 1024** inside the guest—that is, our daemon. After the handshake, **both sides speak that same HTTP/SSE as before**.
- **guest → host**: the guest connects to `(CID=2, port=N)`, and Firecracker connects to the host UDS `{uds}_{N}`.
  The MVP **doesn't need this direction** (the daemon is the server, the client always initiates), so we don't implement it—simpler.

Inside the guest (`server.py`) listening on vsock, the standard library is enough:

```python
import socket, asyncio
s = socket.socket(socket.AF_VSOCK, socket.SOCK_STREAM)
s.bind((socket.VMADDR_CID_ANY, 1024))   # listen on this VM's port 1024
s.listen()
server = await asyncio.start_server(self.handle, sock=s)   # handle doesn't change one line
```

On the host side (`client.py`'s `VsockTransport`), because `urllib` doesn't speak this handshake, we hand-write a **minimal
HTTP/1.1 client** (we are both client and server, the protocol is simple, a few dozen lines suffice):

```python
sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
sock.connect(uds_path)
sock.sendall(b"CONNECT 1024\n")
# read "OK <port>\n", confirm the handshake succeeded
# then write "POST /execute HTTP/1.1\r\n...\r\n\r\n<body>" into it,
# and stream the response back per Content-Length / text/event-stream—yield line by line, reusing the existing SSE parsing.
```

> A side dividend: after 3a factors out the HTTP frame send/receive, the TCP path could actually drop `urllib` too. But to keep the existing
> 42 tests all green with **zero behavior change**, `TcpTransport` in 3a **still wraps the current `urllib`**—
> unifying everything onto raw sockets is an optional follow-up cleanup, not on Stage 3's critical path.

### 4.2 rootfs: export from the docker image, package without root

Firecracker needs an **ext4 disk image** as the root filesystem. We don't build it from scratch; instead we reuse the
`microsandbox-agent` image already built in Stage 2 (it has Python + ipykernel + jupyter_client):

```
docker create microsandbox-agent           # make a non-started container, to grab its rootfs
docker export <id> | tar -x -C rootfs/      # export the whole filesystem tree into the rootfs/ directory
cp -r src/microsandbox rootfs/opt/.../       # inject our package (Stage 2 mounts it at runtime, in the VM it has to be put in)
install init  rootfs/init                    # place a minimal init (see §4.3)
mkfs.ext4 -d rootfs/ microsandbox.ext4 512M  # ★ package the directory directly into ext4, never mounting, no root
```

`mkfs.ext4 -d <dir>` is the key: it **writes a directory tree directly into a new ext4 image** without `mount`,
so **the entire rootfs build chain needs no root** (`docker`/`tar`/`mkfs.ext4 -d` all run as the current user).
The files in the image are owned by the build user (uid 1000), but the in-guest daemon runs as root, root can read everything,
no issue.

### 4.3 init: PID 1 inside the guest

After the kernel finishes booting it executes `init` (PID 1). We place a **minimal shell init** that mounts the pseudo-filesystems and then
`exec`s our daemon (`exec` lets the daemon take over PID 1, saving a process layer):

```sh
#!/bin/sh
mount -t proc     proc /proc
mount -t sysfs    sys  /sys
mount -t devtmpfs dev  /dev      2>/dev/null   # the kernel may already have mounted it
mount -t tmpfs    tmp  /tmp                    # the only writable area, matching Stage 2's --tmpfs /tmp
export HOME=/tmp PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1
exec python3 -m microsandbox.server --transport vsock --vsock-port 1024 --backend kernel
```

In the kernel boot args we use `init=/init` to point to it; the root device is
`/dev/vda` (the ext4 we attached), with `ro` for a read-only root (all writes go to tmpfs /tmp, same as Stage 2).

### 4.4 Launching Firecracker: declarative config-file

There are two ways to start a VM: the REST API (start the process first, then PUT configs one by one) and `--config-file` (a single JSON
declaring everything). **For the learning phase, choose `--config-file`**, because a single file lets you read off what the whole VM looks like:

```json
{
  "boot-source":   { "kernel_image_path": "vmlinux",
                     "boot_args": "console=ttyS0 reboot=k panic=1 init=/init ro" },
  "drives":        [{ "drive_id": "rootfs", "path_on_host": "microsandbox.ext4",
                      "is_root_device": true, "is_read_only": true }],
  "machine-config":{ "vcpu_count": 1, "mem_size_mib": 256 },
  "vsock":         { "guest_cid": 3, "uds_path": "fc.vsock" }
}
```

What `client._spawn_microvm` does maps one-to-one to Stage 2's `_spawn_resident_container`:
create a per-VM working directory → write the above config (uds/paths all inside that directory) →
`subprocess.Popen(["firecracker", "--config-file", cfg, "--api-sock", api])` →
`_wait_until_healthy` (over vsock) → on `close`, `proc.terminate()`/`kill()` + delete the working directory.

---

## 5. Protocol/transport evolution principle & "isolation actually gets stronger"

**The protocol (protocol.py) bytes are unchanged**, and this main thread still holds in Stage 3—we are only moving the same string of HTTP/SSE
bytes from a "TCP connection" onto a "vsock UDS connection". Because the transport method really changed, the client **needs to change for the first
time** (introducing the `Transport` abstraction), but the change is confined to the transport layer; upper-level APIs like `run_code` are unchanged for the user,
and `server.py`'s request handling logic and `backend.py`'s execution logic all stay put.

**Security: Stage 3 is the first time "isolation actually gets stronger" appears, rather than "weakened for the sake of a management channel".**
In Stage 2, to let the client connect to the in-container daemon, it **must open a TCP management port**, which incidentally also opens up the guest's outbound
network (the earlier resident-container stage honestly recorded this regression). Stage 3 uses vsock as the control channel, so it **fundamentally
doesn't need to give the VM a NIC**—the management channel goes over virtio-vsock, orthogonal to "whether there's a network". So we can:

- The guest has **no virtio-net at all** → the sandbox code is completely network-less (back to Stage 1's cut-off strength),
  while **keeping** the management channel. The "manage and yet cut off the network" that Stage 2 couldn't do, Stage 3 achieves naturally thanks to vsock.
- Stack the guest's own kernel on top → this is the strongest isolation combination in the project so far.

---

## 6. Feasibility on this machine (WSL2) and the one-time privileged setup

Environment-probe conclusions (2026-06, this machine):

| Check | Result | Impact |
|--------|------|------|
| `/dev/kvm` | **exists** (`crw-rw---- root kvm`) | WSL2 nested virtualization is on, Firecracker has a chance |
| Architecture | x86_64 | download the x86_64 firecracker / vmlinux |
| Current user opening `/dev/kvm` | **denied** (not in the `kvm` group) | needs a group add or sudo—see the one-time setup below |
| Passwordless sudo | **unavailable** | I (Claude) can't sudo non-interactively, so the privileged steps you must run yourself |
| `mkfs.ext4` / `docker` / `curl` | all present | the rootfs build chain is complete, and can run **without root** (`mkfs.ext4 -d`) |
| Disk / memory | 919G / 14G available | ample |

**One-time privileged setup (please run it in a terminal with `! <cmd>`, or in your own shell)**:

```bash
# 1) add yourself to the kvm group so firecracker can open /dev/kvm without sudo (one-time)
sudo usermod -aG kvm $USER
# 2) restart WSL to apply the group: in Windows PowerShell run `wsl --shutdown`, then reopen the terminal
#    (after reopening, `id` should show the kvm group, and python's open(/dev/kvm) no longer raises PermissionError)
```

Download the materials (**no sudo needed**, I can run it for you, but it's placed here so you can verify the sources):

```bash
# firecracker static binary (GitHub release)
# vmlinux: a kernel image provided by Firecracker CI, with virtio-vsock
# the exact version numbers are pinned when 3b lands and written into scripts/, to avoid the URLs in this doc going stale
```

**Once past the kvm-group hurdle, I can fully automate the rest of Stage 3**: the rootfs goes through `mkfs.ext4 -d` without root,
firecracker needs no sudo after the group add, and the tests can really run on this machine. Two things still pending **hands-on verification when 3b launches**:
① WSL2's nested KVM can be used normally by Firecracker; ② the virtio-vsock driver in the downloaded vmlinux works.
These two are "very likely to work, but must be proven by powering on"; this doc doesn't make guarantees.

---

## 7. Compatibility: Stage 3 is a "new topology", the old ones are untouched

As with Stage 2, Stage 3 **adds** `backend="microvm"`, coexisting with `local/docker/container/kernel`,
and removes none of the old ones—the existing 42 tests are the safety net and must stay all green as-is.

`tests/conftest.py` adds a `requires_firecracker` skip guard (checking the firecracker binary +
`/dev/kvm` readable + materials ready); missing any one **skips the whole group**, just like when docker is unavailable—this way on other
machines / CI, `pytest` stays all green, and the microVM cases really run only once this machine is set up. The `backend` value table extends to:

| `backend` | topology | guest/in-container execution backend | isolation strength | stage |
|-----------|------|----------------------|----------|------|
| `local` | host daemon | subprocess (no isolation) | none | 0 |
| `docker` | host daemon | a throwaway container per execution | shared kernel | 1 |
| `container` | resident container | in-container subprocess (stateless) | shared kernel | 2a |
| `kernel` | resident container | Jupyter kernel (stateful) | shared kernel | 2b |
| **`microvm`** | **resident microVM** | **in-VM Jupyter kernel (stateful)** | **own kernel + KVM** | **3** |

---

## 8. Stage 3 acceptance criteria

- [x] **3a**: the transport abstraction lands; the existing 42 tests are all green with zero changes (+3 vsock unit tests = 45);
      `VsockTransport`'s CONNECT handshake + HTTP frame encode/decode + SSE parsing are covered by pure unit tests (no VM dependency).
- [x] **3b**: `backend="microvm"` works end to end—`run_code` from a real Firecracker microVM
      gets the result back; the in-VM kernel backend persists variables across `run_code`; after `close` there's no leftover process/working directory.
      (See §9 for measured records; `tests/test_microvm.py` has 4 items, auto-skipped when materials are missing.)
- [x] **3c**: cold start ~0.94s recorded; resource limits take effect through `machine-config` (`test_microvm` verifies it);
      the guest has no NIC yet is still manageable ("isolation actually gets stronger" delivered); **the stretch is done**: snapshot restore gives millisecond-scale cold start (see §9).
      Warm pool (one snapshot forked into N VMs) → Stage 4.
- [ ] `pytest` is all green throughout (the microVM cases really run on this machine; environments without firecracker auto-skip).

**The same discipline carried over from Stage 2**: add backend/transport implementations, keep the protocol bytes unchanged, and keep the changes out of
the client transport layer as much as possible; `tests/` all green is the safety net for cross-stage refactoring.

---

## 9. 3b measured records (this machine, WSL2, 2026-06)

It works; recording the key facts and the pitfalls hit (the part most valuable for learning):

- **Materials**: the firecracker v1.16.0 static binary + Firecracker CI's vmlinux 6.1.155 (vsock /
  virtio-blk / ext4 / devtmpfs all built-in `=y`, verified against `.config` before downloading). The rootfs is built by
  `scripts/build-rootfs.sh` from the agent image via `docker export` + `mkfs.ext4 -d`, packaged without root (~250MB).
- **Cold start**: from constructing `Sandbox(backend="microvm")` to the daemon being ready, stably **~0.94s** (including firecracker
  starting the process + kernel boot + python startup + the daemon listening on vsock). Even with a whole extra guest kernel it's still sub-second—
  the payoff of the microVM "cutting out a traditional VM's BIOS/PCI/USB, keeping only virtio". (Snapshots achieving millisecond cold start are left to 3c.)
- **Pitfall 1 · `sys.executable` is empty**: when the daemon starts as PID 1 and init doesn't set `PATH`, Python can't compute
  its own executable path, and the `local` backend's `create_subprocess_exec("")` raises PermissionError. Fix: in init,
  explicitly `export PATH=...` and exec python with an absolute path (see `scripts/build-rootfs.sh`'s /init).
- **Pitfall 2 · loopback is down by default**: the kernel backend's Jupyter kernel speaks ZMQ over 127.0.0.1, but in the microVM
  `lo` is down by default, causing "kernel startup 60s timeout". The minimal rootfs has no ip/ifconfig, so in the daemon's
  vsock startup path we bring `lo` up using a `SIOCSIFFLAGS` ioctl (see `server._ensure_loopback_up`).
- **"Isolation actually gets stronger" delivered**: the guest is configured with only vsock and **no virtio-net**—`/sys/class/net` has only `lo`,
  and connecting to 1.1.1.1 directly raises OSError (no outbound network). That is, "the management channel goes over vsock + the sandbox code is fully cut off"
  both hold at once, exactly what Stage 2's "forced to open outbound to expose a management port" couldn't do (§5).

### Snapshot restore (3c stretch, implemented)

Firecracker's signature trick: save the "booted and warmed-up" VM state to disk, and on restore skip the kernel boot for millisecond-scale readiness.

- **How to build** (`scripts/build-snapshot.sh`): boot a base VM → warm up the Jupyter kernel (run a chunk of
  `pass` to force the kernel up) → `PATCH /vm {Paused}` → `PUT /snapshot/create {Full}`. Two artifacts:
  `vendor/snapshot/vmstate` (~13KB device/CPU state) + `memfile` (512MB guest memory, **including the hot kernel**).
- **How to restore** (`Sandbox(backend="microvm", from_snapshot=True)` → `_restore_microvm`): start an
  empty firecracker → drive the state back in via the REST API `PUT /snapshot/load` + `resume_vm`. Snapshot load/create
  can only go through the API (`--config-file` can't express it), and the client drives it with `_firecracker_api` (HTTP over AF_UNIX).
- **Measured comparison** (this machine):

  | Path | Ready | First run_code | To first result |
  |------|------|---------------|-----------|
  | Cold start (`_spawn_microvm`) | ~0.94s | ~0.8s (incl. kernel cold start) | ~1.77s |
  | Snapshot restore (`_restore_microvm`) | **~0.03–0.04s** | ~0.13s (kernel already hot) | **~0.17s** |

  To first result, about **10× speedup**; readiness itself about 30×.
- **Disk**: a snapshot only stores memory + device state, **the disk contents are still provided by the host's `rootfs.ext4`**—on restore it must
  still be at the original path (which is why the restore path's `_check_microvm_available` still validates the rootfs).
- **Known limitation (single instance)**: the vsock uds path in the snapshot is fixed, so only one VM can be restored at a time. **Concurrent restore
  + a warm pool** (one base snapshot forked into N second-scale sandboxes) needs a per-VM uds override, left to Stage 4.
