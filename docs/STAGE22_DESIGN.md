# Stage 22 design — the E2B layer producer: run the layer's command **in-guest**, one snapshot → two consistent diffs

> Status: **foundation landed (22a, 22b-1, 22b-2a); the producer (22b-2b) is implemented but blocked on a
> Firecracker+NBD limitation — deferred.** This stage exists because **Stage 20's e2e failed on real KVM**
> (its first run): `test_layered_snapshot_via_api` builds a COW-layered template *with a snapshot*, restores
> it, and reads the file the layer's `RUN` wrote — and the read returned **404** because the Stage-20
> producer grafted the base's RAM onto a separately-built child rootfs, so the base RAM's cached `/etc`
> shadowed the child's disk change. Stage 22 closes it the E2B way (run the layer's command **in-guest**, one
> snapshot → two consistent diffs).
>
> **Landed + verified (this session):** **22a** (`pkg/envdclient`, committed), **22b-1** (the rootfs COW
> producer algebra — `header.MappingFromDirty` + `storage.PublishRootfsDiffBlocks`, KVM-free green), and
> **22b-2a** (**every sandbox now boots a private writable rootfs overlay** — the D2 foundation; full e2e 19
> passed + base-snapshot restore 3 passed incl. concurrent, zero regression).
>
> **22b-2b (the producer) — implemented and proven to WORK up to snapshot, then blocked.** The producer
> resumes the base with the writable overlay, runs the layer's `RUN` command **in the guest** (verified: the
> guest wrote the marker, the build succeeded, both COW diffs were published). But **restoring the produced
> child snapshot panics Firecracker's virtio-blk device**: `InvalidAvailIdx { queue_size: 256, reported_len:
> 12659 }`. Diagnosis (this session): it is specifically **writes to the NBD-backed rootfs before the
> re-snapshot** — Stage-20's read-only re-snapshot restored fine, the file-backed base snapshot restores fine
> (incl. concurrent), only the *written* NBD-backed re-snapshot's block-queue state is rejected on restore.
> This is a deep Firecracker + kernel-NBD live-writable-snapshot consistency issue that needs its own focused
> session (candidate directions in §11). The 22b-2b code (`server.go` producer, `build.go` producer path,
> `test_template.py` assertion, the `envdclient` no-proxy fix) is **held out of the foundation commits** until
> it restores cleanly.

## 1. The problem (why the Stage-20 snapshot is wrong)

A snapshot restores the VM's **RAM**. Stage 20's producer (`LayeredSnapshot`, `server.go:269`) makes the
child's snapshot by **resuming the BASE and re-snapshotting** — so the snapshot's RAM is the *base's* RAM.
Linux keeps a **page cache** in that RAM, including the cached `/etc` directory block. The base never saw
the layer's file, so its cached `/etc` block does not list it. The child's file lives only in the child's
**rootfs**, built *separately* by docker+debugfs (Stage 19). At restore we serve the child rootfs over NBD,
but the guest wakes with **base RAM**: it resolves `/etc/<marker>` from the cached (base) directory block →
`ENOENT`, **without reading the disk**. RAM and disk are inconsistent, and for cached metadata RAM wins.

So Stage 20's split — **RAM from resuming the base, rootfs from docker+debugfs** — produces two artifacts
that were never consistent with each other. That is the bug.

## 2. What E2B actually does (verified against `e2b-dev/infra` @ main, `/home/austin/projects/goproject/infra`)

Every layer is built by a **single running VM**: resume the parent self-consistently, run the layer's
command **inside the guest**, then take **one** Full snapshot from which **both** diffs are computed. Because
the guest itself made the changes, its RAM (page cache) and its disk (the writable overlay) are one
consistent state at the snapshot instant.

- **Resume parent → run command in-guest.** `StepBuilder.Build` resumes the parent for every non-first layer
  (`internal/template/build/phases/steps/builder.go:172`), then runs the step's action in-guest. A `RUN`
  step's command is executed via envd: `commands/run.go:43` → `sandboxtools.RunCommandWithLogger` →
  a Connect `Process.Start` server-stream (`sandboxtools/command.go:134`), success = the stream's `EndEvent`
  with `exit_code == 0` (`command.go:193`).
- **One pause → one Full snapshot → two diffs.** `Sandbox.Pause` (`internal/sandbox/sandbox.go:713`) pauses,
  takes **one Full** snapshot (`sandbox.go:758`, `SnapshotType: ...Full`, `fc/client.go:134`), then derives
  **both** the memfile diff (`sandbox.go:778`) and the rootfs diff (`sandbox.go:796`) from it, returning a
  single `Snapshot{ MemfileDiff, RootfsDiff, Snapfile, … }` (`snapshot.go:14`).
- **Rootfs diff = the writable NBD overlay's dirtied blocks.** Guest writes land in the overlay's cache
  (`block/overlay.go:69` `WriteAt → cache.WriteAt`); on pause `Cache.ExportToDiff` writes only the dirty,
  non-zero blocks (`block/cache.go:108`). **This is the exact shape of our `pkg/block/overlay.go` +
  `ExportToDiff`, built and unit-tested in Stage 21b, waiting.**
- **Memfile diff = UFFD dirty-page tracking (a doc correction).** Prior research (STAGE20 §2.1, and our
  memory) said E2B computes the RAM diff by *userspace block-compare* (`dedupCompare`) against an assembled
  base memfile. **That symbol no longer exists in `main`.** The RAM diff is UFFD fault/dirty tracking:
  `Disable()` marks all blocks dirty just before the snapshot (`block/tracker.go:38`); during the Full
  snapshot dump, pages Firecracker faults through the base handler get their dirty bit **cleared**
  (`tracker.go:47`); the survivors — the guest's touched working set — are the diff (`diff.go:36`, only
  dirty, non-zero blocks). This over-approximates a content diff but is correct. **We keep our
  `header.BuildDiff` block-compare** (it is a legitimate, simpler way to the same "child-owned vs
  zero-owner vs omitted" runs); the correction is to the *documentation of E2B*, not to our algebra.

The load-bearing point for the fix: **the command runs in the guest, so one snapshot captures a mutually
consistent (RAM, disk) pair.** That is precisely what our producer lacks.

## 3. What we already have that this reuses

- **`daemon` envd `processService.Run`** (`envd.go:124`, `runCommand` at `envd.go:64`) — a synchronous
  run-to-completion command with stdout/stderr/exit. A `RUN` build step is exactly this shape (run, check
  exit 0). *We do not need E2B's streaming `Process.Start` for the fix* (see D1).
- **`pkg/block.Overlay`** (`overlay.go`) — read-only base + writable `Cache`, `WriteAt` → cache,
  `ExportToDiff()` → the dirtied blocks. Built + unit-tested (Stage 21b), currently shadowed by
  `block.NewReadOnly` in `buildRootfsBacking` (`nbd.go`).
- **`fc.MicroVM.Snapshot(vmstate, memfile)`** (`fc.go:466`) — pause + Full snapshot, added in Stage 20.
- **`nbd.Bind` + the NBD device pool** (`pkg/nbd`, Stage 21) — binds any `block` provider to `/dev/nbdX`.
- **The whole layered read path** — `storage.PublishMemfileDiff` / `PublishRootfsDiff` (`cow.go`), the v2
  header algebra, the multi-owner UFFD source (`layeredMemSource`, `server.go:415`) and NBD assembly at
  restore. **Restore does not change** — only how the child's two diffs are *produced*.
- **The producer skeleton** — `LayeredSnapshot` (`server.go:269`) already resumes the base
  (`restoreHealthy`), snapshots (`vm.Snapshot`), and publishes the memfile diff. Stage 22 inserts the
  in-guest run + the overlay-diff export between resume and snapshot.

## 4. The gaps, mapped to code

1. **The producer runs no command.** `LayeredSnapshot` resumes the base and immediately snapshots. It must,
   between the two, **run the layer's command(s) in-guest** — so the interface must carry the command.
   `build.Snapshotter.LayeredSnapshot` (`pkg/build/build.go:27`) gains the recipe's `RUN` commands.
2. **The producer's rootfs is read-only.** `buildRootfsBacking` (`nbd.go`) binds `block.NewReadOnly`. The
   producer needs a **writable** binding (`block.NewOverlay(base, cache)`) so the in-guest writes are
   captured, and must call `overlay.ExportToDiff()` after the run to get the child's rootfs diff.
3. **The rootfs diff comes from docker+debugfs, not the run.** For a **layered build *with snapshot***,
   `pkg/build.Build` currently builds the child rootfs via `build-rootfs-layered.sh` and `PublishRootfsDiff`
   (`build.go:130`), *separately* from the memfile. Stage 22 makes the **one-run producer** the source of
   *both* diffs for that path (docker+debugfs stays for cold-start layered builds — see D3).
4. **No Go→envd command client.** The orchestrator reaches a VM's envd at `vm.Slot.RoutableIP:49983` (the
   health check already dials the slot). Add a tiny Connect client to POST `envd.ProcessService/Run`.
5. **The write must be durable before the snapshot.** `echo > file` lands in the page cache; the overlay
   block device may not have it yet. The producer must **`sync` in-guest** after the command so the block is
   in the overlay diff *and* in RAM — otherwise the rootfs diff misses it (see D4).

## 5. Decisions

- **D1 — reuse envd's synchronous `Run`, do not build streaming `Process.Start` yet.** For a `RUN` build
  step (run-to-completion, check exit 0) our existing `Run` is functionally complete. E2B streams because it
  ships live build logs and supports long-running *runtime* start/ready commands — ergonomics and a
  separate product feature, not correctness. This matches the project's ethos (E2B-shaped seams,
  single-box-appropriate impls) and keeps the blast radius on the *fix*. Streaming + start/ready-command is
  filed as Stage 23 (§9). *Chose closer-to-E2B on the mechanism that matters (in-guest execution), simpler
  on the transport.*
- **D2 — every sandbox gets a private writable rootfs overlay (E2B's runtime model); non-NBD is retired as
  the default.** (Chosen by the user over two smaller alternatives — see the fork below.) Restoring a
  read-only-baked snapshot makes the producer, which uses the restore path, unable to write; and a writable
  rootfs is only safe with **per-VM private storage** (the NBD overlay's private cache) — a *shared*
  materialized file mounted rw would corrupt across VMs. So: the base snapshot bakes `root=/dev/vda rw` +
  `is_read_only: false` (build-snapshot.sh), every `--nbd` spawn/restore binds a `block.NewOverlay(base,
  cache)` with a per-VM sparse cache removed on Destroy (`buildRootfsBacking`), and `--nbd` becomes the
  orchestrator default. Writability is gated on the drive being an NBD device: the legacy materialized-file
  path stays `ro`/read-only (fc.go Spawn keys `rootMode`/`is_read_only` off `rootfs.Device`), so
  `--nbd=false` still boots (read-only, no writable rootfs, no layer producer). The whole e2e migrates to
  `--nbd` as its default path.
  > **The fork (resolved).** Two smaller options were considered and rejected: **(A)** keep the guest
  > mounting root `ro` and have only the producer `remount,rw` (writable in `--nbd`, minimal change) — the
  > backward-compatible path; **(C)** a `drop_caches` workaround that keeps the docker+debugfs rootfs and
  > only evicts the stale `/etc` page so the grafted disk becomes visible (simplest, but doesn't run the
  > layer command in-guest — diverges from "the E2B way"). The user chose the fullest-fidelity option.
  > **Honest cost:** this is the largest blast radius of any storage stage — it changes the rootfs semantics
  > of *every* sandbox and requires the base snapshot rebuilt + the full e2e re-verified under a writable
  > rootfs. A mount-state note: build-snapshot.sh now boots the base rootfs `rw`, so its throwaway boot
  > writes ext4 mount state to the seeded file; the bucket base is seeded from that same file, so the
  > restored guest's cached superblock matches (verified by the e2e booting cleanly).
- **D3 — scope the one-run producer to layered builds *with a snapshot*.** A cold-start layered build
  (`with_snapshot=False`) has no snapshot and already boots its child rootfs fresh (no grafting), so its
  docker+debugfs rootfs is already correct (`test_layered_template_via_api` is green). Leave it. Only the
  *with-snapshot* path (the broken one) switches to producing both diffs from the run. The two paths yield
  different but each-self-consistent builds (different buildIDs) — fine.
- **D4 — `sync` in-guest after the command, before the snapshot.** So the overlay diff is durable and the
  RAM/disk pair is consistent at rest (D2). Cheap; a single extra `Run("sync")`.
- **D5 — one child layer = the recipe's `RUN` commands, run in sequence in-guest.** We keep the current
  "child = base + one delta" model (one buildID per template), not E2B's per-step layer chain. The producer
  runs the child recipe's `RUN` commands in order against the base, then snapshots once. `COPY`/`ADD` are
  out of scope for now (the current e2e uses only `RUN`; file injection can reuse envd `Filesystem.Write`
  later). *Simpler than E2B's per-step layers; same correctness for the single-delta case we support.*
- **D6 — the base resume must serve the base rootfs *writable* over NBD.** The producer already resumes the
  base self-consistently (base RAM + base rootfs). Today that rootfs is read-only; the producer needs it
  writable so the in-guest command's disk writes are captured. This is a producer-only binding (`restoreHealthy`
  stays read-only for user restores; the producer takes a writable variant).

## 6. Sub-steps (KVM-free first — the house discipline)

### Stage 22a — Go→envd `Run` client ✅ (committed)
`pkg/envdclient`: a hand-rolled Connect-JSON unary client that POSTs `envd.ProcessService/Run` to
`http://<slot-ip>:49983` (the Go twin of the SDK's `connect.py`, zero new deps). 5 KVM-free unit tests
(request shape, the proto3 `{}` empty-response, a non-zero exit, a Connect error, unreachable).

