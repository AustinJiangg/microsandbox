# Stage 22 design — the E2B layer producer: run the layer's command **in-guest**, one snapshot → two consistent diffs

> Status: **design, for review.** This stage exists because **Stage 20's e2e now fails on real KVM**
> (its first run, this session). `tests/test_template.py::test_layered_snapshot_via_api` builds a
> COW-layered template *with a snapshot*, restores it, and reads the file the layer's `RUN` wrote —
> and the read returns **404 `no such file or directory`**. The failure is not flaky infra: it is the
> direct symptom of the divergence Stage 20's own D5 (revised) deferred (`server.go:262`, *"we do NOT
> run the layer's build command in-guest"*). Stage 22 closes it the way E2B does — and that is exactly
> the "possible next" the roadmap and `CLAUDE.md` already name.

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
- **D2 — the writable overlay is used only DURING the producer build; restore stays read-only (Stage 21c).**
  The produced snapshot restores with a read-only NBD rootfs, as today. Correctness holds because the
  producer `sync`s (D4): the file is on the child rootfs *and* in the restored RAM's page cache. A
  per-restore writable overlay (E2B's runtime model, needed for stateful/pausable runtime sandboxes) is a
  separate, deeper feature — deferred with Stage 23.
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

### Stage 22a — envd in-guest command reach (Go client) + the writable producer binding
- **Go→envd `Run` client** (`pkg/proxy` or a new `pkg/envdclient`): POST `envd.ProcessService/Run` to
  `slot:49983` (Connect-JSON, like the Python SDK's `connect.py`), return stdout/stderr/exit. **KVM-free
  unit test:** a stub HTTP server asserting the request shape + decoding a canned response.
- **`buildRootfsBacking` writable variant** (`nbd.go`): a `buildWritableRootfsBacking` that binds
  `block.NewOverlay(base, cache)` and exposes the `*Overlay` (for `ExportToDiff`). **KVM-free unit test:**
  Overlay round-trip already covered by `overlay_test.go`; add a test that the orchestrator wires a cache
  sized to the base. `go test ./services/...` green.

### Stage 22b — the one-run producer + build wiring (algebra KVM-free; live path KVM)
- **`Snapshotter.LayeredSnapshot` gains the layer commands** (`pkg/build`): the interface + the call site
  pass the recipe's `RUN` commands. The build's *with-snapshot layered* path (`build.go`) stops calling
  `build-rootfs-layered.sh` + `PublishRootfsDiff`, and lets the producer publish **both** diffs.
- **The producer** (`server.go` `LayeredSnapshot`): resume the base with the **writable** backing → for each
  `RUN` command `envdRun(vm, cmd)` (fail on non-zero exit) → `envdRun(vm, "sync")` → `vm.Snapshot` →
  `PublishMemfileDiff` (unchanged) **and** `overlay.ExportToDiff()` → `PublishRootfsDiff`-shaped upload of the
  captured blocks + header, owner = childBuildID. **KVM-free:** the export→publish glue is unit-testable with
  a synthetic overlay diff; the live resume+run+snapshot is exercised by the e2e.

### Stage 22c — the e2e goes correctly green + measured win + honest review
- `test_layered_snapshot_via_api` should now **pass its disk-content assertion** (`test_template.py:114`) —
  the marker is visible because the guest wrote it in-guest before the snapshot. The memfile-COW-win
  assertion (`:141`, the original 🔴) is then reached and validated for the first time.
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
| restore rootfs | writable overlay per sandbox | read-only + producer `sync` | 🟡 correct for our snapshot model; writable-at-restore deferred (D2) |
| layers | per-step chain | one child delta | 🟡 single-delta scope (D5) |
| build logs | streamed | exit-code only | 🟡 ergonomics deferred (D1) |

None of these change the Stage-17/18/20 *seam* (`StorageProvider` / `PageSource` / `header`) or the restore
path; Stage 22 changes only the **producer**.
