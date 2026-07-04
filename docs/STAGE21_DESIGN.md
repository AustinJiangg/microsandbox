# Stage 21 design — NBD-served rootfs (lazy block streaming + portable snapshots)

> Status: **DONE (21a/21b/21c). Real-VM e2e 44/44 in `--nbd` s3 mode.** This was a **re-sequencing**:
> Stage 20 (memfile COW) is paused mid-flight (20a landed, 20b-1 done but uncommitted) because its
> faithful live-VM producer needs a **stable rootfs device path**, which vanilla Firecracker does not give
> a materialized rootfs file (a snapshot bakes the rootfs's *absolute path* and `snapshot/load` cannot
> override it — verified against the Firecracker docs). E2B solves this by serving the rootfs over a
> **userspace NBD block device**. So NBD landed first, as its own stage; Stage 20 resumes after.
>
> **Honest headline:** this stage stops materializing the whole rootfs at boot (Stage 15/18/19 assembled
> it to a baked local path) and instead **streams it lazily, block by block, from object storage** over a
> kernel NBD device backed by our existing `pkg/storage/header` COW mapping + chunked bucket reader — the
> disk-side analogue of the Stage-13/15 UFFD memfile. On one box this is fidelity + a real streaming
> mechanism, not a latency win: unpooled restore is in fact *slower* (~3.5s vs ~1.6s) because the guest
> now faults its working set over NBD from the bucket on first access; the warm pool hides it. The
> measurable claim is "the rootfs is no longer copied whole to a local file before boot."
>
> **Two decisions changed when the design met reality (both toward simpler + more E2B-faithful sequencing):**
>
> 1. **Read-only, not the writable overlay of D2 (deferred to Stage 20).** D2 wanted the guest to mount
>    root `rw` over a writable overlay from the start. But our snapshots are built by a *fresh cold boot →
>    warm → snapshot* (`build-snapshot.sh`), not E2B's *resume-and-re-snapshot*. Making the guest write to
>    root then requires the snapshot's captured RAM to stay consistent with the served base across the
>    boundary — which is exactly what E2B's resume-based producer guarantees and ours (Stage 20) does not
>    yet. So Stage 21c serves the rootfs **read-only** (guest root `ro`, unchanged behavior): provably
>    consistent (served bytes == snapshot-time bytes), zero ext4 risk, and it needs **no `build-snapshot`
>    change and no snapshot rebuild**. The writable `Overlay`/`Cache`/`ExportToDiff` (21b) are built and
>    unit-tested, wired the same way, waiting for Stage 20 to capture the overlay diff *with* the snapshot.
> 2. **Bind-mount the device over the *existing* baked path, not a new constant `/fc-vm/rootfs.ext4`.**
>    The design (following E2B's tmpfs-symlink) proposed baking a constant path into every snapshot and
>    rebuilding them all. We avoid that entirely: the snapshot already bakes `tmpl.Rootfs` (constant per
>    template), so Restore just `mount --bind`s the VM's `/dev/nbdX` over that path **inside a per-VM mount
>    namespace** — firecracker opens the baked path and gets the device. N VMs restoring one snapshot each
>    bind a different device over the same path in their own mount ns (the D3 isolation, verified by
>    `test_concurrent_restores_are_isolated` over NBD). No path constant, no snapshot migration.

## 1. Where this sits

| artifact | stored today | served today | this stage |
|---|---|---|---|
| rootfs | COW **diff** over base (Stage 18/19) | **assembled whole** to the baked local path (`MaterializeLayered`) at boot | **streamed lazily** over a kernel NBD device, block-by-block from the bucket, resolved through the same COW header |
| memfile | compacted / (Stage 20) COW diff | streamed lazily over UFFD | unchanged (Stage 13/15/20) |
| snapfile | whole | materialized whole | unchanged |

The rootfs was materialized whole from Stage 15 on because "a Firecracker snapshot bakes in its rootfs's
absolute path" (`pkg/storage/storage.go` package doc). NBD dissolves exactly that: the snapshot bakes a
**constant** path that a per-VM symlink points at a freshly-allocated `/dev/nbdX`, so the rootfs stops
being a baked host file. That is also the precondition Stage 20's memfile-COW producer was waiting on (a
stable device path lets a snapshot be restored/resumed against a lazily-served layered rootfs).

