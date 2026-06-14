# Firecracker microVM design

How `microsandbox` isolates code: every sandbox is a **Firecracker microVM**. Read
this alongside `docs/ARCHITECTURE.md` (the three-layer client / protocol /
daemon+backend design). The point of the project is to understand the mechanics by
hand, so this walks through each piece with real code anchors.

---

## 1. What the microVM gives you: a real isolation boundary

A shared-kernel container isolates via the host kernel's namespaces + cgroups —
once the kernel itself has a vulnerability (container-escape CVEs appear every
year), untrusted code can punch through to the host. A Firecracker microVM instead
gives the sandbox **its own guest Linux kernel** behind the KVM
hardware-virtualization boundary:

```
   a shared-kernel container               this project: a microVM
┌─────────────────────────┐         ┌─────────────────────────────┐
│  sandbox code            │         │  sandbox code                │
│  ───────────────         │         │  ───────────────             │
│  namespace + cgroup      │         │  guest userspace             │
│        ↓ shared kernel   │         │  ───────────────             │
│  host Linux kernel ◄─ escape │     │  guest's own Linux kernel    │ ← its own kernel
│        surface large     │         │  ───────────────             │
└─────────────────────────┘         │  KVM (/dev/kvm) hw-virt boundary │ ← escaping means breaking KVM
                                     │  ───────────────             │
                                     │  host Linux kernel           │
                                     └─────────────────────────────┘
```

- To escape, untrusted code must break the guest kernel **and then** break KVM — an
  order of magnitude harder than breaking a shared kernel's namespaces. This is why
  E2B / AWS Lambda chose Firecracker.
- Firecracker is a "microVM": it cuts out a traditional VM's BIOS, PCI, USB, and a
  pile of emulated devices, keeping only a tiny few like virtio-block / virtio-vsock
  (and optionally virtio-net), so its **cold start is sub-second** and its memory
  overhead is tiny — exactly what makes "one VM per sandbox" practical.

> **Safety.** This is the first isolation in the project strong enough to
> *seriously discuss* untrusted code — but it is a **learning implementation, not
> security-audited**. Doing it for real needs E2B/Fly.io-grade defense in depth
> (jailer, seccomp-bpf, network policy, rate limiting, escape monitoring), which is
> out of scope. Don't expose it as a service or feed it arbitrary external input.

---

## 2. How it fits together: the control plane owns the VM, the daemon lives inside it

```
host                                                  sandbox microVM (one per Sandbox)
┌──────────────┐      ┌────────────────────────┐      ┌──────────────────────────────────┐
│ SDK          │ HTTP │ control plane (Go)      │ vsock│  daemon (server.py) ← E2B's envd   │
│ client.py    │ ────►│  start/kill firecracker │ ────►│     │  listens on AF_VSOCK :1024   │
│ (pure HTTP)  │ ◄──── │  (microvm.go)           │ ◄────│     ▼                              │
└──────────────┘      │  vsock bridge + health  │      │  JupyterKernelBackend (stateful)   │
                      │  (proxy.go)             │      │     └ variables persist across run_code │
                      └────────────────────────┘      └──────────────────────────────────┘
                                                         ▲ the guest's own kernel + the KVM boundary
```

As of Stage 4 the **control plane** (`control-plane/`, Go) owns the VM lifecycle:
`spawnMicroVM` writes a declarative config and starts `firecracker`; the vsock
bridge (`proxy.go`) connects in; `waitHealthy` polls `/health` before handing the
sandbox back; `destroy` kills the process and removes the per-VM working directory.
The SDK (`client.py`) is now a thin pure-HTTP client that drives it. The daemon
(`server.py`) and the wire protocol (`protocol.py`) don't know they're inside a VM —
`server.py`'s request handling and `backend.py`'s execution are exactly what they'd
be anywhere; only the *kind of socket* the daemon listens on differs (`AF_VSOCK`
instead of TCP). That is the project's core invariant: **keep the protocol bytes
fixed, swap only the isolation and the transport underneath.** (See
`docs/STAGE4_DESIGN.md` for how the control plane was split out.)

