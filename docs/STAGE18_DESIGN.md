# Stage 18 design: storage-mechanism depth (2) — COW layered builds (the rootfs diff)

> Status: **done (18a–18d shipped).** The full COW mechanism is live and the real-VM e2e is green: a
> layered template (`derived`, base `default`) builds through SDK(`base=`) → api(`from`) → orchestrator →
> `pkg/build`, is stored as a rootfs **diff** with a flattened v2 header, then a sandbox cold-starts from it,
> carries the child's added content, and runs code (`tests/test_template.py::test_layered_template_via_api`).
>
> **Honest measured outcome (the headline finding of this stage).** The mechanism is faithful, but the
> *size win on our pipeline is modest* — and understanding **why** is the real lesson:
> - The real `derived` build (default image + one `RUN` layer, pinned to the base's 576 MiB) stored a
>   **278.8 MiB** diff over the 576 MiB base — only **2.07×**, with 178 present runs. Not the ~2.9% Decision 8's
>   lab table predicted.
> - **Root cause (measured, not guessed):** `mkfs.ext4 -d` *is* deterministic at a fixed size — two fresh
>   builds of the **same image** at 576 MiB differ by only **~4,375 / 147,456 blocks (~3%)**, matching
>   Decision 8. But running `docker build FROM agent + RUN …` and then `docker export | mkfs.ext4 -d` reshuffles
>   the ext4 block layout: the same content+marker vs the base differs by **71,364 blocks (~278 MiB)**. Adding
>   even a trivial layer changes `docker export`'s file ordering, so mkfs lays ~half the content at new offsets —
>   spurious relocations, not real delta.
> - **So the size-pin (Decision 8) is necessary but *not sufficient*.** The deeper precondition is that base and
>   child share **block layout**, which E2B gets for free by mutating a **persisted block device in place**; our
>   re-create-the-filesystem-each-build pipeline cannot preserve layout across a content change. Closing this gap
>   (an in-place / overlay block device, or NBD over the layered header) is itemized as a known divergence (§10)
>   and a follow-on — it is *not* a defect in the COW algebra this stage banks, which is correct for any layout
>   (it just stores a bigger diff when the layout diverges). The "store less" win is real but bounded here
>   (576 → 278.8 MiB, ~2×) until layout is preserved.
>
> **Update — closed by Stage 19.** The "layout preservation" gap (§10) was exactly what Stage 19 fixed: a layered
> child is now produced by mutating a **copy of the base's rootfs in place** (`debugfs`) rather than re-mkfs, so
> the same `derived` build stores a **28,672-byte (28 KiB)** diff — **0.0047%**, down from the 278.8 MiB / 48%
> measured here (~10,000× smaller, ~the genuine delta). The COW algebra this stage banked was correct all along;
> only the child's *production* changed. See `docs/STAGE19_DESIGN.md`.

> (Original) Status: **design (proposed).** Second of the deferred "storage-mechanism depth" items the roadmap
> parked behind the Stage-15 `StorageProvider` / `PageSource` seams and the Stage-17 `pkg/storage/header`
> index (`docs/E2B_ALIGNMENT_ROADMAP.md` §5 "Still deferred"; `docs/STAGE17_DESIGN.md` §10 item 3). Stage 17
> compacted a **single flat** build's memfile behind a per-block mapping with **no build owner** — every
> present block belongs to that one build. This stage adds E2B's real **copy-on-write layering**: a build
> can be a **diff over a base build**, the header's mapping carries a **per-entry build owner**, and the
> boot path reads each range **from the build that owns it**. Read `docs/STAGE17_DESIGN.md` (the header this
> extends) and `docs/STAGE15_DESIGN.md` (the object-storage seam + materialize/stream split) first.
>
> **This design is verified against the real `e2b-dev/infra` source** at
> `/home/austin/projects/goproject/infra` (`packages/shared/pkg/storage/header/{mapping,header,metadata,serialization,diff}.go`
> and the orchestrator's `internal/sandbox/{tracker,diffcreator,sandbox,snapshot}.go` +
> `internal/sandbox/build/{build,storage_diff,cache}.go`), not from memory. The faithful core: a build's
> object holds **only its diff blocks**; the header's `BuildMap` carries which **build owns** each logical
> run; `MergeMappings(parent, diff)` flattens a layer chain into one mapping; at boot each faulting offset
> resolves to a `(buildOwner, storageOffset)` and is read from **that** build's object.
>
> ### Correction banked by this stage's source audit (important)
> While verifying E2B for this stage I checked the long-standing claim — repeated in `docs/STAGE15_DESIGN.md`
> §11, `docs/STAGE17_DESIGN.md` §10, and `docs/E2B_ALIGNMENT_ROADMAP.md` §5 — that **E2B stores the
> memfile/rootfs "chunked + compressed" (LZ4/zstd frames)**. That is **false**: E2B does **not** compress its
> memfile/rootfs storage objects. Evidence in the real tree: no non-generated, non-test Go file imports any
> compression package (`klauspost/compress`, `pierrec/lz4`, `compress/*` are absent from the storage/build/
> orchestrator paths; the two compression libs are `// indirect` in `go.mod`, pulled transitively by
> minio/grpc/otel), the diff writer writes **raw** blocks (`header/diff.go` `writeDiff` → `diff.Write(b)`),
> and `BuildStorageOffset += m.Length` advances by the **uncompressed** length (`mapping.go` `CreateMapping`).
> So **compression was never an E2B-fidelity item** — it would be our own extension. This corrected the user's
> first pick (compression) to COW, the genuinely E2B-faithful depth. **17c follow-up:** the three docs above
> are edited in 18c to drop the "compressed" wording (compression re-listed as an *optional non-E2B extension*,
> not a fidelity gap).

## 1. Goal & non-goals

**Goal.** Give the artifact store E2B's copy-on-write layering mechanism, end to end, demonstrated on the
**rootfs** (where a build-time block diff is meaningful — see Decision 1):

- **`pkg/storage/header` grows a build owner + the merge algebra** (mirroring E2B): each `BuildMap` gains
  the **owning build**; `Metadata` gains `BuildId`/`BaseBuildId`/`Generation`; and the package gains
  `CreateMapping` (bitset → runs), `MergeMappings(parent, diff)` (flatten a layer onto its base),
  `NormalizeMappings` (coalesce same-owner runs), and a `Resolve(offset) → (owner, storageOffset, length)`
  lookup. This is the heart of the stage and is pure / KVM-free.
- **A diff producer.** `BuildDiff(base, child, blockSize)` block-compares the child image against a base
  image, writing **only the changed (and non-empty) blocks** to the diff object and emitting a diff mapping
  (changed runs owned by the child; unchanged → resolved to the base's owner via `MergeMappings`). This is
  the single-machine analogue of E2B's dirty-bitset diff (Decision 2).
- **Layered template builds.** `build_template(name=B, base=A, …)` (SDK + API `from` field) stores **B's
  rootfs as a diff over A's**: `{B}/rootfs.ext4` holds only B's changed blocks, `{B}/rootfs.ext4.header`
  carries the flattened mapping that points unchanged runs at `{A}/rootfs.ext4`.
- **A layer-aware boot/materialize path.** Materializing a layered rootfs reads its header and **assembles**
  the full local file by reading each run **from its owning build's object** (a small per-build object cache
  — E2B's `DiffStore` analogue), then writes the baked path the snapshot expects. A rootfs with **no**
  `.header` falls back to today's whole-object download (backward compatible).

**Non-goals** (bounded out / deferred — see §10, §11):

- **The memfile stays single-build (Stage 17), not layered.** A meaningful memfile diff requires the **same
  VM continuing** (restore base → run → re-snapshot with dirty tracking); two *independent* memfile snapshots
  differ everywhere (ASLR, timers) so a build-time byte-compare is meaningless. Memfile COW is **Stage 19**
  (§11) — it needs live-VM snapshot creation + the UFFD multi-build read; it *consumes* this stage's header.
- **No live-VM snapshotting, no Firecracker diff-snapshot/dirty-bitmap.** Our "dirty" is a build-time block
  compare, not Firecracker's runtime dirty tracking (Decision 2). The live flow is Stage 19.
- **The rootfs is still materialized whole (assembled), not NBD-streamed.** Lazy rootfs streaming over the
  same header is the **NBD stage** (`docs/STAGE17_DESIGN.md` §10 item 1). This stage exercises the multi-build
  read at *materialize* time; NBD would later serve the same layered header *lazily*.
- **No compression** (it is not an E2B mechanism — see the correction above), **no cross-node cache**, **no
  auth/TLS/perf claim.** Same standing caveats as the whole repo; still a learning implementation, **not
  security-audited**, never safe for untrusted input.

## 2. Target architecture (what moves)

The data path, the wire, the SDK's run path, the memfile/UFFD path, and the File / local-fs escape hatches
are **unchanged**. Only *how a derived rootfs is stored* and *how a layered rootfs is materialized* move.

```
  BEFORE (Stage 17)                                AFTER (Stage 18)

  build B (no base):                               build B with base A (build-time COW):
    rootfs = full B image                            buildBase = materialize A's full rootfs (assemble if A is layered)
    Upload {B}/rootfs.ext4 = full                     diff,hdr = header.BuildDiff(buildBase, B's rootfs, 4KiB)
    (no rootfs header)                                  • diff = concat(blocks where B != A, and non-empty)
                                                        • childMap = changed runs owned by B
                                                      hdr = MergeMappings(A's resolved rootfs header, childMap)
                                                      Upload {B}/rootfs.ext4        = diff (changed blocks only)
                                                      Upload {B}/rootfs.ext4.header = Metadata{owner=B,base=A} + flattened Mapping
                                                    (build B with NO base -> exactly the Stage-17 whole-object upload)

  prepareRestore/prepareSpawn (s3):                prepareRestore/prepareSpawn (s3):
    Materialize {B}/rootfs.ext4 -> baked path        if Exists {B}/rootfs.ext4.header:
      (one whole-object download)                       hdr = Deserialize(Open {B}/rootfs.ext4.header)
                                                        assemble baked rootfs: for each run in hdr.Mapping:
                                                          read run from {run.owner}/rootfs.ext4 @ run.storageOffset
                                                          (per-build object cache; gap -> zeros)
                                                      else: Materialize whole (Stage-17 fallback)

  memfile path, UFFD serve loop, snapshot restore  ── UNCHANGED (memfile stays Stage-17 single-build)
```

Component → what changes:

| Component | Change |
|---|---|
| `pkg/storage/header` | `BuildMap` gains an **owner**; `Metadata` gains `BuildId`/`BaseBuildId`/`Generation`; add `CreateMapping`, `MergeMappings`, `NormalizeMappings`, `BuildDiff`, `Resolve`. New **format v2** (owner-bearing); v1 (Stage-17 memfile) still read. KVM-free. |
| `pkg/storage` | rootfs gains a header (`RootfsHeaderName = "rootfs.ext4.header"`); `PublishRootfsDiff` (upload diff + header); a layer-aware `MaterializeLayered` (assemble from owning builds) + a small multi-build reader/cache (E2B's `DiffStore` analogue). |
| `pkg/build` | when `base` is set: resolve+assemble the base rootfs, `BuildDiff` against it, `MergeMappings` onto the base header, upload diff+header. No base → today's whole upload. |
| `cmd/orchestrator` (`prepareSpawn`/`prepareRestore`, `TemplateService`) | probe `rootfs.ext4.header` → assemble; else materialize whole. `TemplateCreate` carries an optional `base`. |
| api + SDK | `POST /templates` gains optional `from`; `build_template(base=…)`. |
| deps | **none** — the owner is a header-local **build-table index** (uint32), not a uuid, keeping string build IDs + fixed-width serialization with zero new deps (Decision 5). |

## 3. The COW format + merge — the heart of the stage

E2B's layering (verified): a build's `{buildID}/memfile`|`rootfs.ext4` object holds **only that build's diff
blocks**; the header's mapping says, per run, **which build owns it** and **where in that build's object** it
sits; unmapped/empty runs are zeros. A new build's header is the **flatten** of its diff over its parent's
(already-flattened) header, so boot needs only the one build's header.

**E2B's `BuildMap`** (`mapping.go`):
```go
type BuildMap struct {
    Offset             uint64    // logical byte offset of the run (block-aligned)
    Length             uint64    // run length (block-multiple)
    BuildId            uuid.UUID // which build owns this run  <-- the COW owner Stage 17 dropped
    BuildStorageOffset uint64    // byte offset of the run inside THAT build's object
}
```
**E2B's `Metadata`** (`serialization.go`, `metadataVersion = 3`): `Version/BlockSize/Size/Generation/BuildId/
BaseBuildId`. `NextGeneration(childID)` bumps `Generation`, keeps `BaseBuildId`, sets `BuildId = child`.

**E2B's diff producer** (`tracker.go`→`diffcreator.go`→`diff.go`→`metadata.go`): a runtime **dirty bitset**
(pages the guest changed since restore) drives `writeDiff`, which reads each dirty block; an **all-zero** block
goes to an `empty` bitset (owner `uuid.Nil`, served as zeros), a non-empty one is written to the diff and goes
to `dirty`. `DiffMetadata.CreateMapping` turns the `dirty` bitset into runs owned by the new build and the
`empty` bitset into `Nil`-owned runs, then `MergeMappings(dirty, empty)` combines them.

**E2B's merge** (`mapping.go` `MergeMappings(base, diff)`): a two-pointer overlap walk — where a `diff` run
overlaps a `base` run, the **diff wins** (its owner/offset) and the base run is split into the non-overlapped
left/right remainders (which keep the base owner, with `BuildStorageOffset` shifted by the trimmed prefix).
The result covers the whole size; `NormalizeMappings` then coalesces adjacent same-owner runs. This flatten is
done **at build time**, so a chain `C→B→A` collapses into one mapping whose runs point at whichever of A/B/C
last wrote them — **no recursion at boot**.

**E2B's boot resolution** (`header.go` `GetShiftedMapping(offset)` → `build/build.go` `File.ReadAt`):
`GetShiftedMapping` returns `(BuildStorageOffset+shift, length, buildId)`; `File.getBuild(buildId)` opens
`{buildId}/<file>` (cached in a TTL `DiffStore`, `cache.go`) and range-reads it. So one boot reads from
**several** build objects, one per run's owner.

**What this stage implements** (rootfs, build-time diff; faithful in shape):

- **`BuildMap{Offset, Length, Owner, BuildStorageOffset}`** where `Owner` is a **uint32 index into a header-
  local build table** (the list of build IDs this header references) — see Decision 5; index `0` is reserved
  for the **zero/gap owner** (E2B's `uuid.Nil`).
- **`Metadata{Version=2, BlockSize, Size, BuildId, BaseBuildId, Generation}`** + the build table.
- **`BuildDiff(baseRootfsPath, childRootfsPath, blockSize) → (diffBytes, childMapping)`**: walk both images
  in `blockSize` blocks; a child block that **differs from the base** (and is non-empty) is appended to the
  diff and added to the child's run; identical (or empty) blocks are skipped (they resolve to the base/zero).
  This `child != base` test is our build-time analogue of E2B's runtime dirty bit (Decision 2).
- **`MergeMappings` / `NormalizeMappings` / `Resolve`**: ported from E2B's `mapping.go` (with the owner as the
  table index), unit-tested against the same overlap cases E2B's `mapping_test.go` covers.
- **On-disk** `{buildID}/rootfs.ext4.header` = `Metadata` ‖ build-table ‖ `count` ‖ `mapping[]`, all fixed-
  width little-endian (the Stage-17 discipline, plus a length-prefixed table of build-ID strings).

## 4. Key design decisions

### Decision 1 — COW the **rootfs** (build-time block diff), defer the memfile to Stage 19 (with the user)
A build-time byte/block compare yields a **meaningful** diff only when unchanged blocks are bit-identical
between base and child. That holds for the **rootfs** (a filesystem image: B = A + installed files shares
almost all disk blocks) but **not** for the **memfile**: two independent boot snapshots differ across nearly
every page (ASLR, timers, allocation order), so diffing them would store almost the whole image — defeating
COW. A meaningful memfile diff needs the **same VM continuing** (restore base → run → re-snapshot), i.e. live-
VM snapshot creation — a whole lifecycle subsystem. So this stage banks the **full COW header/merge/multi-
build-read mechanism** on the rootfs (where it pays off and stays KVM-free), and **Stage 19** consumes that
mechanism for the memfile once live snapshotting exists. This mirrors Stage 17→18 (bank the index, then the
owner). The "store less" win is real here: a derived template's rootfs object holds only its changed blocks.

### Decision 2 — build-time block compare stands in for E2B's runtime dirty bitset (faithful analogue)
E2B's `dirty` bitset is "which pages the guest wrote at runtime" (`tracker.go`); ours is "which rootfs blocks
differ from the base image" computed at build time (`BuildDiff`). Same **mapping shape and merge algebra**,
simpler source — exactly the analogue Stage 17 used for zero-detection ("our 'is this block non-zero' stands
in for E2B's 'did this block change vs the parent'"). The empty-block → zero-owner rule is kept verbatim.

### Decision 3 — flatten at build time (E2B's model), read one object per run at boot
`MergeMappings` collapses the layer chain into a single mapping when the child is **built**, so a layered
rootfs's header is self-contained and boot does **no chain walking** — it just reads each run from its owner's
object. This is faithful to E2B (a build's header carries the flattened mapping) and keeps the boot path a
simple per-run lookup. The cost is that the child's header references its ancestors' objects, so those objects
must remain present (E2B's immutable `{buildID}/` artifacts — we keep that).

### Decision 4 — the rootfs header is optional; absence = the Stage-17 whole-object path
Like Stage 17's memfile header, a rootfs with **no** `rootfs.ext4.header` is a non-layered build →
materialized by today's whole-object download. Present → assemble from layers. So a non-`base` build is
byte-for-byte today's behavior, the `default` template is unaffected, and old buckets boot unchanged. The
fallback gets a unit test, not a hope.

### Decision 5 — the owner is a header-local **build-table index**, not a `uuid` (zero new deps; an improvement)
E2B stores a raw `uuid.UUID` (16 bytes) per `BuildMap` and needs `github.com/google/uuid`. Our build IDs are
arbitrary strings, and Stage 17 banked a **zero-new-dependency** discipline. So the header carries a
**build table** — the ordered, de-duplicated list of build-ID strings it references — and each `BuildMap`'s
owner is a **uint32 index** into it (index 0 = the zero/gap owner, E2B's `uuid.Nil`). This keeps fixed-width
mapping entries, supports string IDs, is **smaller** than a per-entry uuid when few builds are referenced, and
adds no dependency. It is a deliberate, documented divergence (an improvement, not a simplification) from
E2B's per-entry uuid.

### Decision 6 — format **v2** for the layered (rootfs) header; the memfile stays **v1** this stage
The owner-bearing format is **Version 2**. The **rootfs** uses v2. The **memfile** keeps emitting the
Stage-17 **v1** (single-build, no owner) header — its UFFD path is untouched this stage (memfile COW is Stage
19). The deserializer dispatches on `Version`, so both coexist and an old (v1) memfile bucket still boots.
Stage 19 migrates the memfile to v2 when it gains a real owner. Touch only what the stage owns.

### Decision 7 — the parity oracle stays behavioral (unchanged since Stage 11)
Where a rootfs's blocks physically sit and which build owns them are invisible to the wire. The Python e2e
suite (currently **43/43**) is the oracle: a sandbox booted from a layered (assembled) rootfs must run code,
keep kernel state, and expose ports exactly as before; a **new** layered-template test asserts a `base`-derived
template carries its added content (e.g. an extra package) **and** that `{B}/rootfs.ext4` is materially smaller
than a full rootfs (the COW win, measured). 18c reports the honest bytes-stored / assemble-time numbers.

### Decision 8 — a layered build must mkfs the child at the **base's fixed size** (measured; the COW precondition)
E2B's rootfs diff is small because a layer is the **same block device** mutated in place. Our `build-rootfs.sh`
instead `mkfs.ext4 -d`s a **fresh** image each build, sizing it `du(content) + margin` — so a naive block diff of
two independently-built images depends entirely on whether `mkfs.ext4 -d` lays the shared files at the **same
block offsets**. Measured on this box (synthetic A vs B = A + a small "package"):

| how A and B are mkfs'd | changed blocks (B vs A) |
|---|---|
| **same fixed size**, B = A + one file | **0.7%** |
| **different sizes** (`du+margin` each, B larger) | **31%** — changing the image size shifts ext4's block-group layout, so most "changed" blocks are spurious relocations, not content |
| **same fixed size**, B = A + a multi-file package (~3 MiB) | **2.9%** (~the genuinely-added bytes) |

So `mkfs.ext4 -d` *is* deterministic and append-friendly **at a fixed size**, but resizing reshuffles the layout.
The COW precondition is therefore: **a layered build pins the child rootfs to the base's exact size** (the
single-machine analogue of E2B's shared device). 18c teaches `build-rootfs.sh` a fixed-size argument and passes
the base's size; it fails loudly if the child's content exceeds the base size (the user must rebuild the base
with more margin). The storage **mechanism** (18b) stays correct for *any* sizes — it just stores a bigger diff
when sizes differ — so the size-pin is an efficiency precondition, not a correctness one. **Honest:** without the
size-pin the diff is ~31% (still less than a full image, but far from the 2.9% the pin buys).

> **18d amendment — the size-pin is necessary but NOT sufficient (measured on the real e2e).** The table above was
> measured by mutating *one* mkfs input (size or a single appended file). The real layered build adds a Docker
> **layer** (`RUN …`) and re-exports: `docker export | mkfs.ext4 -d` then lays the filesystem out differently than
> the base's export did, so ~half the *content* blocks move even at the pinned size. Measured here: two fresh mkfs
> of the **same image** at 576 MiB differ by ~3% (~4,375 blocks — mkfs is deterministic), but the real
> `derived` (default + a `RUN` layer) differs from the base by **71,364 blocks (~278 MiB, ~48%)**, so its stored
> diff is 278.8 MiB (2.07×), not ~3%. The missing precondition is **block-layout preservation across a content
> change**, which E2B gets by mutating a persisted block device in place; our re-create-each-build pipeline does
> not. This is a known divergence (§10, "layout preservation"), not a defect in the merge algebra — which stays
> correct for any layout. The honest win banked is the *mechanism* + a real (if bounded ~2×) byte reduction.

## 5. Code "from → to" map

| concern | from (Stage 17) | to (Stage 18) |
| --- | --- | --- |
| header owner | none (single build) | `BuildMap.Owner` (build-table index); `Metadata.BuildId/BaseBuildId/Generation`; format v2 |
| merge algebra | none | `CreateMapping` / `MergeMappings` / `NormalizeMappings` / `Resolve` (ported from E2B `mapping.go`) |
| rootfs storage | whole `{buildID}/rootfs.ext4`, no header | layered: `{B}/rootfs.ext4` = diff blocks + `{B}/rootfs.ext4.header` (flattened mapping) |
| diff producer | n/a (zero-detection only) | `BuildDiff(base, child)` — block compare, changed/non-empty blocks only |
| rootfs boot | `Materialize` whole | `MaterializeLayered` — assemble from each run's owning build (multi-build cache); whole-object fallback |
| memfile | compacted, v1 header, UFFD | **unchanged** (single-build, Stage 17) |
| template build | flat (no base) | optional `base` → diff over it; api `from`, SDK `base=` |
| deps | none added | **none** (owner = table index, not uuid) |

## 6. Independently verifiable sub-steps

> Re-split after 18a: the empirical size-pin finding (Decision 8) made the original 18b — "producer +
> boot wiring" in one step — too large and entangled with `build-rootfs.sh`. It is now 18b (the pure
> `pkg/storage` mechanism, KVM-free) + 18c (the build/orchestrator wiring incl. the size-pin) + 18d
> (api/SDK + the real e2e + docs). The header substrate (18a) and the storage mechanism (18b) stay
> KVM-free and fully unit-tested before any VM/build path is touched — the 14a/15a/17a discipline.

### Stage 18a — `pkg/storage/header`: the owner + the merge algebra (no wiring)
Add to the header package: the owner-bearing `BuildMap`/`Metadata` (format v2) + the build table; `CreateMapping`
(bitset → runs), `MergeMappings`, `NormalizeMappings`, `BuildDiff(base, child, blockSize)`, and `Resolve(offset)`.
Keep v1 read for the memfile. **KVM-free unit tests** (porting E2B's `mapping_test.go`/`diff_test.go` cases):
serialize↔deserialize v2 round-trip incl. the build table; `BuildDiff` of a child that changes a known block
set → expected diff bytes + child mapping; `MergeMappings` over the full overlap matrix (base-before-diff,
diff-inside-base with left/right remainders, base-inside-diff, partial overlaps) → a whole-covering flattened
mapping; `NormalizeMappings` coalescing; `Resolve` returning the right `(owner, storageOffset)` for layered
ranges and the zero/gap owner otherwise. Nothing else changes; `go test ./services/...` green. (Mirrors how
17a/14a/15a banked the format before swapping behavior.)

### Stage 18b — `pkg/storage`: the rootfs COW mechanism (no build/VM wiring)
`pkg/storage` gains `RootfsHeaderName` + `OpenRootfsHeader` (the rootfs analogue of `OpenMemfileHeader`),
`PublishRootfsDiff(baseBuildID, childRootfsPath, childBuildID)` (materialize+assemble the base's full rootfs,
`header.BuildDiff` the child against it, `NormalizeMappings(MergeMappings(baseMapping, childDiff))`, upload the
compacted diff as `{child}/rootfs.ext4` + the flattened v2 header as `{child}/rootfs.ext4.header`), and
`MaterializeLayered(buildID, dst)` (assemble the baked rootfs by reading each run from its owner's object via a
per-owner `OpenReaderAt` cache, gaps/zero-owner runs left zero in a truncated file; **no header → today's whole
download**, the Stage-15 fallback). **KVM-free unit tests over a `Local` two-build fixture**: a base uploaded
whole + a child diff → `MaterializeLayered(child)` reconstructs the child byte-for-byte; a three-layer chain
(A→B→C) assembles correctly; a child that zeroes a block assembles zeros there; the no-header base falls back to
the whole object; the stored `{child}/rootfs.ext4` holds only the changed non-zero blocks (measured smaller).
Nothing in `pkg/build`/orchestrator/api changes; `go test ./services/...` green. (The 17a/15a discipline: bank
the mechanism, unit-tested, before any VM/build path.)

### Stage 18c — wire the producer + boot path (build/orchestrator), incl. the size-pin ✅ done
`build-rootfs.sh` learns a fixed-size argument; `pkg/build` for a `base`-set build resolves the base alias,
materializes the base rootfs, builds the child **pinned to the base's size** (Decision 8), and publishes via
`PublishRootfsDiff` (no `base` → today's whole upload, unchanged). The orchestrator's `prepareSpawn`/
`prepareRestore` probe `rootfs.ext4.header` → `MaterializeLayered`, else `Materialize` whole; the
`TemplateService` carries an optional `base`. Unit-test the build command sequence (the size-pin arg) with the
injectable executor; the assemble path is already covered by 18b.

### Stage 18d — wire the layered template API/SDK + docs + honest review ✅ done
`TemplateCreate` carries an optional `base`; api `POST /templates` gains `from`; SDK `build_template(base=…)`.
A new Python e2e: build `derived` with `base=default` + a `RUN pip install <small pkg>`, create a sandbox on it,
assert the package imports (content carried) **and** that the stored rootfs diff is materially smaller than a
full rootfs (the COW win). Finalize this doc's status + measured outcome; update `CLAUDE.md`, `docs/ARCHITECTURE.md`,
the roadmap (§10 item 3 → done for the rootfs; memfile COW = Stage 19), and **apply the compression
correction** (§ "Correction" above) to STAGE15/STAGE17/roadmap. Full e2e re-run; 🟢 self-review.

## 7. Keeping tests green (honest trade-offs)

- **No new provisioning.** Like Stage 17, this adds **no service and no dependency** — minio already holds the
  artifacts; we add one sibling object (`rootfs.ext4.header`) and store a derived rootfs as a diff. A plain
  `pytest` needs exactly what Stage 16/17 needed.
- **Go units stay hermetic + grow.** The header (owner + merge) is pure: format, `BuildDiff`, and the full
  `MergeMappings` overlap matrix are unit-tested with no network/KVM. `MaterializeLayered` is tested over a
  `Local` two-build fixture. The S3-touching paths still self-skip without `MSB_TEST_S3_ENDPOINT`.
- **Behavioral parity, plus a real measured win.** The stage must not change existing behavior; 43/43 + the new
  layered-template case is the proof. Unlike Stages 13–15, the win is concrete and reported: a derived rootfs
  stores only its diff (measured bytes), not a fidelity-only change.
- **Fallback is tested, not hoped.** Decision 4's no-header rootfs path has a unit test; the `default` template
  (no base) is unchanged.
- **Safety note carried forward.** Layering changes only where bytes sit and which build owns them; the sandbox
  stays inbound-reachable / outbound-denied (Stage 12), auth stays the Stage-16 learning seam. Nothing here
  makes it safe to expose to untrusted input.

## 8. New dependencies

**None.** The owner is a header-local build-table **index** (uint32), not `github.com/google/uuid` (E2B's
per-entry uuid), so the format stays hand-serialized fixed-width little-endian — the Stage-17 discipline. See
Decision 5. The build IDs themselves are the strings we already use as object-key prefixes.

## 9. What this completes

Stage 18 turns the Stage-17 single-build memfile index into E2B's **layered** storage model on the rootfs:
a build can be a **diff over a base**, the header records **which build owns** each run, `MergeMappings`
flattens a layer chain, and the boot path **assembles** from the owning builds — so a derived template stores
only its delta. The `pkg/storage/header` package now holds the full COW algebra (`CreateMapping`/`MergeMappings`/
`NormalizeMappings`/`Resolve`), the shared substrate the **memfile COW** (Stage 19) and the **NBD-streamed
rootfs** both consume — the latter would serve *this same layered header* lazily instead of assembling.

## 10. Known divergences from E2B (verified against `e2b-dev/infra`; deferred or improved)

Faithful on the **mechanism this stage owns** (per-build diff objects + per-entry owner + `MergeMappings`
flatten + multi-build read + empty→zero), deliberately simpler or improved on the rest:

| axis | E2B (real) | this stage | status |
|---|---|---|---|
| owner type | per-entry `uuid.UUID` | per-entry **build-table index** (uint32) | 🟢 improved (zero-dep, smaller) |
| diff source | runtime **dirty bitset** (Firecracker diff snapshot / page tracking) | build-time **block compare** vs base image | 🟡 analogue (Decision 2) |
| layered artifact | both **memfile and rootfs** layered | **rootfs only**; memfile single-build | 🔴 memfile COW deferred to Stage 19 (Decision 1) |
| boot read | multi-build read, **lazy** (UFFD memfile / NBD rootfs) | multi-build read at **materialize** (assemble whole rootfs) | 🟡 lazy streaming = NBD stage |
| flatten | `MergeMappings` at snapshot time | `MergeMappings` at build time | 🟢 faithful |
| empty blocks | `empty` bitset → `uuid.Nil`, served as zeros | gap/zero owner (index 0), served as zeros | 🟢 faithful |
| compression | **none** (myth corrected — see header) | none | 🟢 faithful (E2B doesn't compress) |
| **layout preservation** | base+child share a **persisted block device** mutated in place, so a child's diff is only the genuinely-changed blocks | **Stage 19:** a layered child copies the base's rootfs image and applies only its delta in place via `debugfs` (no re-mkfs), so unchanged files keep their blocks | 🟢 **closed in Stage 19** — the same `derived` diff dropped from 278.8 MiB (2.07×) to 28 KiB (0.0047%); see `docs/STAGE19_DESIGN.md` |
| cross-node cache | NFS-wrapped shared chunks | per-build local object cache | 🟡 multi-host — deferred |

## 11. Deferred follow-on — Stage 19 (memfile COW via live-VM snapshot), sketched

The memfile half needs what this stage deliberately avoids: a **meaningful** memfile diff, which only the
**same VM continuing** produces. Sketch of the later stage, consuming this stage's header:
1. **Live-VM snapshot creation** — restore a base build, optionally run build commands in the guest, **pause**
   the VM and take a snapshot (Firecracker `PUT /snapshot/create` on a paused VM — new; today we only restore).
2. **The memfile diff** — the new full memfile vs the base's: a block compare (our analogue) or, faithfully,
   Firecracker's **diff snapshot + dirty bitmap**. Store `{B}/memfile` = changed blocks, v2 header owned by B
   over A (the same `MergeMappings` this stage builds).
3. **UFFD multi-build read** — `prepareRestore` builds a page source that, per fault, resolves the owner via
   the header and range-reads **that build's** compacted memfile (a per-build chunked-source cache — E2B's
   `DiffStore`/`build.File.ReadAt`). This is the multi-build read done *lazily* over UFFD, the richer lesson
   this stage sets up but does not need.

None of these change the Stage-18 *seam*; they deepen the *mechanism* behind the same `header` interfaces,
which is why Stage 18 (the algebra + the rootfs proof) lands first.