### Stage 22b-1 — the rootfs COW producer algebra ✅ (KVM-free)
- `header.MappingFromDirty` (a per-block dirty/empty set → the same COW mapping `BuildDiff` produces, via a
  shared `diffAccumulator` refactored out of `BuildDiff`, so the two paths can't drift). Unit-tested for
  run-for-run agreement.
- `storage.PublishRootfsDiffBlocks(DiffBlocks)` (the disk half of the producer): consume the overlay's
  exported dirty blocks directly — needs only the base **header** (never its bytes), uploads only the
  changed blocks. Round-trip unit-tested through `MaterializeLayered`.

### Stage 22b-2a — writable rootfs for every sandbox (D2 foundation, KVM)
`buildRootfsBacking` binds `block.NewOverlay(base, per-VM cache)` (writable) instead of `block.NewReadOnly`;
fc.go Spawn + build-snapshot.sh bake `root rw` + `is_read_only: false` (gated on the drive being an NBD
device); `--nbd` becomes the orchestrator default; the default snapshot is rebuilt writable. **Verified by
the full e2e booting/restoring/isolating on a writable rootfs** (this is the load-bearing verification — it
touches every sandbox's boot path).

### Stage 22b-2b — the one-run producer + build wiring (KVM)
- `Snapshotter.LayeredSnapshot` gains the layer's `RUN` commands; the *with-snapshot layered* `build.go`
  path stops calling `build-rootfs-layered.sh` + `PublishRootfsDiff` and lets the producer publish **both**
  diffs.
- The producer (`server.go` `LayeredSnapshot`): resume the base (now a writable overlay) → for each `RUN`
  `envdclient.Run` (fail on non-zero exit) → `Run("sync")` → `vm.Snapshot` → `PublishMemfileDiff` (unchanged)
  **and** `overlay.ExportToDiff()` → `PublishRootfsDiffBlocks`, owner = childBuildID.

### Stage 22c — the e2e goes correctly green + measured win + honest review
- `test_layered_snapshot_via_api` now **passes its disk-content assertion** (`test_template.py:114`) — the
  marker is visible because the guest wrote it in-guest before the snapshot. The memfile-COW-win assertion
  (`:141`, the original 🔴) is then reached and validated for the first time.
- Correct the committed docs' `dedupCompare` claim (§2 correction) where it appears (STAGE20 §2.1, the
  roadmap, the memory). Honest 🔴/🟡/🟢 review.

## 7. Keeping tests green (honest trade-offs)

- 22a + the 22b glue are **pure Go, KVM-free** — `go test ./services/...` + `go test ./daemon` stay the
  oracle for those.
- The live producer (22b) + the corrected e2e (22c) need real KVM/`--nbd`/s3 — the Python e2e, which already
  gates on kvm/firecracker/network/vendor and now `--nbd`.
- **Backward compatibility:** cold-start layered builds (docker+debugfs, D3) and non-layered templates are
  untouched; restore reads (v1/v2 memfile, layered NBD rootfs) are untouched — Stage 22 only changes how a
  *layered-with-snapshot* build **produces** its two diffs.
- **Same honesty rule as Stages 13–21:** this is a *correctness* fix plus deeper E2B fidelity, not a
  single-box speedup (the producer runs one extra in-guest command + `sync`; restore is unchanged). The
  headline is that the layered snapshot is now *correct* (disk changes survive), then the memfile-COW-win
  number (22c) that Stage 20 never reached.

## 8. New dependencies

**None.** Reuses `connect`-JSON over stdlib HTTP (the Python SDK already does this hand-rolled; the Go client
is the same shape), `pkg/block`, `pkg/nbd`, `fc.Snapshot`, and the existing header/storage algebra.

## 9. What this completes / what it defers

**Completes:** the layered *memfile* + *rootfs* are now produced by **one self-consistent run** (E2B's
model), so a layered snapshot restores to a VM whose disk changes are actually visible — the Stage-20 e2e's
premise finally holds. It also lands the first **orchestrator→guest command execution** capability.

**Defers to Stage 23 (a natural follow-on, same in-guest capability):**
- E2B's streaming `Process.Start` (live build logs) (D1).
- **Runtime start/ready commands** — a template defines a server to launch + a readiness probe, run once at
  finalize and captured in the snapshot (E2B: `finalize/builder.go`, `ready.go`); reuses this stage's
  in-guest command reach.
- A **per-restore writable overlay** (stateful/pausable runtime sandboxes) (D2).
- Multi-step layer chains and `COPY`/`ADD` in-guest (D5).

## 10. Known divergences from E2B (deferred, by decision)

| axis | E2B (real) | Stage 22 | status |
|---|---|---|---|
| command runs in-guest during build | yes (`Process.Start` stream) | yes (envd synchronous `Run`) | 🟢 faithful mechanism; simpler transport (D1) |
| one run → two diffs | yes (`Sandbox.Pause`) | yes (resume-writable → run → sync → snapshot → both diffs) | 🟢 faithful |
| rootfs diff source | writable NBD overlay dirty blocks | same (`block.Overlay.ExportToDiff`) | 🟢 faithful |
| memfile diff source | UFFD dirty-page tracking | `header.BuildDiff` block-compare | 🟡 same result set, simpler mechanism (kept) |
| restore rootfs | writable overlay per sandbox | writable overlay per sandbox (Stage 22b-2a) | 🟢 faithful (D2, full-B) |
| layers | per-step chain | one child delta | 🟡 single-delta scope (D5) |
| build logs | streamed | exit-code only | 🟡 ergonomics deferred (D1) |

## 11. The 22b-2b blocker — the writable-NBD re-snapshot won't restore (candidate directions)

**Symptom.** The producer builds a layered snapshot correctly (resume base → run the `RUN` command in-guest
→ `sync` → Full snapshot → publish both diffs). Restoring that child snapshot panics Firecracker at
`src/vmm/src/devices/virtio/block/virtio/device.rs`: `InvalidAvailIdx { queue_size: 256, reported_len:
12659 }` — the restored virtio-blk avail ring is `reported_len` ahead of the used ring, far beyond the
256-entry queue, i.e. the block device's queue state is inconsistent on restore.

**What the diagnostics rule in/out (this session):**
- The **file-backed base snapshot restores fine**, including 3 concurrent restores (`test_microvm_snapshot`),
  under the writable-overlay boot (22b-2a). So writable NBD *at restore* is not the problem.
- **Stage-20's read-only re-snapshot restored fine** (its e2e failed later, on the marker read, not a block
  panic). So re-snapshotting a UFFD-restored VM is not itself the problem.