Two orthogonal concerns, worth keeping separate (see `ARCHITECTURE.md`):
*isolation/transport* (a microVM reached over vsock — owned by the control plane)
and the *execution model* (a stateful Jupyter kernel — the backend concern). The
microVM is therefore not a "backend"; it's the isolation the control plane creates,
inside which the daemon runs the kernel backend.

---

## 3. vsock: how the host and the VM talk

vsock (virtio-vsock) is a socket designed for "host↔VM"; its address is `(CID,
port)` rather than `(IP, port)`: the guest's CID is 3, the host is fixed at 2.
Firecracker **multiplexes the guest's vsock onto a single Unix domain socket (UDS)
on the host**, with a text handshake:

- **host → guest (the direction we need)**: the control plane connects to the
  host UDS (e.g. `/tmp/microsandbox-vm-xxxx/fc.vsock`), sends a line `CONNECT
  <port>\n` (e.g. `CONNECT 1024`), Firecracker replies `OK <hostport>\n`, and from
  then on this byte stream is wired through to the process **listening on
  `AF_VSOCK` port 1024** inside the guest — our daemon. After the handshake, both
  sides speak the same HTTP/SSE as always.
- **guest → host**: the MVP doesn't need this direction (the daemon is the server,
  the client always initiates), so it isn't implemented.

Inside the guest (`server.py`), the standard library is enough:

```python
import socket, asyncio
s = socket.socket(socket.AF_VSOCK, socket.SOCK_STREAM)
s.bind((socket.VMADDR_CID_ANY, 1024))   # listen on this VM's port 1024
server = await asyncio.start_server(self.handle, sock=s)   # handle is transport-agnostic
```

On the host side (`control-plane/proxy.go`), Go's `net/http` can't dial this
handshake, so the control plane hand-rolls it in a `RoundTripper`:

```go
conn, _ := net.Dial("unix", udsPath)
fmt.Fprintf(conn, "CONNECT 1024\n")                       // then read back "OK <port>\n" to confirm
req.Write(conn)                                            // "POST /execute HTTP/1.1\r\n...\r\n\r\n<body>"
resp, _ := http.ReadResponse(bufio.NewReader(conn), req)  // stream SSE / read JSON back
```

A reverse proxy wraps that transport, so `POST /sandboxes/{id}/execute` from the SDK
is bridged to `/execute` inside the guest, with the daemon's SSE streamed straight
back. (Before Stage 4 this lived in the SDK as `_VsockTransport`; the protocol bytes
are unchanged — only the language and the location moved.)

---

## 4. The VM materials: an ext4 rootfs + a minimal init

### 4.1 rootfs: export from the agent image, package without root

Firecracker needs an **ext4 disk image** as the root filesystem. We don't build it
from scratch; `scripts/build-rootfs.sh` reuses the `microsandbox-agent` image
(Python + ipykernel + jupyter_client — docker is only a one-time build tool here):

```
docker create microsandbox-agent              # a non-started container, to grab its rootfs
docker export <id> | tar -x -C rootfs/ --exclude='dev/*'   # export the whole tree (skip device nodes)
cp -r src/microsandbox rootfs/opt/microsandbox/src         # inject our package (no runtime mount inside a VM)
# write a minimal /init (see 4.2)
mkfs.ext4 -d rootfs/ rootfs.ext4<size>        # ★ pack the directory straight into ext4 — no mount, no root
```

`mkfs.ext4 -d <dir>` is the key: it **writes a directory tree directly into a new
ext4 image** without `mount`, so the **entire build chain needs no root**
(`docker` / `tar` / `mkfs.ext4 -d` all run as the current user). Files in the image
are owned by the build user, but the in-guest daemon runs as root and can read
everything, so it's fine.

### 4.2 init: PID 1 inside the guest

