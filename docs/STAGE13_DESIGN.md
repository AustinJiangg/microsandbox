# Stage 13 design: UFFD lazy snapshot restore (become the VM's memory supplier)

> Status: **done (13a + 13b + 13c).** The first of the roadmap's remaining *deferred*
> items (`docs/E2B_ALIGNMENT_ROADMAP.md` §5 "Still deferred"). Today a restored VM gets its
> memory from a `File` backend — firecracker `mmap`s the whole `memfile` and the kernel
> demand-pages it, with **us on the outside**. This stage flips the snapshot-load memory
> backend to **`Uffd`**: guest RAM starts empty, and every first touch of a page faults out to
> **our own user-space handler**, which copies that page in from the `memfile`. We become the
> guest's memory supplier. Read `docs/MICROVM_DESIGN.md` (the snapshot/restore model) and
> `docs/STAGE5_DESIGN.md` (the warm pool, which also rides `Restore`) first.
>
> **Why bother, honestly.** On *one box* this is **not** a latency win: the `File` backend
> already demand-pages from the page cache, and a per-fault user-space round-trip can even be
> marginally slower. The payoff is twofold and both fit the project's "learn the principles"
> charter: (1) you learn `userfaultfd`, the page-fault-interception primitive shared by
> Firecracker, gVisor, CRIU and QEMU post-copy migration; and (2) once *we* supply the pages,
> the source no longer has to be a local file — it can be object storage, a remote node, or a
> page cache shared across the fleet. That is exactly why E2B uses UFFD, and it is the
> precondition for the roadmap's "storage swaps go live" work. This doc does **not** claim a
> speedup; 13b measured the real number and reports it as-is.
>
> **Outcome (measured, this WSL2 box).** Unpooled restore-to-ready is **~0.54s with `--uffd` vs
> ~0.57s with `File` — identical within run-to-run noise** (the ~0.5s of per-sandbox `ip` setup
> from Stage 12 dominates both); the warm pool hands out in **~11–25ms either way** (it pre-warms
> off the request path, so its latency is backend-independent); and the Python e2e passes
> **37/37 on both backends**. As predicted, no single-box speedup — so **`File` stays the default
> and `--uffd` is opt-in** (Decision 3). What's banked is the `userfaultfd` mechanism and the
> now-pluggable page source, not latency.

## 1. Goal & non-goals

**Goal.** Restore a microVM with `mem_backend.backend_type = "Uffd"` instead of `"File"`, served
by a handler we own:

- **A `pkg/uffd` page-fault handler.** A pure-Go handler that: binds a Unix domain socket;
  receives firecracker's `userfaultfd` file descriptor (over `SCM_RIGHTS`) plus the guest
  memory layout (JSON); `mmap`s the `memfile` read-only; then loops, reading page-fault events
  off the uffd fd and serving each with `UFFDIO_COPY` from the right `memfile` offset.
- **`fc.Restore` flips the backend.** It starts the handler (listening *before* `/snapshot/load`,
  a hard ordering requirement), points `mem_backend` at the handler's UDS, and ties the
  handler's lifetime to the `MicroVM` (stopped in `Destroy`).
- **A switch with a `File` fallback.** An orchestrator flag (`--uffd`) selects the backend, so we
  can A/B against `File`, measure, and fall back if WSL2 surprises us. 13b is the decisive
  proof that the whole ioctl path works on this kernel under real firecracker.

**Non-goals** (bounded out / single-machine simplifications):

- **No remote / shared / compressed memory source.** The handler serves from the *local*
  `memfile`, same bytes the `File` backend used. Sourcing pages from object storage or across
  the fleet is the *point* this unlocks, but it belongs to the later "storage swaps" stage.
- **No separate handler process.** The handler runs as a **goroutine inside the orchestrator**
  (chosen with the user — Decision 1), not the per-VM helper *process* the Firecracker/E2B
  reference uses. Fewer moving parts, lifecycle naturally bound to the `MicroVM` struct.
- **No UFFD write-protection / live-migration tricks.** Only the MISSING-page (lazy populate)
  use case. `UFFD_FEATURE_*` beyond what firecracker negotiates is untouched.
- **No claim of a latency improvement.** See the honesty note above.

## 2. Target architecture (who does what)

The division of labour is the whole concept. firecracker owns the uffd; we own the page supply.