- The one new variable is **guest writes to the NBD-backed rootfs before the re-snapshot** (Stage 22's whole
  point). So: *snapshotting a VM that has written to the kernel-NBD block device leaves the virtio-blk queue
  in a state Firecracker rejects on the next restore.*

**Leading hypotheses (for the focused follow-up):**
1. **In-flight NBD I/O not drained at pause.** Firecracker's `PATCH /vm Paused` stops the vcpus, but the
   virtio-blk worker's requests to `/dev/nbdX` (served async by our userspace `pkg/nbd`) may not be fully
   completed when `/snapshot/create` serializes the queue. `sync` flushes the *guest* page cache but may not
   guarantee the *device* queue is idle. Try: an explicit drain/quiesce of the NBD device (or our
   `nbd.Dispatch` loop) before pause; or a `blockdev --flushbufs` + settle in-guest; or verify our NBD server
   advances the completion (used) ring in lockstep.
2. **Guest-RAM vs device-state mismatch across UFFD.** The virtqueue rings live in guest RAM (served over
   UFFD from the COW memfile). If the Full snapshot doesn't capture the *dirtied ring page* into the child
   memfile (so restore serves the base ring page while the vmstate has the child's avail_idx) → mismatch.
   Check whether FC's Full snapshot over a UFFD backend writes *all* guest RAM (faulting untouched pages) —
   the memory's long-standing 🔴 assumption — and specifically whether the block-queue pages are captured.
3. **file→NBD backend switch.** Our base snapshot is created **file-backed** (`build-snapshot.sh`,
   `path_on_host=$ROOTFS`) and resumed **NBD-backed**. E2B builds NBD-backed from the start. The write +
   re-snapshot may expose a state incompatibility this switch introduced. Candidate: rebuild the base
   snapshot over NBD too (a `build-snapshot.sh` rework), removing the switch.

**Bisect (done, this session).** Ran the *old* Stage-20 producer (resume → snapshot, **no in-guest command,
no `sync`**) on the writable-overlay foundation. Result: the **same panic**, `reported_len: 12659`
**identical** to the with-command run. So:
- It is **not** the in-guest command's writes — it is the **writable overlay at re-snapshot itself**. The
  old read-only producer (Stage 20/21) re-snapshotted + restored fine; swapping in the writable overlay
  (22b-2a) breaks it with no code-path change.
- The **identical 12659** across independent runs means it is **deterministic**, not random in-flight I/O —
  which rules out "un-drained pending writes" and points at a **structural** wrong value: most likely the
  guest's virtqueue **avail-ring page**, dirtied by the `rw`-mount's background writes (atime / ext4 journal)
  during resume, **is not captured into the child memfile** by the Full-snapshot-over-UFFD, so at restore it
  is served from the *base* memfile (the base's old avail_idx = 12659) while the vmstate holds the child's
  used_idx → `avail - used` blows past the queue size.

**Experiment C (done, this session): boot the base snapshot `ro` (device still `is_read_only:false`), re-run
the no-command bisect.** Result: the block-device `InvalidAvailIdx { … 12659 }` panic **is gone** — but a
**different** virtqueue panic appears at `src/vmm/src/devices/mod.rs:32`: *"The number of available virtio
descriptors 65533 is greater than queue size: 256!"* (`65533 = -3 mod 65536`, a generic per-device check; the
resume log shows both `[Net:eth0]` and `[Block:rootfs]` being kicked, so it is one of them). So:
- The `rw`-mount's **background writes** were indeed the cause of the *block-queue* corruption (12659) —
  booting `ro` fixes that one. Confirms the write→block-queue link.
- **But a second, deeper inconsistency remains** (65533) even with a quiescent `ro` mount. The only remaining
  difference from Stage 20/21 (which re-snapshotted + restored fine) is the **writable device itself**
  (`block.Overlay` / `is_read_only:false`) vs the read-only device (`block.ReadOnly` / `is_read_only:true`).
  So **the writable block device changes the virtio state such that a re-snapshot of a UFFD-restored VM no
  longer restores** — independent of writes and of mount mode. This is a *systemic* re-snapshot/virtqueue
  consistency problem, not a single-device drain issue.

**Conclusion / where the focused follow-up must start.** This is deeper than a `sync`/drain fix. Two concrete
directions, in order of promise:
1. **Whether FC's Full-over-UFFD snapshot captures a self-consistent (memfile, vmstate) pair for the virtio
   rings when the device is writable.** Instrument or read FC's `create_snapshot` + the UFFD memory dump
   path; the `-3`/`+N` avail-vs-used skews smell like ring pages served from the base memfile that don't
   match the saved device state. If the memfile diff is missing dirtied ring pages, the header/`BuildDiff`
   or the multi-owner read is a suspect too.
2. **E2B's architecture — build the base snapshot NBD-backed (writable overlay) from the start**, removing
   both the file→NBD backend switch *and* the read-only→writable transition our path introduces at re-snapshot
   (a `build-snapshot.sh` rework so the base is snapshotted over the same writable NBD stack it will resume
   on). This is the biggest change but the most likely to be robust, and it is literally how E2B avoids the
   whole class of problem.

**Bisect for next session:** does a **read-only** device (`block.ReadOnly`, `is_read_only:true`) + a
`remount,rw` only for the command restore? That isolates whether the writable *device config* or the
writable *overlay* is the culprit — one KVM run.

None of these change the Stage-17/18/20 *seam* (`StorageProvider` / `PageSource` / `header`) or the restore
path; Stage 22 changes only the **producer**.

## 12. Chosen path — the E2B architecture: create the base snapshot over NBD (blueprint)

**User decision:** pursue §11 direction 2 (E2B's architecture) — make the base snapshot NBD-backed-writable
from creation, so the whole chain (base create → producer resume → re-snapshot → restore) rides one
consistent writable NBD stack, eliminating the file-backed-writable → NBD-writable transition our current
path takes at re-snapshot.

**Root-cause framing (precise).** Stage 21 (read-only device) re-snapshots + restores fine. Our writable
path (`block.Overlay`, `is_read_only:false`) panics — *both* base and Stage-21 are **file-backed at
creation** (`build-snapshot.sh` boots `path_on_host=$ROOTFS`, a regular file) and resumed **NBD-backed**.
So the differentiator is **writable + the file→NBD transition together**. E2B never transitions: it
snapshots the base over the *same* writable NBD block device it later resumes on. So the base snapshot must
be **created over NBD**, and Firecracker must see a **block device at a stable path** at both create and
restore (a device *node* path is not stable across VMs — bind a device over a stable file path, as
`fc.Restore` already does).

**The one thing E1 must confirm before E2/E3 (the risk).** It is not yet proven that an *NBD-writable* base
snapshot re-snapshots + restores cleanly — the bisect only proved the *file-backed-writable* one does not.
E1 de-risks the whole plan with a **plain restore** of an NBD-created snapshot; only if that is panic-free
do E2/E3 follow.

### Sub-steps

**E1 — create the base snapshot over NBD; verify a plain restore (de-risk).**
- **`fc.Spawn` binds the device over a stable path (like `fc.Restore`).** Today cold-start over NBD points
  `path_on_host` straight at `/dev/nbdX` (`fc.go:150`); change it to the Restore-style setup — bind
  `/dev/nbdX` over `tmpl.Rootfs` in the per-VM mount ns and boot `path_on_host=tmpl.Rootfs`. Now a
  cold-started VM (and any snapshot of it) sees a **block device at a stable path**, matching restore.
  Verify cold-start unaffected (`test_microvm` cold-start cases stay green).
- **Orchestrator `--make-snapshot <name>` mode** (in `main.go`, after `newServer`, before serving): resolve
  the template → `spawnHealthy(tmpl)` (now NBD block-device at a stable path) → **warm the kernel** (a
  code-interpreter `Execute` of `pass`, mirroring `build-snapshot.sh`'s warm-up; reuse the envd/CI ports) →
  `fc.Snapshot(vmstate, memfile)` → publish (snapfile + Stage-17 compacted memfile + the baked rootfs path =
  `tmpl.Rootfs`) → exit. Reuses the orchestrator's storage/network/nbdPool init.
- **Verify:** create the default snapshot via `--make-snapshot default`, run `test_microvm_snapshot`
  (plain restore, incl. concurrent). **Panic-free ⇒ the E2B path is confirmed; panic ⇒ stop, the writable
  re-snapshot is broken independent of the transition (fall back to §11 direction 1).**

**E2 — wire it in.** The e2e fixture builds the default snapshot with the orchestrator (`--make-snapshot`)
instead of `build-snapshot.sh` (which can't drive our userspace NBD). Keep `build-snapshot.sh` only for the
non-NBD escape hatch (or retire it). Re-verify the full suite green.

**E3 — re-implement the producer (reverted at the foundation commit).** Restore the 22b-2b code (from
`docs/STAGE22_DESIGN.md` §6 + the memory ledger): `Snapshotter.LayeredSnapshot(…, commands)`,
`restoreHealthyWritable` (returns the `*block.Overlay`), the `server.go` producer (resume base over NBD →
`envdclient.Run` each command → `sync` → `fc.Snapshot` → `PublishMemfileDiff` + `overlay.ExportToDiff()` →
`PublishRootfsDiffBlocks`), the `build.go` producer early-return + `runCommands`, and the `envdclient`
`Proxy:nil` fix (else the 502 returns). Run `test_layered_snapshot_via_api` (now `--nbd` default): the
marker is visible AND the re-snapshot restores. This closes Stage 22.

**Note on D2/full-B:** with the base `rw`-booted, background writes still dirty the block queue — E2B lives
with this because its whole stack is NBD-consistent, so the re-snapshot is self-consistent regardless. E1's
result decides whether `rw` boot stays or the base should boot `ro` with the producer remounting (Option A's
mechanism) as an extra safety.