After the kernel boots it executes `init` (PID 1). We place a **minimal shell init**
that mounts the pseudo-filesystems and then `exec`s the daemon (`exec` lets the
daemon take over PID 1, saving a process layer):

```sh
#!/bin/sh
mount -t proc     proc /proc
mount -t sysfs    sys  /sys
mount -t devtmpfs dev  /dev  2>/dev/null   # the kernel likely already mounted it (DEVTMPFS_MOUNT=y)
mount -t tmpfs    tmp  /tmp                # the only writable area (the root is read-only)
# PATH must be set explicitly: a minimal init leaves it empty, and then python can't even
# compute sys.executable (which the Jupyter kernel manager needs to spawn the kernel). See pitfall 1 in §8.
export PATH=/usr/local/bin:/usr/bin:/bin
export HOME=/tmp PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 PYTHONPATH=/opt/microsandbox/src
exec python3 -m microsandbox.server --vsock-port 1024
```

In the kernel boot args we point `init=/init`; the root device is `/dev/vda` (the
ext4 we attached), mounted `ro` (all writes go to tmpfs `/tmp`).

---

## 5. Launching Firecracker: one declarative config file

There are two ways to start a VM: the REST API (start the process, then PUT configs
one by one) and `--config-file` (a single JSON declaring everything). We use
`--config-file`, because one file lets you read off the whole VM at a glance:

```json
{
  "boot-source":   { "kernel_image_path": "vmlinux",
                     "boot_args": "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda ro init=/init" },
  "drives":        [{ "drive_id": "rootfs", "path_on_host": "rootfs.ext4",
                      "is_root_device": true, "is_read_only": true }],
  "machine-config":{ "vcpu_count": 1, "mem_size_mib": 512 },
  "vsock":         { "guest_cid": 3, "uds_path": "fc.vsock" }
}
```

The control plane's `spawnMicroVM` (`microvm.go`) creates a per-VM working directory
→ writes the above config (uds/paths all inside that directory) →
`exec.Command("firecracker", "--config-file", cfg, "--api-sock", api)` → `waitHealthy`
(over vsock) → on `destroy`, terminate/kill the process + delete the working
directory. Killing the firecracker process destroys the entire VM, so cleanup is
trivially that simple.

Snapshot load/create can't be expressed via `--config-file` and must go through the
REST API; `restoreMicroVM` drives it with `firecrackerAPI` (HTTP over `AF_UNIX`) —
see §8.

---

## 6. Isolation actually gets stronger (vsock vs a management port)

A TCP-port-mapped container has a dilemma: to let the host reach the in-container
daemon you **must open a management port**, which incidentally opens the guest's
outbound network too. vsock dissolves this: it carries the management channel
**orthogonally to whether the guest has a NIC**. So the microVM can:

- have **no virtio-net at all** → the sandbox code is completely offline
  (`/sys/class/net` has only `lo`; connecting to `1.1.1.1` raises `OSError`), while
  the management channel still flows over vsock. "Manageable *and* network-cut-off"
  — a combination a port-mapped container can't have.
- stack the guest's own kernel on top → the strongest isolation in the project.

(Outbound network for the sandbox, when wanted, would be a deliberate opt-in via
virtio-net — a future enhancement, not the default.)

---

## 7. One-time setup (kvm group + artifacts)

The microVM needs `firecracker` + a vsock-capable guest kernel under `vendor/`, and
access to `/dev/kvm`.

```bash
# 1) Join the kvm group so firecracker can open /dev/kvm without sudo (one-time),
#    then restart WSL to apply it: in Windows PowerShell run `wsl --shutdown`, reopen the terminal.
sudo usermod -aG kvm "$USER"

# 2) Put the artifacts under vendor/:
#    - vendor/firecracker : the static binary (a GitHub release; this repo used v1.16.0)
#    - vendor/vmlinux     : a guest kernel with virtio-vsock / virtio-blk / ext4 / devtmpfs
#                           built in (=y) -- e.g. a Firecracker CI kernel (this repo used 6.1.155);
#                           verify against its .config before downloading.

# 3) Build the VM rootfs (and optionally a warm snapshot):
docker build -t microsandbox-agent .   # the agent image the rootfs is exported from
scripts/build-rootfs.sh                # export the ext4 rootfs (no root needed)
scripts/build-snapshot.sh              # optional: a warm snapshot for millisecond restore
```