## 2. What E2B actually does (verified against `e2b-dev/infra` @ `b4ba014b1`, real source)

A subagent read the real orchestrator. The mechanism, with code evidence:

1. **Constant baked path + a symlink chain to `/dev/nbdX` (the crux).** The FC drive is set with
   `PathOnHost = "/fc-vm/rootfs.ext4"`, a **constant** (`fc/config.go` `SandboxDir=/fc-vm`,
   `SandboxRootfsFile=rootfs.ext4`; `fc/client.go:162` `setRootfsDrive`; drive is `IoEngine:"Async"`,
   `IsRootDevice:true`). That constant resolves through two hops: a **tmpfs symlink inside the FC mount
   namespace** (`fc/script_builder.go` `startScriptV2`: `mount -t tmpfs … /fc-vm; ln -s <link> /fc-vm/rootfs.ext4`)
   and a **host symlink** re-pointed at the device just before the drive is set (`fc/process.go:276`
   Create / `:341` Resume: `SymlinkForce(providerRootfsPath, SandboxCacheRootfsLinkPath())`, where
   `providerRootfsPath = /dev/nbd{slot}`). **The snapshot never records the device node or a content
   file** — only the constant path — so restoring on any host just needs a free NBD slot symlinked in.
   **Firecracker is stock** (`fc-versions/build.sh` builds upstream; the Go SDK is only the HTTP client).
2. **Kernel NBD client + a userspace server.** `/dev/nbdX` driven via **netlink** (`Merovius/nbd/nbdnl`),
   **multiconn (4 socketpairs)** bound with `nbdnl.Connect(idx, socks, size, flags…)`
   (`nbd/path_direct.go`, block size 4096). Each socket's server end runs a `Dispatch` goroutine
   (`nbd/dispatch.go`) that parses the 28-byte NBD request (magic `0x25609513`), handles
   `Read/Write/Trim`, and writes replies (magic `0x67446698`) — delegating to a
   `Provider{io.ReaderAt, io.WriterAt, Size()}`. Devices come from a **pool** over `/dev/nbdX`
   (`nbd/pool.go`): `modprobe nbd nbds_max=N`, free-slot detection via sysfs
   (`/sys/block/nbdN/{pid,size}`), a ready-slot channel.
3. **Reads resolve through the COW header to a chunked bucket object.** kernel → `Dispatch.cmdRead` →
   `Overlay.ReadAt` (cache-first) → read-only `Storage`/`build.File.ReadAt` →
   `header.GetShiftedMapping(off)` returns the **owning build's id + physical offset** → that build's
   `StorageDiff` (`{buildID}/rootfs`) → a `Chunker` that fetches a **1 MiB chunk** on a local-cache miss
   and mmaps it into a sparse cache file; `uuid.Nil` (zero/gap) blocks are served **without any fetch**.
   This is the *same* per-block owner machinery as the memfile.
