# Stage 12 design: TAP/netns networking (give every sandbox a real network identity)

> Status: **agreed direction.** The second of the roadmap's *deferred* stages, and the one
> that **reverses Decision D1** (vsock-first). Every sandbox stops being "fully offline,
> reachable only over vsock" and gains a real virtio-net NIC, a private IP inside its own
> network namespace, and an in-VM daemon that listens on **TCP** — reached by real
> `<port>-<sandboxID>` hostnames through the existing two-hop proxy. This also unlocks
> **user-port exposure** (a server the sandbox code starts on `:8000` becomes reachable at
> `8000-<id>`). Read `docs/MICROVM_DESIGN.md` (§3 vsock, §6 "no NIC"), `docs/STAGE9_DESIGN.md`
> (the proxy/catalog topology), `docs/STAGE11_DESIGN.md` (the ConnectRPC daemon), and
> `docs/E2B_ALIGNMENT_ROADMAP.md` (§4 D1) first. Three sub-steps (12a → 12b → 12c).
>
> **Scope (chosen with the user): networking only.** UFFD lazy snapshot restore and the
> storage swaps (SQLite→Postgres, in-mem→Redis, Local→object storage) the roadmap grouped
> under "Stage 12" are **explicitly deferred to their own later stages** — they are orthogonal
> to networking, and the storage swaps would pull in external service dependencies that break
> the project's "single machine, no external deps, VM cases auto-skip" testing property. The
> novel, load-bearing lesson of Stage 12 is the network pivot; this doc covers only that.

## 1. Goal & non-goals

**Goal.** Replace the vsock data path with a real per-sandbox network, matching E2B's shape:

- **Per-sandbox TAP + network namespace.** Each microVM gets a virtio-net NIC backed by a
  host **TAP** device, set up inside its **own netns**, with a fixed private guest IP. A
  veth pair bridges the netns to the host root namespace; host-side iptables DNAT makes the
  VM reachable at a per-slot routable address.
- **`envd` + `code-interpreter` on TCP.** The in-VM daemon listens on TCP `:49983` (envd) and
  `:49999` (code-interpreter) — E2B's actual ports — instead of vsock 1024/1025. The
  `daemon/main.go` `--addr` path already proves the daemon serves over TCP; this makes the
  in-VM path use it.
- **`<port>-<sandboxID>` hostname routing.** `client-proxy` routes by parsing the `Host`
  header (`49983-<id>`, `49999-<id>`, or any `<userport>-<id>`) instead of the `X-Sandbox-Id`
  header; the orchestrator dials `<slot-ip>:<port>` over TCP. The Stage-11 `/codeinterpreter.`
  path-prefix routing is retired — the **port in the hostname** now selects the service.
- **User-port exposure.** The same `<port>-<id>` mechanism reaches *any* guest port, so a
  server the sandbox starts on `:8000` is reachable at `8000-<id>`. The SDK gains
  `sandbox.get_host(port)`.

**Non-goals** (bounded out / single-machine simplifications):

