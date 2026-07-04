# Stage 20 design — storage depth (4): COW layered **memfile** via live-VM re-snapshot

> Status: **design, pending user go-ahead on 20a.** This closes the one artifact E2B layers that we
> still store per-build in full: the **memfile** (guest RAM snapshot). Stages 17–19 banked everything
> the algebra needs — a per-block `.header` index (17), the COW owner/`MergeMappings` machinery + a
> rootfs diff proof (18), and layout preservation so the diff is the genuine delta (19). Stage 20
> applies that *same* machinery to RAM. Read `docs/STAGE17_DESIGN.md` (the header) and
> `docs/STAGE18_DESIGN.md` (the COW algebra) first.
>
> **Honest headline (to be measured in 20c, not promised now):** the win is that N child templates
> sharing a base no longer each store a full compacted memfile (~228 MiB, Stage 17); each stores only
> the RAM pages that differ from the base. How small that diff is depends entirely on *how much RAM the
> child actually changes* — the empirical question 20c answers with a number, exactly as Stage 18/19
> measured the rootfs diff.

## 1. Where this sits

| artifact | stored today | this stage |
|---|---|---|
| rootfs | COW **diff** over base (Stage 18/19), assembled whole at boot | unchanged |
| snapfile (VM/device state) | whole per build (small) | unchanged |
| **memfile** (guest RAM) | **whole, compacted** per build (Stage 17: present blocks + `.header`) | **COW diff** over base, served lazily over UFFD |