`tests/conftest.py` builds + runs the control plane for the VM cases and guards them
on "go toolchain + firecracker binary + vmlinux present and `/dev/kvm`
readable/writable"; if any is missing, the VM cases skip as a group. The vsock-bridge
unit tests now live in Go (`control-plane/proxy_test.go`, run with
`go test ./control-plane`) and need none of this. On machines without KVM, `pytest`
therefore still completes.

---

## 8. Measured records & snapshot restore (this machine, WSL2)

It works; the key facts and the pitfalls hit (the part most valuable for learning):

- **Materials**: firecracker v1.16.0 + Firecracker CI's vmlinux 6.1.155 (vsock /
  virtio-blk / ext4 / devtmpfs all `=y`, checked against `.config`). The rootfs is
  built from the agent image via `docker export` + `mkfs.ext4 -d`, packaged without
  root (~250MB).
- **Cold start**: from constructing `Sandbox()` to the daemon being ready, stably
  **~0.94s** (firecracker process start + guest kernel boot + python startup + the
  daemon listening on vsock). Sub-second even with a whole extra guest kernel — the
  payoff of the microVM keeping only virtio.
- **Pitfall 1 · empty `sys.executable`**: when the daemon starts as PID 1 and init
  doesn't set `PATH`, Python can't compute its own executable path, so the Jupyter
  kernel manager (and `commands.run`'s shell) can't spawn subprocesses. Fix: in
  init, `export PATH=...` and exec python with an absolute path (see
  `scripts/build-rootfs.sh`'s /init).
- **Pitfall 2 · loopback down by default**: the Jupyter kernel speaks ZMQ over
  `127.0.0.1`, but in the microVM `lo` is down by default → "kernel startup 60s
  timeout". The minimal rootfs has no `ip`/`ifconfig`, so the daemon brings `lo` up
  with a `SIOCSIFFLAGS` ioctl (see `server._ensure_loopback_up`).

### Snapshot restore

Firecracker's signature trick: save the "booted and warmed-up" VM state to disk,
and on restore skip the kernel boot for millisecond-scale readiness.

- **Build** (`scripts/build-snapshot.sh`): boot a base VM → warm up the Jupyter
  kernel (run a `pass` to force it up) → `PATCH /vm {Paused}` → `PUT
  /snapshot/create {Full}`. Two artifacts: `vendor/snapshot/vmstate` (~13KB
  device/CPU state) + `memfile` (512MB guest memory, **including the hot kernel**).
- **Restore** (`Sandbox(from_snapshot=True)` → the control plane's `restoreMicroVM`):
  start an empty firecracker → drive the state back in via `PUT /snapshot/load` +
  `resume_vm` (over the REST API, since `--config-file` can't express it).
- **Measured comparison**:

  | Path | Ready | First run_code | To first result |
  |------|-------|----------------|-----------------|
  | Cold start (`spawnMicroVM`) | ~0.94s | ~0.8s (incl. kernel cold start) | ~1.77s |
  | Snapshot restore (`restoreMicroVM`) | **~0.03–0.04s** | ~0.13s (kernel already hot) | **~0.17s** |

  To first result, about **10× faster**; readiness itself about 30×.
- **Disk**: a snapshot stores only memory + device state — the disk contents are
  still provided by the host's `rootfs.ext4`, so on restore it must still be at the
  original path (which is why `restoreMicroVM` still validates the rootfs).
- **Known limitation (single instance)**: the vsock uds path in the snapshot is
  fixed, so only one VM can be restored at a time. Concurrent restore + a warm pool
  (one base snapshot forked into N second-scale sandboxes) needs a per-VM uds
  override — a future enhancement.