4. **Guest writes → a per-sandbox writable overlay (block COW).** `block.Overlay` wraps a read-only base
   + a writable `block.Cache` (a sparse, mmap'd file): read cache-first-then-base, **write cache-only**
   (`block/overlay.go`, `block/cache.go`); the shared base objects are never mutated. On pause the cache
   is exported to a diff (`cache.ExportToDiff` → dirty non-empty blocks + a `{Dirty,Empty}` bitset),
   which becomes the new layer's rootfs object.
5. **A layer is one live-VM run producing BOTH diffs.** Building a layer **resumes the parent layer's
   Full snapshot** (memory via UFFD + rootfs via NBD, both from the parent — self-consistent), runs the
   layer's command **in the guest** (envd), `sync`s, pauses, takes a **Full** snapshot, and diffs **both**
   the memfile (UFFD dirty pages) and the rootfs (overlay export) against the parent header, keyed to the
   new build id (`sandbox.go` Pause; `template/build/phases/steps/builder.go`;
   `diffcreator.go`). **This is Stage 20's faithful producer** — and it is not "base RAM + foreign child
   disk"; it is a self-consistent resume + an in-guest command.

**Correction banked:** E2B does **not** graft a base's RAM onto a different child's rootfs. The stable
NBD path is what makes *resuming any snapshot against a lazily-assembled multi-build rootfs* possible; the
small/meaningful memfile diff comes from *resuming the parent and running one command in-guest*, not from
a mismatch. (This reframes Stage 20's D5 — see §9.)

## 3. What we already have that this reuses

- **`pkg/storage/header`** (`Locate`, `MergeMappings`, `NormalizeMappings`, the v2 owner mapping) — E2B's
  `GetShiftedMapping`/`BuildMap` is our `Locate`/`BuildMap` almost 1:1 (Stages 17–18). The NBD read path
  is a thin loop over `Locate`.
- **The chunked bucket reader** (`uffd.NewChunkedSource`, `source_bucket.go`, 1 MiB) + the layered
  multi-owner reader (`uffd.NewLayeredSource`, Stage 20a) — the exact per-owner, chunk-cached read the
  NBD `Provider` needs, just addressed by a block device instead of a page fault.
- **`storage.MaterializeLayered` / `assembleRuns`** (Stage 18/19) — the whole-rootfs assembler NBD
  *replaces* at boot; its per-owner resolution logic is the blueprint for the lazy provider.
- **Per-VM isolation via a namespace** — `fc.startFirecracker` already launches firecracker inside the
  slot's **netns** (`ip netns exec`); we add a **mount ns** the same way for the constant-path tmpfs.
- **A root orchestrator** — already runs as root (sudoers), so it can `modprobe nbd` and open `/dev/nbdX`
  with no new privilege (like it manages netns today). `nbd.ko` is present in this box's WSL2 kernel.

## 4. The gaps (mapped to code)

1. **No NBD subsystem.** New `services/pkg/nbd`: a device pool over `/dev/nbdX` (modprobe, sysfs free
   detection, bitset, ready channel) + a userspace `Dispatch` server (parse the 28-byte NBD request,
   `cmdRead/Write/Trim` → a `Provider`) + the kernel bind (netlink `nbdnl`, multiconn).
2. **No read-through COW block stack.** New `services/pkg/block` (or under `pkg/nbd`): `Overlay` (RO base
   + writable `Cache`), the RO base = a `Provider` resolving each offset via `header.Locate` → the owning
   build's chunked bucket object (reuse the Stage-20a reader), the writable `Cache` = a sparse mmap file.
   (`ExportToDiff` is the Stage-20-producer hook — added here but exercised later.)
3. **fc still materializes + bakes a per-template path.** `fc.Spawn`/`fc.Restore` set
   `path_on_host: tmpl.Rootfs` (`is_read_only:true`) and `prepareRestore`/`prepareSpawn` call
   `MaterializeLayered` to a baked path. NBD replaces both: allocate a device, build the provider, start
   the server, symlink a **constant** baked path → `/dev/nbdX` inside a per-VM mount ns, boot/restore.
4. **Snapshots bake a per-template path.** Every template's snapshot must be **rebuilt** to bake the
   constant rootfs path (`build-snapshot.sh` + the seeded default), a one-time migration (like Stage 18's
   rebuild). Cold start (`Spawn`) and the warm pool inherit the same constant-path + symlink.

## 5. Decisions

- **D1 — kernel NBD via netlink multiconn + `Merovius/nbd/nbdnl` (E2B-faithful) [DECIDED].** Use the
  kernel `/dev/nbdX` client bound over netlink (`nbdnl`), 4 connections, our own `Dispatch` server —
  exactly E2B's `path_direct.go`. This adds the **first new runtime Go dependency since the storage/uffd
  stack** (`github.com/Merovius/nbd/nbdnl`), a deliberate exception to the hand-rolled-zero-dep discipline
  in favor of E2B fidelity (the classic single-conn `ioctl(NBD_SET_SOCK)/NBD_DO_IT` alternative was
  rejected). The `Dispatch` server, pool, and block stack are still hand-rolled.
- **D2 — writable overlay from the start (E2B-faithful) [DECIDED].** The NBD rootfs is **writable**: an
  `Overlay` over a read-only chunked base + a per-VM writable `Cache` (sparse mmap file); guest disk
  writes land in the cache (base objects immutable), and `ExportToDiff` on pause yields the dirtied
  blocks — E2B's exact model, and the write path the Stage-20 in-guest producer needs. **Consequence:**
  the guest must mount root **`rw`** for the overlay to be exercised (today it is `ro` with writes to the
  in-VM tmpfs `/tmp`). So this stage flips the guest `boot_args` from `root=/dev/vda ro` to `rw` and the
  rootfs drive from `is_read_only:true` to writable; ext4 rw-mount journal/superblock writes then land in
  the per-VM overlay cache (a few dirty blocks per VM), not the shared base. `/tmp` may stay a tmpfs, but
  root writes now persist in the overlay for the VM's life (exported on pause). This is a real change to
  the guest filesystem semantics — chosen for fidelity over the simpler read-only path.
- **D3 — constant baked path + per-VM mount namespace.** Bake `/fc-vm/rootfs.ext4` (constant) in every
  snapshot; at (re)start, `unshare`-a-mount-ns, tmpfs `/fc-vm`, symlink the constant path → the VM's
  `/dev/nbdX`. The mount ns is per-VM (like the netns) so N VMs restoring the *same* snapshot each resolve
  the one baked path to *their own* device without collision — E2B's `startScriptV2` trick.
- **D4 — reuse `pkg/storage/header` + the Stage-20a chunked reader as the `Provider` base**, not a new
  mapping. The NBD `Provider.ReadAt` is a loop over `header.Locate` feeding the per-owner chunked source.
- **D5 — device pool sized to the warm pool.** `modprobe nbd nbds_max=N`; a ready-slot channel like E2B's
  `Populate`. Freed on `Destroy` (disconnect + flush), alongside the netns/UFFD teardown.

## 6. Sub-steps (KVM-free first, the house discipline)

### Stage 21a — `pkg/nbd`: device pool + userspace NBD server (protocol/dispatch)
Device pool (modprobe check, sysfs `/sys/block/nbdN/{pid,size}` free detection, bitset, ready channel,
`GetDevicePath = /dev/nbd%d`) + the `Dispatch` server (28-byte request parse, `cmdRead/Write/Trim` → a
`Provider{ReaderAt,WriterAt,Size}`) + the netlink bind (D1). **KVM-free unit tests:** the dispatch loop
over an in-memory `socketpair` + a `bytes`-backed `Provider` (assert a read request returns the right
bytes, a write lands, protocol framing round-trips); the sysfs free-detection over a fake sysfs tree. The
real device bind needs the `nbd` module → gated like KVM (auto-skip). `go test ./services/...` green.

### Stage 21b — the read-through COW block stack
`Overlay` (RO base + writable `Cache`), the RO base `Provider` = `header.Locate` → the Stage-20a per-owner
chunked bucket reader, the writable `Cache` = a sparse mmap file (write cache-only, read miss →
`BytesNotAvailableError` → fall through to base), and `ExportToDiff` (dirty non-empty blocks → a diff +
`{Dirty,Empty}` mapping, for the Stage-20 producer). **KVM-free unit tests** over the Local provider +
temp files: a layered base assembles the right bytes through the provider; a write is read back from the
cache; an unwritten block falls through to the base; `ExportToDiff` yields exactly the dirtied blocks.

### Stage 21c — wire fc + constant-path/mount-ns/symlink + real-VM e2e + measured win
Rebuild snapshots to bake the constant path; `fc.Spawn`/`fc.Restore` allocate a device, build the base
provider + **writable overlay** (D2), start the NBD server, `unshare` a mount ns + tmpfs + symlink the
constant path → `/dev/nbdX`, flip `boot_args` to `root=/dev/vda rw` + the drive to writable, boot/restore
over NBD; `prepareSpawn`/`prepareRestore` stop calling `MaterializeLayered`. **Real-VM e2e** (nbd module +
KVM): boot the default + a layered template over NBD, assert it boots, a **guest write to root persists**
within the VM (exercising the overlay), code runs, content is present, and the rootfs is **not**
materialized whole (a probe asserts no full local rootfs file + that chunk fetches happened). Honest
🔴/🟡/🟢 review.

## 7. Keeping tests green (honest trade-offs)

- 21a's dispatch/protocol + pool sysfs parsing and all of 21b are **pure Go, KVM-free** — the parity
  oracle stays `go test ./services/...`. The real device bind + the layered-rootfs boot need the `nbd`
  module + KVM, covered by the Python e2e (auto-skips without them, like KVM/netns today).
- **Backward compatibility:** a non-layered / old bucket rootfs (no header) is served as a single-owner
  whole object through the same provider; the local-fs escape hatch still materializes whole (NBD is the
  s3-mode path). Both stay green.
- Same **honesty rule** as Stages 13–20: fidelity + a real mechanism; the single-box restore latency is
  unchanged (net setup dominates). The claim is "rootfs streamed, not materialized whole" + portable
  snapshots — not speed.

## 8. New dependencies

**`github.com/Merovius/nbd/nbdnl`** (D1, decided) — the netlink NBD bind, E2B's choice. The **first new
runtime Go dependency since the storage/uffd stack**, a deliberate exception to the hand-rolled-zero-dep
discipline for E2B fidelity (single-conn `ioctl` hand-roll rejected). The `Dispatch` server, device pool,
and block/overlay stack are all still hand-rolled. No other new deps.

## 9. What this completes / relation to Stage 20

- Lands the **last "materialized whole" artifact** as a lazy stream: after this, rootfs *and* memfile both
  stream from the bucket (rootfs over NBD, memfile over UFFD), resolved by the same `header` COW algebra.
- Makes **snapshots portable** (constant baked path) — the precondition Stage 20's producer waited on.
- **Reframes Stage 20's producer (D5).** With NBD, the faithful E2B producer becomes reachable: *resume the
  parent (memory over UFFD + rootfs over NBD) → run the layer's command **in the guest** → pause → Full
  snapshot → two diffs*. That needs an **in-guest command-execution** path (start/ready-command subsystem)
  we don't have — still a real chunk of work, but no longer blocked on the rootfs path. The pragmatic
  alternative (graft base RAM + child rootfs over NBD + a prime cell) is now *possible* but **diverges**
  from E2B (§2 correction). **Which way Stage 20 resumes is a fork to decide after Stage 21 lands**, not
  here.

## 10. Known divergences from E2B (this stage)

| axis | E2B (real) | this stage | status |
|---|---|---|---|
| device | kernel `/dev/nbdX`, netlink multiconn | same (D1) | 🟢 faithful |
| dispatch | userspace `Dispatch`, `Provider` | same | 🟢 faithful |
| read resolution | `GetShiftedMapping` → owning build's chunk | `header.Locate` → owning build's chunk | 🟢 faithful (our owner index vs uuid, Stage 18) |
| writable rootfs | overlay (RO base + writable cache), root `rw` | **read-only** this stage (guest root `ro`); writable overlay built + wired, deferred to Stage 20 | 🟡 deferred (see status §1) — E2B's `rw` is producer-coupled, which we lack until Stage 20 |
| baked path → device | tmpfs symlink to `/dev/nbdX` in a mount ns (constant `/fc-vm` path, rebuilt snapshots) | **`mount --bind` `/dev/nbdX` over the existing baked `tmpl.Rootfs`** in a per-VM mount ns (no path constant, no snapshot rebuild) | 🟢 faithful (same mount-ns isolation; simpler retrofit — see status §2) |
| Firecracker | stock upstream | stock (v1.16.0) | 🟢 faithful |
| netlink bind | `Merovius/nbd/nbdnl` | same (D1) | 🟢 faithful (new dep, accepted) |
| cross-node cache | NFS-wrapped shared chunks | per-VM local cache | 🟡 multi-host — deferred |

None of these change the storage *seam*; they add the NBD *transport* in front of the same
`StorageProvider`/`header` mechanism, which is why Stages 15/17/18 landed first.

## 11. What shipped

- **21a — `services/pkg/nbd`** (device pool + userspace server + netlink bind). `nbd.go` (`Provider` =
  `ReadAt`/`WriteAt`/`Size`, wire constants), `dispatch.go` (parse the 28-byte NBD request, serve
  `Read`/`Write`/`Trim`/`Flush` → `Provider`, 16-byte simple reply), `pool.go` (`modprobe nbd nbds_max=N`
  + sysfs `/sys/block/nbdN/size` free scan → ready channel), `export.go` (`Bind` = 4 socketpairs to the
  kernel via `nbdnl.Connect`, a `Dispatch` goroutine per socket). New runtime dep `github.com/Merovius/nbd`
  (D1). KVM-free unit tests (dispatch over `net.Pipe`, sysfs over a fake tree) + a gated real-device test
  `TestBindRealDeviceRoundTrip` (`MSB_TEST_NBD=1` + root).
- **21b — `services/pkg/block`** (the COW block stack). `block.go` (`ReadSource` + `NewLayeredBase` reusing
  `uffd.NewLayeredSource`), `cache.go` (per-VM sparse writable `Cache` + `ExportToDiff` → dirty non-empty
  blocks + `{Dirty,Empty}` bitset, the Stage-20 producer's input), `overlay.go` (`Overlay` = read
  cache-first-then-base, write cache-only), `readonly.go` (`ReadOnly` — the read-only provider Stage 21c
  binds; the `Overlay` swaps in for Stage 20's `rw`). Storage-free (injected `OpenFunc`), KVM-free tests.
- **21c — wiring.** fc: `RootfsBacking{Device, Close}`, `Spawn` points the drive at the device, `Restore`
  wraps firecracker in `ip netns exec … unshare --mount sh -c "mount --bind DEV BAKED && exec firecracker"`,
  `Destroy` tears the backing down after firecracker dies. orchestrator: `--nbd` flag → an `nbd.Pool`,
  `buildRootfsBacking` (resolve alias → `openRootfsBase` layered/whole → `block.NewReadOnly` → `nbd.Bind`),
  `prepareSpawn`/`prepareRestore` skip `MaterializeLayered` in NBD mode (restore ensures a placeholder at
  the bind target). `storage.ObjectSize` (whole-object size for a non-layered base, no interface widening).
- **Validated on real VMs (this box, WSL2, firecracker v1.16.0, `nbd.ko`):** cold-start and restore over
  NBD both boot to a healthy daemon (de-risked with two throwaway spikes first, then the full path);
  real-VM e2e **44/44** in `--nbd` s3 mode, including `test_concurrent_restores_are_isolated` (concurrent
  NBD restores in per-VM mount namespaces) and `test_layered_template_via_api` (the multi-owner layered
  base). `test_restore_is_fast` bound loosened (6s in `--nbd`, 2.5s plain) to reflect the honest unpooled
  latency; it stays a "restore doesn't hang" sanity check.

## 12. Deferred (unchanged from the design)

- **Writable rootfs / guest `rw`** — the 21b `Overlay` + `ExportToDiff`, wired but not exercised, land with
  Stage 20's producer (which captures the overlay diff with the snapshot; see status §1).
- **NBD device pool sizing** — a fixed 64; `nbds_max` is a module-load param, so a pre-loaded smaller `nbd`
  caps it (documented `pkg/nbd` caveat), fine for one box.
- **Cross-node chunk cache** — per-VM local cache only (multi-host, deferred with the rest of the roadmap).