The memfile was left single-build in Stage 17/18 on purpose (`pkg/build/build.go:168`: *"memfile COW is
Stage 20"*): a **build-time** memfile diff is meaningless — two independently-booted RAM snapshots differ
everywhere. A meaningful memfile diff needs the **same VM continuing**: restore the base's RAM, let the
guest run, then re-snapshot. That live-VM re-snapshot is the one genuinely new capability here.

## 2. What E2B actually does (verified against `e2b-dev/infra` @ main)

Researched this stage (a subagent read the real source). Two prior assumptions were **corrected**:

1. **E2B does NOT use Firecracker "Diff" snapshots.** `orchestrator/pkg/sandbox/fc/client.go`
   `createSnapshot` passes `SnapshotType: ...Full`, and `setMachineConfig` sets
   `TrackDirtyPages = false`. There is **no** Firecracker dirty bitmap. E2B takes a **Full** snapshot and
   computes the memory diff **itself, in userspace, by content comparison** against the base's assembled
   memfile (`orchestrator/pkg/sandbox/block/dedup.go` `dedupCompare`, per 4 KiB page). *So the faithful
   choice is block-compare — which happens to be the simpler one too.*
2. **The dirty-vs-empty crux (two classes, not one).** Per page, `dedupCompare` classifies **three** ways:
   - `IsZero(page)` → **empty**: recorded in the *empty* set, **no bytes stored**, mapped to `uuid.Nil`
     (`metadata.go` `toDiffMapping`: `CreateMapping(&ignoreBuildID=uuid.Nil, d.Empty, …)`). This
     **overrides** the base with zeros at read time.
   - `!bytes.Equal(page, base)` → **dirty**: stored in the diff, owned by *this* build.
   - equal to base → **unchanged**: no diff entry, falls through to the base via `MergeMappings`.

   The crux: a page the layer **zeroed** must become an explicit zero-owner run that *overrides* the base;
   without the explicit empty set it would fall through to the base's stale non-zero bytes. **Our
   `header.BuildDiff` (`header.go:386`) already does exactly this** — a changed all-zero block becomes a
   `Owner:""` run (no bytes written), a changed non-zero block is child-owned, an unchanged block is
   omitted. Stage 18 got the crux right for the rootfs; the memfile reuses it verbatim.
3. **Flatten at build time.** `metadata.go` `ToDiffHeader`: `MergeMappings(base, diff)` +
   `NormalizeMappings`, store the full `[0,Size)` mapping in the layer's header with `Generation+1` and
   `BaseBuildId` preserved. Read = one binary search, no chain walk. **We have all of this** (`MergeMappings`,
   `NormalizeMappings`, the v2 header with `Generation`/`BaseBuildId`, Stage 18).
4. **Serve at restore (UFFD multi-build read).** `orchestrator/pkg/sandbox/build/build.go` `File.readAt`:
   resolve the faulting offset → owning `BuildId` via the flattened header → range-read **that build's**
   object (each build cached separately, `DiffStore` keyed by `(buildID, fileType)`); `uuid.Nil` →
   zero-fill in place, no fetch → `UFFDIO_ZEROPAGE`. This is our **20a** (the multi-owner page source).
5. **Same machinery for rootfs and memfile.** `build/diff.go`: `DiffType` in `{Memfile, Rootfs}`; both
   flow through the *identical* `header` package and `build.File`. **Only the transport differs** — memfile
   over **UFFD**, rootfs over **NBD** (we materialize the rootfs whole instead — NBD is a separate deferred
   stage). Our `pkg/storage/header` is already shared; keep it symmetric.

**Compression — a correction to a repeated project claim.** Earlier audits (Stage 18) concluded "E2B stores
raw blocks, compression is not an E2B mechanism." That is **outdated**: current E2B has V4/V5 header formats
with **optional** zstd/lz4 compression in 2 MiB frames (`shared/pkg/storage/compress_encode.go`, `FrameTable`),
flag-gated, with raw **V3 still supported**. Compression is **orthogonal to COW** and off the COW critical
path. **Stage 20 stores raw (E2B's V3 equivalent, our v1/v2 headers);** framed compression is re-filed as a
genuine (optional) E2B mechanism for a later stage, not "not E2B." CLAUDE.md / the roadmap / STAGE15–18 carry
the stale wording — correcting those committed docs is a separate, user-approved cleanup.

## 3. What we already have that this reuses **verbatim**

- `header.BuildDiff` (`header.go:386`) — the 3-way page classifier incl. the zero-owner override. **The crux
  is done.**
- `header.MergeMappings` / `NormalizeMappings` / the **v2** header (`Generation`, `BaseBuildId`, per-entry
  owner index) — Stage 18. Artifact-agnostic; RAM is just another byte stream.
- `storage.PublishRootfsDiff` (`cow.go:32`) — the exact 3-step producer shape to mirror.
- `uffd.PageSource` (`uffd.go:214`) + `chunkedSource` (`source_bucket.go`) — the per-owner reader the
  multi-owner source composes (like `cow.go` `assembleRuns`' per-owner cache, but **lazy** and per-fault).
- `fc.firecrackerAPI` + `MicroVM.workdir` — the producer's snapshot-create reuses the existing FC API helper.

## 4. The four gaps (mapped to code)

1. **Producer — a Go live-VM re-snapshot.** Today snapshot *creation* lives only in `scripts/build-snapshot.sh`
   (fresh boot → `PATCH /vm Paused` + `PUT /snapshot/create Full`); Go's `fc.Restore` only *loads*. Add
   `fc.MicroVM.Snapshot(vmstate, memfile)` (the two API calls, via `firecrackerAPI`) and a build path that
   **restores the base**, warms/primes the guest, pauses, and snapshots.
2. **`storage.PublishMemfileDiff`** — the memfile mirror of `PublishRootfsDiff`: materialize the base's full
   memfile, `BuildDiff` the child's full memfile against it, `MergeMappings` → v2 header, upload
   `{childID}/memfile` (dirty blocks only) + `{childID}/memfile.header`. Plus `MaterializeMemfileFull` (expand a
   compacted/layered memfile to a full temp file to diff against — the memfile analogue of `MaterializeLayered`,
   but the memfile is never *booted* from a whole file, only diffed from one at build time).
3. **Page source → multi-owner** (`source_mapped.go`). Today `mappedSource` holds **one** `phys` object;
   `Extent` has no owner (`server.go:280` even *drops* the owner the header carries). Add `Owner` to `Extent`,
   make the source resolve each fault to the owning build's lazily-opened `chunkedSource` (zero-owner → zeros).
4. **Wiring** (`server.go:238` `prepareRestore`). When the memfile header is layered (multiple owners /
   `Generation>0`), build the multi-owner source, opening a reader per distinct owner. Stop dropping the owner.

## 5. Decisions

- **D1 — block-compare, not FC Diff snapshot (E2B-faithful).** Take a **Full** snapshot and diff in userspace
  (`BuildDiff`) against the base's assembled memfile. This is what E2B does (§2.1), reuses all our algebra, and
  keeps rootfs+memfile symmetric. `track_dirty_pages`/Diff snapshots are explicitly *not* E2B's path here.
- **D2 — keep the header-local build index** (uint32), not E2B's per-entry `uuid.UUID`. The research confirms
  the owner is just an identity for `MergeMappings`/resolution — our index is a legitimate space optimization,
  algorithm-unchanged.
- **D3 — store raw blocks** (our v2 header, E2B's V3 equivalent). Framed zstd/lz4 compression is a real but
  **optional, orthogonal** E2B mechanism (§2 correction) — deferred, not adopted here.
- **D4 — unify the page source** (recommended): generalize `mappedSource` so a single-build memfile is the
  degenerate one-owner case, matching E2B's single `build.File` path for both. Costs a small touch to the
  Stage-17 wiring (`prepareRestore` stops dropping the owner even in the single-build case). *Alternative:* add
  a separate `layeredSource` and branch by header version — smaller blast radius, but two code paths where E2B
  has one. Per "choose closer to E2B," unify.
- **D5 — how the child's RAM gets a (meaningful) diff.** E2B runs the *layer's build commands in the guest*,
  which dirties RAM. Our layers change the **rootfs** via docker (Stage 19), not via in-guest commands — so the
  producer **restores the base, re-runs the warm-up (health + a prime cell), pauses, and re-snapshots**; the
  diff captures whatever the warm-up changed. For 20c's e2e to actually exercise cross-owner reads, the child
  primes something the base did not (e.g. importing a module the child `RUN pip install`'d), so some faults
  resolve to the child and most to the base. **Honest divergence:** running the layer's *actual* build commands
  in-guest (E2B's model) is a deeper convergence tied to a start/ready-command subsystem — deferred (§10).
- **D6 — skip parent-dedup / promotion.** E2B additionally dedups a page against *any* ancestor (not just the
  immediate base) with a budget/promotion pass (`parentByKey`). We classify only three ways vs the immediate
  base — correct layered reads, just less storage-optimal. A pure storage optimization, not a correctness
  requirement (confirmed by the research).

## 6. Sub-steps (KVM-free first, the house discipline)

### Stage 20a — the multi-owner memfile page source (no build/VM wiring)
`Extent` gains `Owner`; generalize `mappedSource` to hold a lazily-populated `map[owner]PageSource` fed by an
injected `open(owner) (io.ReaderAt, func() error, error)` (so `pkg/uffd` stays storage-free — the orchestrator
supplies the opener in 20c). `ReadAt` locates the run, reads from its owner's chunked source at the physical
offset, zero-owner/gap → zeros. **KVM-free unit test:** two in-memory owner objects + extents crossing them,
assert `ReadAt` stitches the right bytes and zero-fills gaps. `go test ./services/...` green.

### Stage 20b — the diff producer: `PublishMemfileDiff` + `MaterializeMemfileFull` + `fc.MicroVM.Snapshot` + build wiring
- **Algebra (KVM-free):** `MaterializeMemfileFull` (expand compacted/layered memfile → full temp file) and
  `PublishMemfileDiff` (mirror `PublishRootfsDiff`, reusing `BuildDiff`/`MergeMappings`). Unit-tested with byte
  slices — no VM.
- **Live-VM capability (needs KVM):** `fc.MicroVM.Snapshot(vmstate, memfile)` (PATCH `/vm` Paused + PUT
  `/snapshot/create` Full via `firecrackerAPI`); a layered snapshot path in `pkg/build.Build` that, when `base`
  is set and `withSnapshot`, does `fc.Restore(base)` → warm/prime → `Snapshot` → `PublishMemfileDiff` instead of
  `build-snapshot.sh` + `PublishMemfile`. Exercised for real in 20c.

### Stage 20c — wire `prepareRestore` + real-VM e2e + measured win + honest review
`prepareRestore` builds the multi-owner source (opening a reader per owner) when the memfile header is layered,
else the Stage-17 single-owner path (unified under D4). Real-VM e2e: build a layered template **with a
snapshot**, restore it (warm-pool + direct), assert it boots, its RAM carries the child's prime, and code runs;
a Go probe (extend `cmd/msb-rootfs-stat` or a sibling) reports the stored memfile-diff bytes; assert
`stored_memfile_diff < full_compacted_memfile / K` for a K set from the measurement. Honest 🔴/🟡/🟢 review.

## 7. Keeping tests green (honest trade-offs)

- 20a + the 20b algebra are **pure Go, KVM-free** — the parity oracle stays `go test ./services/...`.
- The live-VM re-snapshot (20b) + the layered-memfile boot (20c) need real KVM — covered by the Python e2e,
  which already gates on go/firecracker/kvm/network/vendor (auto-skip otherwise).
- **Backward compatibility:** an old bucket (single-build compacted memfile, Stage 17) has a v1 header / one
  owner → the unified source serves it exactly as before. A pre-Stage-17 raw memfile (no header) still hits the
  `chunkedSource` fallback. Both must stay green.
- Same **honesty rule** as Stages 13–19: this is fidelity + a real mechanism, and the single-box latency story is
  unchanged (net setup dominates restore). The claim is the *stored-bytes* win (measured in 20c), not speed.

## 8. New dependencies

**None.** No `roaring` bitmap dep (E2B uses it; our single-pass `BuildDiff` emitting coalesced runs replaces the
two bitmaps — the same three classes, expressed as zero-owner vs child-owner vs omitted runs). No compression
libs (D3). Consistent with the "hand-rolled, zero-new-dep" discipline of Stages 17–19.

## 9. What this completes

The **last artifact E2B layers that we stored whole**. After Stage 20, rootfs *and* memfile are both COW diffs
over a base, produced by the same `header` algebra, differing only in transport (rootfs materialized whole /
memfile served lazily over UFFD) and in production (rootfs = docker+debugfs delta / memfile = live-VM
re-snapshot). It also lands the **live-VM snapshot-create** capability in Go for the first time (previously
shell-only), a precondition for pause/resume-style features.

## 10. Known divergences from E2B (deferred)

| axis | E2B (real) | this stage | status |
|---|---|---|---|
| diff source | Full snapshot + userspace block-compare | same | 🟢 faithful (§2.1) |
| dirty-vs-empty crux | two bitmaps → dirty/`uuid.Nil` | `BuildDiff` zero-owner vs child-owner runs | 🟢 faithful |
| flatten | `MergeMappings`+`NormalizeMappings` at build | same | 🟢 faithful |
| serve | fault → owner → that build's object; `uuid.Nil` → zeropage | multi-owner `mappedSource` | 🟢 faithful |
| owner id | per-entry `uuid.UUID` | header-local build index | 🟢 improved (zero-dep, smaller) |
| what dirties RAM | the layer's **build commands run in-guest** | restore base → **warm-up/prime** → re-snapshot | 🟡 analogue (D5); in-guest build commands deferred (start/ready-command subsystem) |
| parent-dedup | dedup vs any ancestor + promotion budget | dedup vs immediate base only | 🟡 storage-only optimization deferred (D6) |
| compression | optional zstd/lz4, 2 MiB frames, V4/V5 (raw V3 too) | raw (v2 header) | 🟡 orthogonal, optional — deferred (D3), **not** "not-E2B" |
| memfile transport | UFFD | UFFD | 🟢 faithful |
| cross-node cache | NFS-wrapped shared chunks | per-VM local chunk cache | 🟡 multi-host — deferred |

None of these change the Stage-17/18 *seam* (`StorageProvider`/`PageSource`/`header`); they deepen the
*mechanism* behind it — which is why the algebra (18) and the header (17) landed first.