```
  ┌─ orchestrator (root) ──────────────────────────────────────────────┐
  │  fc.Restore:                                                        │
  │    1. uffd.Serve(uds, memfile)  ── binds & listens on uds, mmaps memfile (read-only)
  │    2. PUT /snapshot/load { mem_backend:{ backend_type:"Uffd", backend_path:uds }, resume_vm:true }
  │                                                                     │
  │    ┌─ goroutine: uffd handler ─────────────────────────────────┐   │
  │    │  recvmsg(uds) -> uffd fd (SCM_RIGHTS) + JSON mappings      │   │
  │    │  loop: read(uffd fd) -> uffd_msg                           │   │
  │    │    PAGEFAULT@addr -> UFFDIO_COPY one page from memfile     │   │
  │    │    REMOVE[start,end] -> UFFDIO_ZEROPAGE that range         │   │
  │    └────────────────────────────────────────────────────────────┘  │
  └─────────────────────────────────────────────────────────────────────┘
        │ firecracker (in the slot's netns) creates the uffd, registers guest RAM
        │ MISSING, connects the uds, sends fd+mappings, then resumes the vCPUs
        ▼
   VM: guest touches a page that isn't present  ── kernel ──▶  PAGEFAULT event on the uffd fd
                                                  ◀── UFFDIO_COPY fills it ──  vCPU continues
```

The UDS is used **once** (to hand over the fd + layout). Every page fault after that is read
directly off the **uffd fd** — the socket is not in the hot path.

## 3. The wire ABI (pinned to firecracker v1.16.0)

Getting this exactly right is the #1 failure mode (a wrong JSON field name => firecracker's
handshake can't be parsed). Pinned from `src/firecracker/examples/uffd/uffd_utils.rs` @ `v1.16.0`:

```jsonc
// Sent over the UDS as a single message: a JSON array, with the uffd fd as SCM_RIGHTS ancillary data.
[ { "base_host_virt_addr": 140737..., "size": 134217728, "offset": 0, "page_size": 4096 } ]
//    ^ host VA of region base        ^ region bytes      ^ byte offset of region in memfile  ^ bytes (NOT kib in 1.16.0)
```

- **fd transfer:** `recvmsg` with an `SCM_RIGHTS` control message (Rust side: `recv_with_fd`).
  The handler does **not** create or `UFFDIO_API`/`UFFDIO_REGISTER` the uffd — firecracker
  already did. The handler only *serves* an already-registered fd.
- **Per-fault offset:** for a fault at `addr`, align down to the page, find the region whose
  `[base_host_virt_addr, +size)` contains it, then
  `src = memfile_mmap_base + region.offset + (aligned_addr − region.base_host_virt_addr)`,
  and `UFFDIO_COPY` one `page_size` from `src` to `aligned_addr`.

These ioctl numbers / event tags aren't in `x/sys/unix` (only `SYS_USERFAULTFD` is), so
`pkg/uffd` defines them from the kernel ABI (computed via the `_IOWR(type,nr,size)` macro at
init, not hand-typed magic):

| symbol | value (x86_64) | struct | who calls |
| --- | --- | --- | --- |
| `UFFDIO_COPY` | `_IOWR(0xAA,3,40)` = `0xC028AA03` | `uffdio_copy{dst,src,len,mode,copy}` | handler |
| `UFFDIO_ZEROPAGE` | `_IOWR(0xAA,4,32)` = `0xC020AA04` | `uffdio_zeropage{range,mode,zeropage}` | handler (REMOVE) |
| `UFFD_EVENT_PAGEFAULT` | `0x12` | `uffd_msg` (32 B) arg.pagefault.address | event tag |
| `UFFD_EVENT_REMOVE` | `0x9` | `uffd_msg` (32 B) arg.remove.{start,end} | event tag |

## 4. Key design decisions

### Decision 1 — handler is a **goroutine in the orchestrator**, not a per-VM process
Chosen with the user. The Firecracker/E2B reference spawns a separate handler *process* per VM
(good isolation; in production it can serve pages from anywhere). Here the orchestrator already
owns the `*MicroVM` and spawns firecracker itself, so a goroutine per VM keeps the lifecycle
trivially bound to the VM struct, adds **no new binary / vendored artifact**, stays all-Go, and
leaves the pure logic (mapping parse, offset math) unit-testable without KVM. Trade-off: a panic
in the handler could take down the orchestrator — so the serve loop is defensively coded and
recovers, and the unsafe surface (`mmap`, ioctl, raw fd) is confined to `pkg/uffd`.

### Decision 2 — `pkg/uffd` owns the raw kernel ABI
`x/sys/unix` ships `SYS_USERFAULTFD` but none of the `UFFDIO_*` ioctls, structs, or event tags.
`pkg/uffd` defines them (Decision-3 table) and is the *only* place with `unsafe`/syscall code, so
the rest of the tree stays clean. The constants are derived via an `_IOWR` helper so they read as
their kernel definition, not as opaque hex.

### Decision 3 — behind a `--uffd` switch, `File` stays the default until proven
WSL2 runs a custom Microsoft kernel; the `userfaultfd(2)` syscall is present (an unprivileged
call returns `EPERM`, not `ENOSYS`, and `vm.unprivileged_userfaultfd` exists => `CONFIG_USERFAULTFD=y`)
and the orchestrator runs as root (Stage 12) so it can create the fd — but the decisive proof is
firecracker actually resuming a guest over the Uffd backend. So 13b keeps `File` as the default and
puts UFFD behind `--uffd`; we flip the default only after the real-VM e2e passes on this box. A
load/handler failure logs and (for now) surfaces as a restore error rather than silently degrading.

