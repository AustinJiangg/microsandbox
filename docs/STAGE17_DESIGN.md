# Stage 17 design: storage-mechanism depth (1) — a `.header`-indexed, compacted memfile

> Status: **proposed (design only).** First of the deferred "storage-mechanism depth" items the
> roadmap parked behind the Stage-15 `StorageProvider` / `PageSource` seams
> (`docs/E2B_ALIGNMENT_ROADMAP.md` §5 "Still deferred"; `docs/STAGE15_DESIGN.md` §11). Stage 15 made
> object storage the source of truth and streamed the memfile page-by-page over UFFD, but as **one
> flat, full-size artifact** read in 1 MiB chunks with **no index** — so a fault anywhere pulls (and
> the build stores) even the snapshot's vast zero regions. This stage adds E2B's `pkg/storage/header`:
> a per-block **mapping** that records which logical ranges are actually present, lets the builder
> **compact** the stored memfile to only its non-zero blocks, and lets the boot path **serve zero
> ranges without a fetch** and **range-read only present bytes**. Read `docs/STAGE15_DESIGN.md` (the
> object-storage seam + the `chunkedSource` this deepens) and `docs/STAGE13_DESIGN.md` (the UFFD
> handler the source feeds) first.
>
> **This design is verified against the real `e2b-dev/infra` source** (`packages/shared/pkg/storage/header`
> — `header.go`, `metadata.go`, `mapping.go`), not from memory. The faithful core: a `Metadata` +
> ordered `[]BuildMap` mapping where each entry maps a logical byte range to a `(build, storage offset)`
> and **unmapped gaps are served as zeros, never read from storage**. The deliberate single-build
> simplifications (we drop the per-entry `BuildId` / `BaseBuildId` until COW layers land; no compression
> yet) are named in §4 and §10, not papered over — the same method every prior stage held to: "reproduce
> E2B's *seams* with single-machine-appropriate implementations behind E2B-shaped interfaces."
>
> **Decisions taken with the user:** (1) of the four §11 gaps (NBD rootfs, chunked+`.header`+compression,
> COW layers, cross-node cache), tackle **the `.header` index first** — it is E2B's shared foundation (the
> NBD rootfs, COW builds, and chunk cache all consume the header), it *deepens existing code*
> (`uffd.chunkedSource`) rather than opening a new subsystem, it is fully KVM-free unit-testable, and it
> is the **first storage stage with a real chance of doing less work** (zero pages stop being fetched and
> stored), not just adding fidelity. (2) Use the header to **compact** the stored memfile (faithful to
> E2B — storage holds only present bytes, and the remap is the COW foundation), not merely index a
> full-size object (Decision 2).
>
> **Why bother, honestly.** Unlike Stages 13–15 (each "fidelity, not speed on one box"), this one *can*
> cut work: a freshly-snapshotted memfile is mostly zero, so compaction shrinks the stored object and
> the boot fetches only the guest's touched **non-zero** pages. Whether that nets a wall-clock win on
> this loopback box (where the bytes saved are cheap to move anyway) is **measured in 17b and reported
> as-is** — no pre-claim. The durable win is the header mechanism itself: it is the precondition for the
> NBD rootfs (same index), COW layered builds (the per-entry build owner), and a cross-node chunk cache.

## 1. Goal & non-goals

**Goal.** Give the streamed memfile E2B's `pkg/storage/header` mechanism, end to end:

- **`pkg/storage/header`: a new package** (mirroring E2B's) holding the on-disk format — a fixed-size
  `Metadata` (version, block size, logical size) + an ordered **`Mapping`** of present ranges — plus
  `Serialize`/`Deserialize` and a `BuildFromMemfile(path, blockSize)` that scans a memfile into
  `(mapping, compacted-bytes)` by dropping all-zero blocks. Pure, KVM-free, the heart of the stage.
- **Producers compact + index.** `pkg/build` (API builds) and `cmd/msb-seed` (the baked default /
  script-built templates) stop uploading the raw memfile; they upload a **compacted** `{buildID}/memfile`
  (only non-zero blocks, concatenated) plus a `{buildID}/memfile.header` (the serialized `Metadata`+`Mapping`).
- **The boot path consumes the header.** The orchestrator fetches + parses `{buildID}/memfile.header`,
  and builds a header-aware `uffd.PageSource` that resolves each faulting **logical** offset through the
  mapping: a gap → a **zero page with no network fetch**; a present range → a **chunked range read of the
  compacted object** at the translated physical offset (the existing 1 MiB chunk cache, now over a dense
  object so every fetched byte is a real byte).
- **Stay backward/forward compatible.** A `{buildID}/` with **no** `memfile.header` falls back to today's
  full-object `chunkedSource` (old buckets still boot); the format carries a `Version` + `BlockSize` so
  COW layers (item 3) extend it without a flag day. The local-fs `MmapSource` and File-backend paths are
  **untouched** (they read the local raw memfile) — only the s3-streamed path gains the header.

**Non-goals** (bounded out / deferred — see §10):

- **rootfs stays materialized whole.** The header is **memfile-only** this stage. The rootfs is still
  downloaded to its baked path; header-indexing + lazy-streaming the rootfs is the **NBD stage** (§10
  item 1) — that is where the rootfs's header earns its keep.
- **snapfile stays whole.** Small, fetched whole, no header (faithful — E2B does the same).
- **No compression yet.** Blocks are stored raw. LZ4/zstd frames are a clean follow-on behind the same
  mapping (§10 item 2b) and are deferred so this stage isolates the *index*.
- **No COW layers / no per-entry build owner.** A single flat build → every present block belongs to
  *this* build, so the `Mapping` drops E2B's per-entry `BuildId`/`BuildStorageOffset`-into-another-build.
  Layered diffs are §10 item 3. The format keeps a `Version` so adding the owner is additive.
- **No cross-node cache, no auth/TLS/perf claim.** Same standing caveats as the whole repo; still a
  learning implementation, **not security-audited**, never safe for untrusted input.

## 2. Target architecture (what moves)

The data path, the wire, the SDK, and the local-fs / File-backend escape hatches are **unchanged**. Only
*how the memfile is stored* and *how the s3 boot path reads it* move.

```
  BEFORE (Stage 15)                                AFTER (Stage 17, s3 mode)

  build / seed → Upload {buildID}/memfile          build / seed → header.BuildFromMemfile(memfile, 4KiB):
                  = the RAW full memfile                            • compacted = concat(non-zero blocks)
                  (zeros and all)                                   • mapping   = present ranges -> phys offsets
                                                                  Upload {buildID}/memfile        = compacted
                                                                  Upload {buildID}/memfile.header = Metadata+Mapping

  prepareRestore (s3):                             prepareRestore (s3):
    rr = OpenReaderAt {buildID}/memfile              if Exists {buildID}/memfile.header:
    src = NewChunkedSource(rr)   ── per-fault          hdr = Deserialize(Open {buildID}/memfile.header)
      pulls the 1 MiB chunk around the LOGICAL         rr  = OpenReaderAt {buildID}/memfile   (compacted)
      offset, even across zero regions                 src = NewMappedSource(rr, hdr.Extents(), blockSize)
                                                       └─ fault @ logical off:
                                                            gap   -> zero-fill p, NO fetch
                                                            present-> chunked range read @ phys off
                                                     else:  src = NewChunkedSource(rr)   (Stage-15 fallback)

  uffd.Serve(uds, src)  ── UNCHANGED: still UFFDIO_COPY of the buffer the source fills
                            (a gap's buffer is simply zeros; the serve loop never learns about the header)
```

Component → what changes:

| Component | Change |
|---|---|
| `pkg/storage/header` (**new**) | `Metadata` + `Mapping` (`[]BuildMap`) format; `Serialize`/`Deserialize`; `BuildFromMemfile(path, blockSize) → (Mapping, compacted io.Reader/temp)`; `Extents()` (mapping → plain offset triples for uffd). KVM-free. |
| `pkg/uffd` | `NewMappedSource(ra, extents, blockSize)` — a `PageSource` that zero-fills gap blocks (no fetch) and chunk-fetches present blocks. Stays **storage-free**: `extents` is a slice of plain ints, not a storage type. `chunkedSource` kept for the no-header fallback. |
| `pkg/build` | replace the raw memfile upload with: build header+compacted, upload `{buildID}/memfile` (compacted) + `{buildID}/memfile.header`. rootfs/snapfile uploads unchanged. |
| `cmd/msb-seed` | same swap for the baked/script-built memfile. |
| `cmd/orchestrator` (`prepareRestore`) | if `memfile.header` exists: parse it, build `NewMappedSource`; else fall back to `NewChunkedSource` (Stage-15 behavior). |
| `pkg/storage` | a `HeaderName = "memfile.header"` constant + (optionally) a small `OpenHeader` convenience; no interface change. |
| deps | **none** (we drop `uuid`; format is hand-serialized little-endian, like the uffd ABI structs). |

## 3. The header format — the heart of the stage

A Firecracker memfile is a flat image of guest RAM: `Metadata.Size` bytes, almost all zero right after a
boot snapshot. We index it in `BlockSize` blocks and store only the non-zero ones.

**`Metadata`** (fixed-size, `binary.LittleEndian`, mirroring E2B's `metadata.go`):

```go
type Metadata struct {
    Version   uint64 // format version (start at 1; COW/compression bump it)
    BlockSize uint64 // index granularity in bytes (= guest page size, 4096)
    Size      uint64 // total LOGICAL size of the memfile (so gaps past the last present block read as zero)
    // E2B also carries Generation, BuildId, BaseBuildId (uuid) — dropped until COW (§10 item 3).
}
```

**`Mapping`** — an ordered `[]BuildMap` of the **present** (non-zero) logical ranges, mirroring E2B's
`mapping.go` minus the build owner:

```go
type BuildMap struct {
    Offset             uint64 // logical byte offset in the memfile (block-aligned)
    Length             uint64 // length of this present run (block-multiple)
    BuildStorageOffset uint64 // byte offset of this run inside the COMPACTED {buildID}/memfile object
    // E2B's BuildId uuid.UUID (which build owns this range) is dropped until COW lands (§10 item 3).
}
```

**Resolution (the lookup the page source does), faithful to E2B:** for a faulting logical offset `L`,
binary-search the sorted mapping for the entry `e` with `e.Offset ≤ L < e.Offset+e.Length`:
- **found** → read the compacted object at `e.BuildStorageOffset + (L − e.Offset)` (E2B's exact
  `BuildStorageOffset + shift` arithmetic);
- **not found** (a gap, incl. past the last entry but `< Size`) → **zero**, no storage read (E2B's
  `BuildId == uuid.Nil` gap rule).

**Build (`BuildFromMemfile`), faithful to E2B's `CreateMapping` over a dirty bitmap:** walk the memfile in
`BlockSize` blocks; a block is "present" iff it has any non-zero byte. Coalesce maximal runs of present
blocks into one `BuildMap` (so the mapping is `#runs` entries, tiny — a mostly-zero memfile has few runs),
appending each run's bytes to the compacted output and assigning a sequential `BuildStorageOffset`. Zero
runs emit nothing. (Our "is this block zero" is the single-build analogue of E2B's dirty bitmap, which it
gets from the diff between a build and its parent — same shape, simpler source.)

**On-disk object** `{buildID}/memfile.header` = `Metadata` ‖ `uint64(len(mapping))` ‖ `mapping[]` (each
`BuildMap` is three little-endian `uint64`s). Self-describing, fixed-width, no external schema — the same
hand-rolled binary discipline `pkg/uffd` uses for the kernel ABI structs.

## 4. Key design decisions

### Decision 1 — `.header` index first, of the four §11 gaps (with the user)
It is E2B's **shared foundation**: `pkg/storage/header` is consumed by the memfile UFFD path *and* the
rootfs NBD path *and* COW builds *and* the chunk cache. Doing it first unblocks the rest; doing NBD first
(higher headline impact) would still need this underneath. It also deepens **existing** code
(`uffd.chunkedSource` → header-aware) rather than opening the NBD subsystem, so it stays one
Conventional-Commit-sized stage, KVM-free testable.

### Decision 2 — **compact** the stored memfile (faithful), not index-only (chosen with the user)
Two ways to use the header:
- **(A) Compaction (chosen):** store only non-zero blocks; the mapping remaps logical → physical. Faithful
  to E2B (storage holds only present/delta bytes), shrinks the upload, and the remapping arithmetic is
  *exactly* what COW layers reuse — so it is the real foundation, not a detour.
- **(B) Index-only (rejected):** keep the full-size object; the header only marks which blocks to skip
  fetching. Simpler (no remap), still avoids fetching zeros at boot, but stores the zeros and throws away
  the remap step COW needs.
We take **(A)** because the stage's stated purpose is the *foundation*, and the remap is the lesson. (B) is
noted as the lower-effort fallback if (A)'s build step proves troublesome.

### Decision 3 — block size = guest page size (4 KiB)
The UFFD handler faults at the region page size (4 KiB on this box). Setting `BlockSize` = page size means
**every faulting page maps to exactly one block** → wholly zero or wholly present, no mixed pages, and
maximal zero-compaction. The mapping stays tiny anyway because present blocks are **run-length coalesced**.
Physical reads remain **chunked at 1 MiB** over the dense compacted object (the existing cache), so we keep
the "few big range GETs, not thousands of tiny ones" property Stage 15 chose (Decision 6 there). The
`MappedSource` fills a fault buffer block-by-block so it stays correct even if a region ever uses larger
(huge) pages spanning many blocks.

### Decision 4 — `pkg/uffd` stays storage-free
`source_bucket.go` deliberately takes a plain `io.ReaderAt` "so pkg/uffd stays free of any storage / minio
import." We keep that: `NewMappedSource` takes the compacted object's `io.ReaderAt` **plus a plain
`[]Extent{Logical, Length, Physical uint64}`** (gaps are simply the ranges no extent covers). The
orchestrator parses the `pkg/storage/header` bytes and converts to `[]Extent`; uffd never imports storage.
This keeps the ioctl/unsafe package's dependency surface minimal, as Stage 13/15 insisted.

### Decision 5 — the header is optional; absence = the Stage-15 path
The boot path probes `Exists {buildID}/memfile.header`. Present → `MappedSource`. Absent → today's
`NewChunkedSource` over the full object. So a bucket seeded before this stage still boots unchanged, and
the s3/File/local-fs escape hatches are untouched. The e2e fixture re-seeds, so it exercises the new path;
the fallback is covered by a unit test, not left theoretical.

### Decision 6 — the parity oracle stays behavioral (unchanged since Stage 11)
Where the memfile's bytes physically sit and how zeros are elided are invisible to the wire. The Python
e2e suite (currently **43/43**) is the oracle: a sandbox booted from a compacted+indexed memfile must run
code, keep kernel state, and expose ports exactly as before. Green against MinIO proves the swap moved
*storage mechanism*, not behavior. 17b additionally **measures** bytes-fetched and restore-to-ready and
reports the honest number (Stages 13–15's discipline).

## 5. Code "from → to" map

| concern | from (Stage 15) | to (Stage 17) |
| --- | --- | --- |
| header format | (none) | `pkg/storage/header`: `Metadata` + `Mapping`, `Serialize`/`Deserialize`, `BuildFromMemfile` |
| stored memfile | raw, full-size `{buildID}/memfile` | **compacted** `{buildID}/memfile` (+ `{buildID}/memfile.header`) |
| who builds it | `pkg/build` / `msb-seed` upload the raw file | both compact+index, upload object + header |
| uffd page source | `NewChunkedSource(rr)` (logical == physical) | `NewMappedSource(rr, extents, blockSize)`; `chunkedSource` kept for fallback |
| zero pages at boot | fetched as part of their 1 MiB chunk | **served as zeros, never fetched** |
| boot resolution | offset == object offset | logical → physical via the mapping; gap → zero |
| compatibility | n/a | header optional; absent → Stage-15 full-object path |
| deps | minio-go (already) | **none added** |

## 6. Three independently verifiable sub-steps

### Stage 17a — `pkg/storage/header`: the format + builder (no wiring)
Add the package: `Metadata`/`BuildMap`/`Mapping`, `Serialize`/`Deserialize` (fixed-width little-endian),
`BuildFromMemfile(path, blockSize) → (Mapping, compactedPath/Reader, error)` (zero-block detection + run
coalescing + sequential storage offsets), and `Extents()`/resolution helpers. **KVM-free unit tests**:
serialize↔deserialize round-trip; a synthetic memfile with known zero/non-zero blocks compacts to the
expected size and mapping; offset resolution returns the right physical offset for present ranges and the
zero/gap signal otherwise (incl. the tail past the last entry, and a fully-zero memfile → empty mapping).
Nothing else changes; `go test ./services/...` green. (Mirrors how 14a/15a banked the interface before
swapping behavior.)

### Stage 17b — wire it: producers compact+index, the boot path consumes the header
`pkg/build` + `cmd/msb-seed` build the header+compacted object and upload both. `pkg/uffd` gains
`NewMappedSource` (+ unit tests over a `Local`/in-memory `io.ReaderAt`: gap → zeros with zero reads,
present → correct bytes, a fault buffer spanning a gap↔present boundary). `prepareRestore` probes the
header and builds the mapped source, else falls back. `conftest`/`dev-up` re-seed (the existing seed call
now writes the new form — usually no fixture change). Verify: `go test ./services/...` green; Python e2e
green against Postgres + Redis + MinIO (target **43/43**), memfile compacted + streamed via the mapping.
**Measure** restore-to-ready + bytes fetched vs Stage 15 and record the honest number.

### Stage 17c — docs, defaults, honest review
Finalize this doc's status; update `CLAUDE.md` (Done list + the storage line), `docs/ARCHITECTURE.md`
(the artifacts state-seam line), and the roadmap (move §11 item 2's *index* half to done; compression /
COW / NBD stay listed). Confirm the **warm pool** (it rides `Restore`, so it streams via the mapping too)
and the **template-build** path (it now compacts+indexes) behave. Re-run the full e2e and give the
🔴/🟡/🟢 self-review.

## 7. Keeping tests green (honest trade-offs)

- **No new provisioning.** Unlike Stages 14/15 (which added postgres/redis/minio), this stage adds **no
  service and no dependency** — minio already holds the memfile; we change its *contents* (compacted) and
  add one sibling object (the header). A plain `pytest` needs exactly what Stage 16 needed.
- **Go units stay hermetic + grow.** `pkg/storage/header` is pure (no network, no KVM): the format and the
  compaction math are fully unit-tested. The `uffd.MappedSource` test uses an in-memory `io.ReaderAt`, no
  MinIO. The S3-touching paths still self-skip without `MSB_TEST_S3_ENDPOINT`.
- **Behavioral parity, not a perf promise.** The stage must not change the e2e count or any observable
  behavior; 43/43 against MinIO is the proof. A latency win is *plausible here* (less fetched/stored) but
  **measured, not claimed** — 17b reports the real number, including if it is within noise on loopback.
- **Fallback is tested, not hoped.** Decision 5's no-header path has a unit test, so an old bucket booting
  is a covered case.
- **Safety note carried forward.** Compaction/indexing change only where bytes sit; the sandbox stays
  inbound-reachable / outbound-denied (Stage 12), auth stays the Stage-16 learning seam. Nothing here makes
  it safe to expose to untrusted input.

## 8. New dependencies

**None.** The format is hand-serialized fixed-width little-endian (`encoding/binary`), the same discipline
`pkg/uffd` uses for the kernel ABI structs. We deliberately **do not** pull `github.com/google/uuid` (E2B's
`BuildId` type) because single-build has no build owner to name yet; it enters with COW (§10 item 3),
keeping the static-binary line and zero-dep-growth.

## 9. What this completes

Stage 17 turns the Stage-15 "flat full object, range-read in chunks" memfile into E2B's
**indexed, compacted** memfile: storage holds only present blocks, the boot fetches only touched non-zero
pages, and the `pkg/storage/header` mechanism — the shared substrate of E2B's rootfs/memfile/COW/cache — now
exists in the tree, KVM-free and unit-tested. That is the precondition that makes the remaining depth items
*incremental* rather than new subsystems.

## 10. Known divergences from E2B (verified against `e2b-dev/infra`; deferred)

Faithful on the **mechanism this stage owns** (object storage + per-block mapping + gaps-as-zeros +
compaction), deliberately simpler on what the next items add. Verified against `pkg/storage/header`:

| axis | E2B (real) | this stage | status |
|---|---|---|---|
| metadata | `Version/BlockSize/Size/Generation/BuildId/BaseBuildId` | `Version/BlockSize/Size` | 🟡 owner/generation deferred to COW |
| mapping entry | `BuildMap{Offset,Length,BuildId,BuildStorageOffset}` | `{Offset,Length,BuildStorageOffset}` | 🟡 drop `BuildId` until COW |
| gaps | `BuildId == Nil` → zeros, never read | unmapped range → zeros, never read | ✅ faithful |
| compaction | store only present/delta blocks | store only present blocks | ✅ faithful (single build) |
| compression | LZ4/zstd frames per chunk | raw blocks | 🟡 **deferred** (item 2b) |
| build model | COW diff layers; header resolves a byte to its owning build | one flat build per memfile | 🟡 **deferred** (item 3) |
| rootfs | same header, served lazily over **NBD** | rootfs still materialized whole (memfile-only header) | 🔴 **deferred** (item 1) |
| cross-node cache | NFS-wrapped shared chunks | per-VM local chunk cache | 🟡 multi-host — **deferred** (item 4) |

**Deferred follow-ons (candidate later stages), in rough priority:**
1. **NBD-streamed rootfs** — give the rootfs the same header + a userspace NBD device, so it streams like
   the memfile and the baked-absolute-path problem dissolves. A whole subsystem; its own stage.
2b. **Compression** — LZ4/zstd frames behind the same mapping (`BuildStorageOffset` already addresses
   variable-size stored blocks), with the header recording each block's stored length.
3. **COW layered builds** — restore the per-entry `BuildId`/`BaseBuildId`; a child build stores only its
   diff and points unchanged ranges at the parent's storage (E2B's `BuildMap`).
4. **Cross-node chunk cache** — once multi-host lands, share fetched chunks between orchestrators (E2B's
   NFS wrap).

None change the Stage-17 *seam*; they deepen the *mechanism* behind the same `header`/`PageSource`
interfaces, which is why they slot in as later stages.