- **No UFFD lazy restore, no storage swaps.** Deferred to later stages (see Scope above).
- **No real DNS / TLS.** On one box, `<port>-<id>` is carried in the **`Host` header** (the
  SDK overrides it while still connecting to `client-proxy`'s known address). E2B's
  `client-proxy` also just parses the `Host` header; the wildcard DNS + LB + cert are upstream
  cloud infrastructure we don't reproduce — the same header-mode simplification Stage 9 used.
- **No outbound internet by default.** The slot installs **DNAT only** (inbound reachability),
  **not** MASQUERADE, so the sandbox is reachable but still **cannot phone home** — preserving
  most of the "offline" isolation property. Outbound egress is a deliberate, documented opt-in
  (Decision 6), not the default.
- **No auth, no multi-host.** As before.

## 2. Target architecture (Stage 12 end state)

```
 SDK ── Host: 49983-<id> (envd) / 49999-<id> (code-interpreter) / <userport>-<id> ──┐
  │  (still POSTs to client-proxy's TCP address; only the Host header carries the route)
  ▼
 client-proxy ──parse Host <port>-<id>──▶ catalog(id→node) ──▶ orchestrator
                                                                  │  TCP dial <slot-ip>:<port>
                                                                  ▼
   host network slot (per sandbox, in pkg/network):                VM (one daemon, two TCP listeners):
     netns msb-ns-<n>                                                • envd            :49983
       ├─ tap0  ── virtio-net ──▶ VM eth0 (fixed IP, e.g. 169.254.0.21)  • code-interpreter:49999
       ├─ veth peer  10.M.n.2/30   (DNAT 10.M.n.2:<port> → 169.254.0.21:<port>)
       └────────────────────────────────────────────────┐
     host root ns: veth host  10.M.n.1/30  ◀─ orchestrator dials 10.M.n.2:<port> (routed via the veth)
```

The guest IP is **identical across all VMs** (baked into the snapshot); uniqueness lives
entirely host-side in the per-slot veth address + DNAT. This is the whole reason for the netns
— see Decision 1.

## 3. Key design decisions

### Decision 1 — per-VM **netns**, because the snapshot bakes a fixed guest IP (the `vsock_override` parallel)

A microVM snapshot freezes the *entire* guest, including its network config: every VM restored
from one snapshot comes up with the **same** eth0 IP and MAC. To run N of them concurrently
(the warm pool, Stage 5) without an IP/MAC collision, each VM lives in its **own network
namespace**, where identical addresses don't conflict.

This is the exact structural twin of **Stage 5's `vsock_override`** (`fc.go` `Restore`): the
snapshot baked a fixed vsock UDS path, so we gave each restored VM its own UDS; the snapshot
bakes a fixed guest IP, so we give each restored VM its own netns. Same problem, same shape of
fix — and the `vsock_override` machinery is *removed* this stage because the network slot
replaces it as the per-VM isolation mechanism.

### Decision 2 — a host-side **`pkg/network`** slot abstraction (mirrors E2B's `pkg/network`)

A new leaf package owns one verb pair: **allocate** a slot (pick a free index `n`; create
netns `msb-ns-<n>`, `tap0`, the veth pair, the addresses, and the DNAT rule) and **free** it
(tear all of that down). It mirrors E2B's `orchestrator/internal/sandbox/network` slot pool and
is structured like `pkg/pool` (a bounded set of reusable slots). The orchestrator allocates a
slot in `server.create`, stores it on the `fc.MicroVM` handle, and frees it in `destroy` /
`destroyAll`. Allocate-on-demand first; a pre-warmed slot pool is a noted later optimization
(it parallels the warm VM pool, but is not needed for correctness).

The setup is done with the host's `ip` / `iptables` (shelled out, the same way `fc` shells out
to `firecracker` and the build scripts shell out to `docker`) — no new Go networking library.
Slot setup + teardown are unit-testable in Go *only where they don't need CAP_NET_ADMIN* (name
derivation, address arithmetic, the command plan); the live netns path is exercised by the
real-VM e2e behind the privilege gate (Decision 7).

### Decision 3 — guest eth0 configured by the kernel `ip=` boot arg (no `ip` binary in the guest)

The minimal rootfs has no `ip`/`ifconfig` — which is exactly why `daemon/loopback.go` raises
`lo` with a raw `SIOCSIFFLAGS` ioctl. Rather than add netlink address/route code to the daemon,
we let the **guest kernel** configure eth0 at boot via the kernel command line, appended to
`boot_args` in `fc.Spawn`:

```
ip=169.254.0.21::169.254.0.1:255.255.255.252::eth0:off
```

The IP is the same for every cold-started VM (uniqueness is host-side, Decision 1), so this is a
constant string. The snapshot path needs nothing here — the configured eth0 is already in the
frozen guest state. `loopback.go` stays (the Jupyter kernel still talks ZMQ over `127.0.0.1`).

### Decision 4 — `client-proxy` routes by `Host: <port>-<sandboxID>`; the orchestrator dials TCP

- **`client-proxy`** (`cmd/client-proxy/proxy.go`): parse `r.Host` of the form
  `<port>-<sandboxID>[.suffix]` → `(port, id)`. Catalog lookup `id→node` is **unchanged**;
  reverse-proxy to the node, carrying the port (an `X-Sandbox-Port` header is the simplest
  internal hand-off, leaving the catalog interface untouched). A malformed host is 400, an
  unknown sandbox 404 — same error shape as today's missing/unknown `X-Sandbox-Id`.