### Decision 4 — `UFFD_EVENT_REMOVE` handled by zeroing
If a page is `madvise(MADV_DONTNEED)`'d away (e.g. a memory balloon — which we don't run, but be
correct anyway), a later fault must get a **zero** page, not stale `memfile` bytes. The handler
tracks removed ranges (or responds to the copy's `EAGAIN`) and serves them with `UFFDIO_ZEROPAGE`.
Without a balloon we may never see a REMOVE; handling it is cheap correctness, not a feature.

### Decision 5 — clean teardown; the uffd fd's EOF is the VM-death signal
`Destroy` kills firecracker first (Stage 12 order: VM, then network slot). When firecracker exits,
the kernel releases the uffd and the handler's `read` returns EOF/`POLLHUP`; the goroutine exits.
A stop pipe (epoll'd alongside the uffd fd) lets `Destroy` also tear the handler down deterministically
and `munmap` the `memfile`, so no fd/mapping leaks across the warm pool's churn.

### Decision 6 — the parity oracle stays **behavioral** (unchanged since Stage 11)
UFFD is a host-side restore-path change, invisible to the wire. The Python e2e suite is the oracle:
a sandbox restored over UFFD must run code, keep kernel state, and expose ports exactly as before.

## 5. Code "from → to" map

| concern | from (today) | to (Stage 13) |
| --- | --- | --- |
| memory backend | `fc.go:227` `mem_backend{File, memfile}` | `{Uffd, <uds>}` when `--uffd` |
| who supplies pages | the kernel, from the `mmap`'d file | `pkg/uffd` handler, via `UFFDIO_COPY` |
| handler lifecycle | — | started in `Restore`, held on `MicroVM`, stopped in `Destroy` |
| kernel ABI | — (none in `x/sys/unix`) | `pkg/uffd` defines ioctls/structs/events |
| switch | — | orchestrator `--uffd` flag (default off in 13b) |

## 6. Layout introduced this stage

```
services/pkg/uffd/
  uffd.go        # ABI (ioctl numbers, structs, event tags), Serve(uds, memfile), the fault loop
  uffd_test.go   # KVM-free: mappings JSON parse, fault-addr -> memfile-offset math, ioctl-number derivation
services/cmd/orchestrator/   # + --uffd flag, threaded into fc.Restore
services/pkg/fc/fc.go        # Restore: start handler + Uffd backend; MicroVM holds the handle; Destroy stops it
```

## 7. Three independently verifiable sub-steps

### Stage 13a — `pkg/uffd` + KVM-free unit tests (the bulk of the new code)
Implement the handler: the ABI definitions, `recvmsg`/`SCM_RIGHTS` fd reception, mapping-JSON
parse, the fault→offset math, and the `UFFDIO_COPY`/`UFFDIO_ZEROPAGE` serve loop. Unit-test the
*pure* parts with no VM: parse a sample `[{base_host_virt_addr,size,offset,page_size}]` body;
assert the offset computed for several fault addresses; assert the derived ioctl numbers equal the
known `0xC028AA03`/`0xC020AA04`. `go test ./services/...` green; nothing wired yet, so the Python
e2e is untouched.

### Stage 13b — wire into `fc.Restore` behind `--uffd`; real-VM proof + measurement
`Restore` starts the handler and flips the backend when `--uffd` is set; `MicroVM` holds the
handle; `Destroy` stops it. Run the real-VM e2e with the orchestrator started `--uffd` and confirm
behavioral parity (run code, stateful kernel, `get_host`). Measure restore latency vs `File` and
record the honest number. Default stays `File` unless the result says otherwise.

### Stage 13c — docs, measured records, warm-pool check, defaults
Finalize this doc's status, update `docs/MICROVM_DESIGN.md` (measured restore numbers + the UFFD
mechanism), `CLAUDE.md`, and the roadmap (move UFFD from "deferred" to "done"). Confirm the warm
pool (which also calls `Restore`) behaves under UFFD. Decide the default (`File` vs `Uffd`) from
13b's data and state why.

## 8. Keeping tests green (honest trade-offs)

- 13a is pure host-side Go: unit-tested without KVM, exactly like `pkg/network`/`pkg/proxy`.
- 13b's UFFD path only runs when the orchestrator is started `--uffd`; the default `File` path is
  unchanged, so the existing e2e stays green regardless. The UFFD e2e is the same Python suite,
  just against an `--uffd` orchestrator.
- The `--uffd` gate means a kernel that can't actually serve faults (should WSL2 surprise us)
  degrades to a clear restore error, never a silently-broken VM — and `File` remains the shipping
  default until 13b proves otherwise.
</content>
</invoke>