- **orchestrator** (`cmd/orchestrator/dataproxy.go`): look up the VM's **slot** by id, and
  reverse-proxy to `<slot.routableIP>:<port>` over **plain TCP** — replacing the
  `proxy.VsockProxy` round-tripper. The `/codeinterpreter.` path-prefix branch is **deleted**;
  the port carries the service now. `FlushInterval: -1` stays, so code-interpreter's streamed
  `Execute` keeps flushing live.
- **`pkg/proxy`** loses its hand-rolled vsock `CONNECT` round-tripper; the TCP path is an
  ordinary `httputil.ReverseProxy`/`net.Dial("tcp", …)`, a real simplification.

### Decision 5 — the SDK sets the `Host` header; `get_host(port)` exposes user ports

`src/microsandbox/client.py` replaces the `X-Sandbox-Id` header (`_data_headers`) with a
`Host: <port>-<id>` header: `_envd` calls target `49983-<id>`, `_stream` (code-interpreter)
targets `49999-<id>`. The SDK still connects to `client-proxy`'s address (`data_url`), only
overriding the `Host` header — so **no DNS is needed on one machine**. A new
`sandbox.get_host(port) -> "<port>-<id>"` lets callers reach a user server (e.g.
`http://<get_host(8000)>/…` via `client-proxy`). `Sandbox.run_code/files/commands` signatures
are unchanged; the ConnectRPC method paths are unchanged (still served by envd on its port).

### Decision 6 — DNAT-only by default: reachable, but still cannot phone home

The slot installs a **DNAT** rule (host inbound `<slot-ip>:<port>` → VM) but **no MASQUERADE**,
so the host (and thus the user, via the proxy) can reach into the VM, while the **sandbox code
still has no route out** to the internet. This keeps most of the headline "fully offline"
isolation property even as we add a NIC — a deliberate safety choice. Outbound egress (a
MASQUERADE rule + a permissive route) is a documented opt-in flag, off by default. The docs'
"fully offline" claim is reworded to "**inbound-reachable, outbound-denied by default**."

### Decision 7 — the orchestrator runs **privileged** (CAP_NET_ADMIN in the root netns); rootless is rejected

**Settled by the 12a privilege spike.** Creating the per-sandbox netns + TAP + veth + DNAT, and
launching firecracker into the netns (`ip netns exec`), needs **CAP_NET_ADMIN** (+ CAP_SYS_ADMIN
to enter the netns) **in the host root namespace** and `/dev/net/tun`. The spike confirmed:

- **The E2B-faithful veth+netns+DNAT mechanism works on this kernel** — proven entirely inside a
  user-ns (no real root): a host-ns→child-ns veth round-trips (ping 0.088ms, HTTP 200), and a
  DNAT `vethIP:port → vmIP:port` (the TAP→VM hop) returns HTTP 200 on the nf_tables NAT backend.
  So the exact `ip`/`iptables` sequence `pkg/network` will shell out is validated.
- **Rootless is rejected**: `pasta`/`slirp4netns` are not installed (a new dep), and a veth in a
  user-ns cannot bridge to the *real* root ns where the SDK lives — a no-root path would both
  pull in a dependency and diverge from E2B's veth+iptables lesson.

**Decision (chosen with the user): the orchestrator runs as root** (E2B's "privileged node agent"
model). On this single dev box that is a one-time passwordless `/etc/sudoers.d/microsandbox` entry
letting the e2e fixture launch it with `sudo -E`; it then does all netns/veth/iptables work and
`ip netns exec firecracker` itself (firecracker-as-root also makes `/dev/kvm` trivial). The `-E`
(plus `SETENV` + `!secure_path` in the drop-in) preserves the developer's PATH/HOME so the
orchestrator's in-process template builder still finds `go`/`docker`/`mkfs` and its warm Go caches
— a sanitized root PATH (`secure_path`, no `/usr/local/go/bin`, `HOME=/root`) would otherwise break
`POST /templates`. This is a
**one-time setup like the `kvm` group**, documented next to it; `fc.CheckHostArtifacts` grows a
`/dev/net/tun` + privilege probe, and the e2e gates the VM cases on it exactly as on `/dev/kvm`
(missing ⇒ skip the group, so `pytest` still completes). Narrowing to a least-privilege setcap'd
`msb-netsetup` helper is a noted later refinement (it complicates the netns-enter for firecracker,
so it is not the first cut).

**WSL proxy gotcha (spike-found).** The autoProxy `http_proxy` intercepts traffic to the
per-sandbox `10.x` addresses — the `no_proxy` `10.*` glob is honored by neither curl nor
Go/Python — so the orchestrator's `10.x` VM dial must use a **proxy-bypassing transport**
(`http.Transport{Proxy: nil}`). The SDK→client-proxy hop is `127.*`, already bypassed. See the
[[wsl2-proxy-intercepts-loopback]] memory.

### Decision 8 — the parity oracle stays **behavioral** (unchanged since Stage 11)

Stage 11 already ended the byte-stable-protocol discipline. The wire is the same ConnectRPC; only
the *transport under it* moves (vsock → TCP) and *who selects the port* moves (path prefix →
hostname). The e2e suite proves the same `Execution` / file / command behavior over the new
transport — plus one new case: reach a user server at `<port>-<id>`.

## 4. Code "from → to" map

| Now (Stage 11) | Stage 12 |
|---|---|
| `fc.go`: `vsock` device, no NIC; `Restore` uses `vsock_override` | `network-interfaces` (TAP) + `ip=` boot arg; firecracker launched **inside the slot's netns**; `vsock_override` removed |
| `fc.go` `VsockPort`/`CodeInterpreterVsockPort` (1024/1025) | TCP ports 49983/49999; `MicroVM` carries its `network.Slot` |
| `fc.CheckHostArtifacts` (firecracker/vmlinux//dev/kvm) | + `/dev/net/tun` + CAP_NET_ADMIN probe |
| `daemon/main.go` two **vsock** listeners; `vsock.go` (`mdlayher/vsock`) | two **TCP** listeners (`:49983`/`:49999`); `vsock.go` + the dep removed |
| `daemon/loopback.go` | **unchanged** (kernel ZMQ still needs `lo`) |
| `pkg/proxy` vsock `CONNECT` round-tripper + `WaitHealthy`/`VsockHealthy` over vsock | plain TCP reverse proxy; health probed over TCP at `<slot-ip>:49983/health` |
| `cmd/orchestrator/dataproxy.go` route by `/codeinterpreter.` prefix → vsock port | route by `<port>` (hostname/`X-Sandbox-Port`) → TCP dial `<slot-ip>:<port>` |
| `cmd/client-proxy/proxy.go` route by `X-Sandbox-Id` header | route by `Host: <port>-<id>`; catalog `id→node` unchanged |
| `src/microsandbox/client.py` `X-Sandbox-Id` header + path-prefix | `Host: <port>-<id>` header; new `get_host(port)` |
| `scripts/build-rootfs.sh` `/init`, `boot_args` | rootfs rebuilt (TCP daemon); base **snapshot rebuilt** with eth0 up |
| **NEW** | `services/pkg/network/` (slot allocate/free); orchestrator wires it into create/destroy |
| `catalog`, `store`, `template`, `pkg/build`, ConnectRPC `.proto`s, the kernel | **unchanged** |

## 5. Layout introduced this stage

```
services/
  pkg/network/                # NEW: per-sandbox network slot
    network.go                # Slot{index, netns, tap, veth, routableIP}; Allocate/Free; the ip/iptables plan
    network_test.go           # name/address derivation + the command plan (CAP_NET_ADMIN-free)
src/microsandbox/
  client.py                   # Host: <port>-<id> routing + get_host(port)
```

No new top-level component (unlike Stages 8–11) — Stage 12 is a transport pivot *under* the
existing seams, so it changes `fc`/`proxy`/the two proxies/the SDK and adds one leaf package.

## 6. Three independently verifiable sub-steps

### Stage 12a — give the (cold-started) VM a real NIC **alongside** vsock; daemon also listens on TCP (additive)
Add `pkg/network` (allocate/free a netns+TAP+veth+DNAT slot — the `ip`/`iptables` plan, including
the plain-DNAT reply path, was validated live in the 12a privilege spike: a faithful three-tier
netns model round-trips host→DNAT→VM with **no MASQUERADE**, consistent with Decision 6).
`fc.Spawn` (the **cold-start** path) launches firecracker inside the slot's netns and adds the
virtio-net interface + `ip=` boot arg, **keeping the vsock device**; the `MicroVM` carries its slot
and `Destroy` frees it. The daemon opens TCP `:49983`/`:49999` listeners **in addition to** the two
vsock listeners (both transports serve the same ConnectRPC services). Rebuild the **rootfs** (TCP
daemon). **Scope note:** the snapshot/restore path stays **vsock-only** this sub-step (a snapshot
can't gain a NIC it was not captured with) — rebuilding the base snapshot *with* eth0, and giving
restored/pooled VMs a slot, moves to **12b** where the data path actually flips to TCP. **Verify:**
the full e2e still passes **over the unchanged vsock path** (proving the networked cold-start VM
still boots and serves), plus a host-side probe that the orchestrator reaches `/health` over
**TCP** at `<slot-ip>:49983` (logged, non-fatal in 12a). This absorbs the heaviest, riskiest
plumbing (netns/TAP/iptables + CAP_NET_ADMIN) while the proven vsock path still carries the suite —
Stage 11a's exact "additive first" playbook.

### Stage 12b — flip the data path to TCP; route by `<port>-<id>` hostname
Switch `cmd/orchestrator/dataproxy.go` to dial `<slot-ip>:<port>` over TCP (delete the
`/codeinterpreter.` branch), `cmd/client-proxy/proxy.go` to parse `Host: <port>-<id>`, and the
SDK to send the `Host` header. **Verify:** the whole e2e passes over TCP + hostname routing.
**Validate the riskiest thing first** — code-interpreter's server-streaming `Execute` over the
TCP two-hop proxy (the successor to Stage 11b's streaming-over-vsock risk) — before the rest.

### Stage 12c — user-port exposure; retire vsock; rewrite the safety story
Add `sandbox.get_host(port)` and an e2e case that starts a tiny HTTP server inside the sandbox
(`commands.run`/`run_code`) and reaches it at `<port>-<id>` through `client-proxy`. Remove the
daemon's vsock listeners + `mdlayher/vsock`, `pkg/proxy`'s vsock round-tripper, `fc`'s
`vsock`/`vsock_override`. Update `docs/MICROVM_DESIGN.md` (§6 "no NIC" → the new netns model),
`docs/ARCHITECTURE.md`, `CLAUDE.md`, the README, and `E2B_ALIGNMENT_ROADMAP.md` — and **rewrite
the safety note**: the sandbox is no longer fully offline; it is inbound-reachable and
outbound-denied by default, still a learning implementation, still not safe for untrusted input.

## 7. Keeping tests green (honest trade-offs)

- **12a is additive and proven by the unchanged vsock e2e** — the safe way to land the big new
  privileged network machinery before anything depends on it.
- **The new privilege gate is the biggest practical hurdle** (Decision 7), **de-risked by the 12a
  spike**: the veth+netns+DNAT mechanism is verified on this kernel and the orchestrator-runs-root
  model is chosen. The e2e gates on `/dev/net/tun` + the privilege like it gates on `/dev/kvm`
  (missing ⇒ skip the group).
- **The base snapshot must be rebuilt** (it now bakes eth0). Like every daemon-touching stage,
  each sub-step needs `build-rootfs.sh` (+ `build-snapshot.sh`) before the real-VM e2e, and every
  built template's rootfs/snapshot too — the fixture only builds a rootfs when absent, so a stale
  one silently runs the old (vsock) daemon.
- **Streaming-over-TCP is the key risk** (12b): the same flush mechanism as today's vsock stream,
  but validate it first before building the rest of 12b on it.
- **Go units stay KVM/CAP-free**: `pkg/network`'s name/address derivation + command plan, the
  Host-header parse in `client-proxy`, the port→service mapping — all testable without a netns,
  mirroring how `pkg/proxy`'s host-parsing is tested today.
- **Safety note carried forward and sharpened:** adding a NIC narrows the "fully offline"
  property — the single most security-relevant change in the project's history. DNAT-only by
  default keeps egress shut; the docs must stop saying "fully offline" and must keep saying this
  is a learning implementation, not security-audited, not safe to expose to untrusted input.
```