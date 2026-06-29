# Stage 15 design: the last storage swap — `Local → object storage` (MinIO/S3)

> Status: **done (15a + 15b + 15c).** The third and final state seam of the roadmap's "storage swaps go
> live" item (`docs/E2B_ALIGNMENT_ROADMAP.md` §5 "Still deferred"). Stage 14 cashed in the two
> *isomorphic* seams (catalog → Redis, store → Postgres) and **deliberately split this one out**
> (`docs/STAGE14_DESIGN.md` §10) because it is **not isomorphic**: a template's artifacts can't
> merely live in a bucket and be opened by path — the Firecracker snapshot bakes in its rootfs's
> *absolute path*, so artifacts must be **materialized to a local path before boot**, and the
> memfile is **streamed page-by-page from the bucket via the Stage-13 UFFD handler** rather than
> downloaded whole. That memfile-streaming is the payoff Stage 13 was built to unlock ("once *we*
> supply the pages, the source need not be a local file"). Read `docs/STAGE13_DESIGN.md` (the
> UFFD page-fault handler) and `docs/STAGE14_DESIGN.md` (the other two swaps) first.
>
> **This design was verified against the real `e2b-dev/infra` source** (its `pkg/storage` +
> `pkg/storage/header` packages), not from memory — see §11 for the point-by-point fidelity map.
> The short version: the **core seam is faithful** (object storage as the source of truth, a
> `StorageProvider` with range reads, the memfile served lazily over UFFD from storage with a
> local cache, providers GCS/**S3**/Local), and the deliberate **single-machine simplifications**
> (we materialize the rootfs instead of NBD-streaming it; per-page instead of chunked+compressed;
> no COW layer diffs; no cross-node NFS cache) are named and deferred in §11 rather than papered
> over. This honors the roadmap's stated method — "reproduce E2B's *seams* with
> single-machine-appropriate implementations behind E2B-shaped interfaces" (§3) — with the lines
> drawn explicitly.
>
> **Two decisions taken with the user (mirroring the Stage 14 forks):**
> - **Backend: MinIO (real S3 API), via `docker-compose`.** Not a local-dir-as-bucket double —
>   the user chose maximal fidelity, exactly as Stage 14 flipped to real Postgres + Redis. The
>   orchestrator and the template builder speak the real S3 protocol (`GetObject` with HTTP
>   `Range`, `PutObject`) to a MinIO bucket on loopback. S3 is one of E2B's real providers
>   (`AWSBucket`), so this is faithful, not a stand-in.
> - **Flip the default.** Object storage becomes the **default** artifact source (the binaries
>   default to S3; the e2e provisions MinIO via compose and fails loudly without it), just as
>   Stage 14 made Postgres + Redis the default. A `--storage local-fs` escape hatch keeps today's
>   direct-from-`vendor/` path selectable.
>
> **Why bother, honestly.** On *one box* this is — like Stages 13 and 14 — **not a speedup**.
> Materializing rootfs + snapfile adds a download (a cache miss on a cold node), and serving the
> memfile as per-page HTTP `Range` GETs is *slower* than the kernel demand-paging a local mmap.
> The payoff is **fidelity and the property it unlocks**: artifacts live in a blob store keyed by
> immutable build identity, not addressed by host path; a node that did **not** build a template
> can still boot it (download rootfs/snapfile, stream the memfile) — the precondition for the
> deferred multi-host work. This doc claims no latency win; 15b measures the real number and
> reports it as-is (the same honesty Stages 13 and 14 held to).
>
> **Outcome (measured, this WSL2 box).** Full e2e **37/37 in s3 mode**, each snapshot restore
> streaming `default/memfile` over UFFD in 1 MiB chunks from MinIO (boots within the ~10s health
> timeout -- per-page would not, so chunked was chosen from the start, Decision 6); the escape hatches
> stay green (`--storage local-fs` File backend; `--storage local-fs --uffd` local mmap). As
> predicted, not a single-box speedup -- the win is real object storage + a page source pluggable end
> to end (mmap <-> bucket), the precondition for multi-host / peer-sourced memory. `minio-go` (pure
> Go) keeps the binaries static.

## 1. Goal & non-goals

**Goal.** Make a template's built artifacts live in object storage (MinIO/S3) as the source of
truth, and teach the **non-isomorphic** boot path that follows:

- **`pkg/uffd`: a pluggable page source.** Replace `Serve(uds, memfilePath)` with
  `Serve(uds, PageSource)`, where `PageSource` supplies one snapshot page at a memfile offset.
  Two impls: `MmapSource` (today's local mmap, factored out verbatim) and `bucketSource` (an S3
  `Range` read per page + a local page cache). This is the seam Stage 13 promised and 15 cashes.
- **`pkg/storage`: a real blob `StorageProvider`.** Reshape the path-returning `TemplateDir`
  interface into E2B's actual blob shape — a whole-object read, a **seekable/range read** (E2B's
  `OpenSeekable`, which is exactly the UFFD page source), `Upload`, and `Exists` — with an `S3`
  impl over `minio-go` and a `Local` (dir-as-bucket) impl kept as a hermetic unit-test double
  (mirroring how Stage 14 kept `catalog.InMemory` / SQLite).
- **buildID-keyed object layout + an alias for resolution.** Artifacts live under an **immutable**
  `{buildID}/{file}` prefix (E2B's `Paths`: `{buildID}/rootfs.ext4`, `/memfile`, `/snapfile`,
  `/metadata.json`). A mutable **alias object** `aliases/<template-name> → buildID` records the
  *current* build for a name; the orchestrator reads it to resolve a name → buildID (the
  single-machine stand-in for E2B's DB-side resolution — Decision 8).
- **The builder uploads; the orchestrator materializes + streams.** `pkg/build` uploads its three
  outputs to `{buildID}/…` and flips the alias. Before boot, the orchestrator resolves the name →
  buildID via the alias, **materializes** rootfs (Spawn + Restore) and snapfile (Restore) to their
  canonical local paths *if missing* (download from the bucket), and serves the memfile via the
  UFFD `bucketSource` (no download). The local `vendor/` tree becomes a materialization **cache**,
  not the source of truth.
- **Flip the default to S3; seed the bucket.** The binaries default to S3; `local-fs` stays
  selectable. The default template (the baked stock image, built outside the API by
  `scripts/build-*.sh`) gets the sentinel buildID `default` and is **seeded** into MinIO by the
  conftest/dev-up fixture; an API-built template is seeded by its own build.

**Non-goals** (bounded out / deferred — the fidelity gaps are itemized in §11):

- **No NBD-streamed rootfs.** E2B does *not* materialize the rootfs either — it serves it lazily,
  block-by-block, as a **userspace NBD block device** (the same chunked storage as the memfile,
  with OverlayFS/squashfs base sharing). We **materialize the rootfs whole** as a single-machine
  simplification; that is also *why* we have the baked-absolute-path problem and E2B does not (its
  rootfs is an NBD device, not a baked host file). Faithful rootfs streaming is a **deferred
  stage** (§11, item 1). Materializing teaches "fetch-before-boot"; it does not claim to be E2B's
  rootfs mechanism.
- **No change to the baked rootfs path.** The snapshot keeps baking the **`vendor/...` local
  path** (Decision 3); we do *not* rebuild snapshots against a separate cache dir. `vendor/` is the
  cache at the baked path; the download path is exercised by the fixture clearing the local copy
  after seeding (Decision 4).
- **No chunked/COW block storage (yet).** E2B stores rootfs+memfile as **chunked, copy-on-write layers**
  with a `.header` index tracking per-block provenance and `NotPresent/Dirty/Zero` state
  (`pkg/storage/header`). 15b starts with **per-page `Range` reads + a local page cache** on a single flat
  artifact set; chunked prefetch is measured-in only if the restore time demands it (Decision 6). The
  `.header` index landed in **Stage 17** (compacted memfile) and COW diff layers in **Stage 18** (rootfs);
  the cross-node NFS cache is still **deferred** (§11).
  > **Correction (Stage 18 source audit):** this once said E2B stores "**compressed**, chunked, …". That is
  > **false** — `e2b-dev/infra` stores **raw** blocks (no compression lib in its storage/build/orchestrator
  > paths; its diff writer writes raw bytes). Compression is **not** an E2B mechanism — it would be our own
  > optional extension, not a fidelity gap. The "compressed" wording is dropped here and throughout this doc.
- **No auth / TLS / multi-host.** MinIO runs with throwaway root creds on loopback, like
  Redis/Postgres in Stage 14. **Not** safe to expose — same standing caveat as the whole repo.
- **A latency claim.** See the honesty note above.

## 2. Target architecture (what moves)

The data path is unchanged; only **where artifacts live** and **how the boot path obtains them**
move. The snapshot's three files split by how firecracker (or we) must read them.

```
  BEFORE (Stage 14)                              AFTER (Stage 15, default = S3)

  build → writes vendor/templates/<name>/        build → writes vendor/… (local cache) AND
            rootfs.ext4, snapshot/{vmstate,                 Uploads to s3://msb/{buildID}/{rootfs.ext4,
            memfile}  (the source of truth)                 snapfile, memfile}, flips aliases/<name>→buildID

  fc.Restore reads the three files DIRECTLY       resolve name → buildID via aliases/<name>, then
    from the local path:                          fc.Restore, before /snapshot/load:
    • rootfs   = path_on_host                        • materialize rootfs  → vendor path  (if missing: GetObject {buildID}/rootfs.ext4 → file)
    • vmstate  = snapshot_path                       • materialize snapfile→ vendor path  (if missing: GetObject {buildID}/snapfile → file)
    • memfile  = File mmap  (or local UFFD)          • memfile: UFFD bucketSource — per-page Range GET of {buildID}/memfile + page cache
                                                                                  (NO full download)
                                                    then PUT /snapshot/load { mem_backend: Uffd → our handler }

                                                   ┌─ MinIO (docker-compose, :9000) ─┐
                                                   │  bucket msb/                      │
                                                   │   aliases/default → "default"     │  ← mutable current-build pointer
                                                   │   aliases/<name>  → "<buildID>"   │
                                                   │   default/{rootfs.ext4,snapfile,memfile}   (immutable, buildID="default")
                                                   │   <buildID>/{rootfs.ext4,snapfile,memfile} │  ← memfile: per-page Range GET (UFFD)
                                                   └───────────────────────────────────┘
```

Component → what changes:

| Component | Change |
|---|---|
| `pkg/uffd` | `Serve(uds, PageSource)`; `PageSource` interface; `MmapSource` (factored out) + `bucketSource` (S3 Range + cache) |
| `pkg/storage` | blob `StorageProvider` (`Upload`/`Open`(whole)/`ReadAt`(range, → `io.ReaderAt`)/`Exists`); `S3` (minio-go) + `Local` (dir-as-bucket, test double); `{buildID}/{file}` keys + an `aliases/<name>` resolver |
| `pkg/build` | after building, **uploads** the three artifacts to `{buildID}/…` and flips `aliases/<name>` |
| `pkg/fc` | `Spawn`/`Restore` take an artifact source; **materialize** rootfs/snapfile if missing; build the memfile `PageSource` (mmap for local-fs, bucket for S3) |
| `cmd/orchestrator` | `--storage` (`s3://…` default, `local-fs`), `--s3-endpoint/-bucket/-access-key/-secret-key`, `--cache-dir` (= `vendor`); construct the `StorageProvider`; resolve alias → buildID; thread into the fleet |
| infra | `docker-compose.yml` gains `minio` (+ a one-shot bucket-create); conftest/dev-up seed `vendor/` artifacts into MinIO (under `default/…` + the alias) and point the binaries at it |
| deps | + `github.com/minio/minio-go/v7` (pure Go) |

## 3. The three artifacts — the heart of the stage

A Firecracker snapshot is three files, and **each has a different "must be local" constraint**.
Getting this split right *is* Stage 15:

| artifact | who reads it | constraint (in *our* design) | Stage 15 handling |
|---|---|---|---|
| `rootfs.ext4` | firecracker, as the root block device (`path_on_host`); **the snapshot bakes its absolute path** (`fc.go:201`) | must be a **local file at the baked path** | **materialize** to the `template.Resolve` path if missing (S3 `GetObject` → file) |
| `snapfile` (our `vmstate`) | firecracker, `PUT /snapshot/load` `snapshot_path` (`fc.go:258`) | must be a **local file** | **materialize** to the local path if missing |
| `memfile` | File backend → firecracker mmap; **Uffd backend → our `pkg/uffd` handler** (`fc.go:242-255`) | File: local file; **Uffd: any `PageSource`** | **stream** page-by-page from S3 via `bucketSource` — no download (the payoff) |

So Stage 15's default boot is: **two materialized files + one streamed file.** rootfs and snapfile
are needed in full immediately, so a whole-object download is right *for our materialize approach*;
the memfile is large and touched lazily, so streaming the touched pages is the win — and it is
*only possible* because Stage 13 made us the memory supplier.

**Why we stream the memfile but materialize the rootfs — and why that differs from E2B.** Under the
Uffd backend, the memfile is read by **our** handler via `UFFDIO_COPY` from a `src` pointer; Stage
15 just changes where those bytes come from (an mmap vs an S3 page cache). The rootfs, by contrast,
firecracker `open`s itself as a host block-device path — so to serve *it* lazily you must interpose
a **block device** under firecracker. **E2B does exactly that**: it presents the rootfs as a
userspace **NBD** device backed by the same chunked object storage, so it streams the rootfs too
and never materializes it. We **choose not to** build an NBD subsystem this stage and materialize
the rootfs instead — a deliberate single-machine simplification (§11, item 1), not a claim that the
rootfs is un-streamable. The asymmetry in *our* design (stream memfile, materialize rootfs) is a
scoping choice; in E2B both stream.

## 4. Key design decisions

### Decision 1 — backend is **MinIO (real S3)**, provisioned by compose (with the user)
The user chose fidelity over a local-dir double, exactly as Stage 14 chose real Postgres + Redis.
MinIO speaks the S3 API, so the code is real-S3 code (it would run unchanged against AWS S3, which
is one of E2B's own providers, or GCS's S3 endpoint). docker is already a hard dependency and
compose already runs postgres + redis (Stage 14), so a `minio` service adds no new *class* of
dependency. The `Local` dir-as-bucket impl survives **only as a hermetic unit-test double** (it
can't be the running default once the default is flipped) — the same demotion `catalog.InMemory`
took in Stage 14.

### Decision 2 — flip the default to S3; UFFD becomes the default memfile supplier (with the user)
The binaries default to `--storage s3://…`; the e2e provisions MinIO and **fails loudly** without
it (that is what "flip the default" means, per Stage 14 Decision 1). The sharp consequence, stated
plainly: **flipping to object storage makes the UFFD handler the default memfile supplier** (now
sourced from the bucket). This does **not** contradict Stage 13's "File stays default" — Stage 13
measured a *local* memfile, where UFFD has no edge over the kernel's own demand-paging; with a
*remote* memfile, UFFD streaming is the only way to avoid a full download, so it is the natural
default *there*. The standalone `--uffd` flag still selects UFFD-vs-File for the **`local-fs`**
mode (a local memfile); in `s3` mode the memfile is streamed via UFFD regardless. (An `s3` +
File-backend "download the whole memfile too" path is a trivial fallback we keep but don't
default to.)

### Decision 3 — the snapshot keeps baking the `vendor/` path; `vendor/` is the cache
The non-isomorphic crux is the baked absolute rootfs path (`fc.go:201`, `scripts/build-snapshot.sh`).
The lowest-risk choice: **do not change where the snapshot is baked** — it stays
`vendor/templates/<name>/rootfs.ext4` — and treat `vendor/` as the **local materialization cache at
exactly the baked path**. The orchestrator materializes rootfs/snapfile from the bucket *to that
same path* if absent, so the baked reference stays valid and `scripts/build-snapshot.sh` is
untouched. (E2B sidesteps the baked path entirely by serving the rootfs as an NBD device — see §11
item 1; a relocatable node-local cache dir with a rebaked snapshot is a noted later refinement.)

### Decision 4 — the memfile stream is e2e-proven; the materialize-download is unit-tested (revised)
The plan was to clear the local artifacts after seeding so the orchestrator must `GetObject` them
back, forcing the download path to run in the e2e. **In practice that was dropped.** The `ensure_*`
fixtures rebuild a *missing* local artifact via `docker` (and the root orchestrator's own template
builds leave a root-owned `~/.docker` buildx lock that makes a forced normal-user rebuild flaky), so
clearing the cache fights the fixtures rather than the orchestrator. What ships instead is just as
honest: the **memfile-streaming payoff is exercised in the e2e** (every snapshot restore streams
`default/memfile` from MinIO over UFFD — that path has no local fallback), and the
**materialize-if-missing download is covered hermetically** by `storage.TestMaterialize` (cache-miss
downloads to a temp path; cache-hit skips). In real multi-host use a fresh node has no local copy, so
the download runs naturally there. (A relocatable cache dir the snapshot is rebaked against — §11 — 
would let the e2e force a cold cache cleanly without fighting the rebuild fixtures; deferred.)

### Decision 5 — `minio-go`, because `*minio.Object` *is* the page source
Client choice (a clear best, not a user fork): `github.com/minio/minio-go/v7` is pure Go (holding
the static-binary line, like `pgx`/`go-redis`/`modernc.org/sqlite` before it) and its
`*minio.Object` already implements **`io.ReaderAt`** + `io.ReadCloser`. That maps one-to-one onto
both jobs: a whole-object `Download` (materialize rootfs/snapfile) and a `ReadAt(page, offset)` (the
UFFD `bucketSource`) — the same split E2B's `OpenBlob` / `OpenSeekable` draws. The full
`aws-sdk-go-v2` would also work but is heavier and has no `ReaderAt` convenience.

### Decision 6 — per-page Range read + a local page cache; chunked prefetch is measured-in if needed
The `bucketSource` serves a fault by `ReadAt`-ing one `page_size` window from the memfile object and
`UFFDIO_COPY`-ing it. A **page cache** (a map/sparse-file of already-fetched offsets) makes repeated
faults on the same page free and bounds the S3 traffic to the guest's *unique* touched pages. Per
page is the clearest baseline; **E2B does this at chunk granularity with a `.header` index and
compression** (§11 item 2) — if 15b's measured restore time is intolerable, a
fetch-a-larger-window-around-the-fault prefetch is added and its effect reported (the empirical path
Stages 13/14 used). The cache is per-VM and freed in `Destroy` with the handler.

### Decision 7 — the parity oracle stays behavioral (unchanged since Stage 11)
Where artifacts live and how the memfile is sourced are invisible to the wire. The Python e2e suite
(currently 37/37) is the oracle: a sandbox booted from S3, with its memfile streamed over UFFD,
must run code, keep kernel state, and expose ports exactly as before. A green suite against MinIO
proves the swap moved *storage*, not behavior.

### Decision 8 — buildID-keyed artifacts + a bucket **alias** for name→buildID resolution
E2B keys artifacts by an **immutable** `{buildID}/{file}` prefix and resolves a template/env → its
current buildID **in the database** (the api knows each env's current build, and passes it to the
orchestrator). We adopt the immutable `{buildID}/{file}` layout (faithful + it means a rebuild
writes a *new* prefix and never disturbs sandboxes still booting the old one), but resolve the
current build with a **mutable alias object** `aliases/<template-name> → buildID` that the
orchestrator reads directly. This keeps the orchestrator self-contained (no new proto field, no
store dependency) — a documented single-machine stand-in for E2B's DB-side resolution. The default
template uses the sentinel buildID `default`. (Threading the buildID api→orchestrator over the
Create RPC, exactly like E2B, is the faithful alternative, deferred to avoid a proto change this
stage.) Known limitation: the *local* cache is still keyed by the baked name-path (Decision 3), so
two concurrent builds of one name share a local rootfs path — fine for one box, gone once the rootfs
is NBD-served (§11 item 1).

## 5. Code "from → to" map

| concern | from (Stage 14) | to (Stage 15) |
| --- | --- | --- |
| storage interface | `TemplateDir(name) → local path` | blob `StorageProvider`: `Upload`/`Open`(whole)/`ReadAt`(range)/`Exists` |
| storage impls | `Local` (returns a path) | `S3` (minio-go) default; `Local` dir-as-bucket → unit-test double |
| object keys | (local paths only) | immutable `{buildID}/{file}` + mutable `aliases/<name>` (Decision 8) |
| uffd page source | `Serve(uds, memfilePath)`; `mmapFile` | `Serve(uds, PageSource)`; `MmapSource` + `bucketSource` (S3 Range + cache) |
| rootfs/snapfile at boot | read directly from the local path | **materialized** from `{buildID}/…` to the baked local path if missing |
| memfile at boot (default) | File mmap (local) | **streamed** page-by-page from `{buildID}/memfile` via UFFD |
| who writes artifacts | build writes local only | build writes local **and uploads** to `{buildID}/…` + flips the alias; fixture seeds `default/…` |
| name → artifacts | `template.Resolve(name)` (local paths) | local paths via `Resolve` (cache) **+** alias `name → buildID` (bucket source) |
| orchestrator flags | `--uffd` | + `--storage`, `--s3-endpoint/-bucket/-access-key/-secret-key`, `--cache-dir` |
| default source | local `vendor/` | **S3** (`local-fs` selectable) |
| provisioning | compose: postgres + redis | + `minio` (+ bucket-create); fixture seeds + clears local |
| deps | pgx, go-redis | + `minio-go/v7` (pure Go) |

## 6. Layout introduced this stage

```
docker-compose.yml                 # + minio service (+ a one-shot mc/createbucket init)
services/pkg/storage/
  storage.go        # CHANGED: blob StorageProvider interface + {buildID}/alias key helpers + name/path rule
  s3.go             # NEW: S3 impl over minio-go (Upload / Open→whole / ReadAt→range / Exists / alias get-set)
  local.go          # NEW: Local dir-as-bucket impl (hermetic unit-test double)
  storage_test.go   # local hermetic; s3 variant skips unless MSB_TEST_S3_ENDPOINT set
services/pkg/uffd/
  uffd.go           # CHANGED: Serve(uds, PageSource); PageSource interface; MmapSource factored out
  source_bucket.go  # NEW: bucketSource — S3 Range read per page + local page cache (//go:build linux w/ uffd)
  uffd_test.go      # + PageSource-based serve test (MmapSource still covers the mmap path)
services/pkg/build/build.go        # + upload the three artifacts to {buildID}/… and flip aliases/<name>
services/pkg/fc/fc.go              # Spawn/Restore: materialize-if-missing + build the memfile PageSource
services/cmd/orchestrator/         # --storage/--s3-* flags; construct StorageProvider; alias→buildID; thread in
scripts/dev-up.sh                  # bring up minio; seed vendor/ → bucket (default/ + alias); pass --storage/--s3-*
tests/conftest.py                  # control_plane fixture: minio up, seed + clear local, point binaries at S3
```

## 7. Three independently verifiable sub-steps

### Stage 15a — `pkg/uffd`: make the page source pluggable (refactor only) ✅
Extract `PageSource` (`ReadPageAt(p []byte, off int64) error`, `Close() error`); factor today's
mmap path into `MmapSource`; change `Serve(uds, memfilePath)` → `Serve(uds, PageSource)`; have
`fc.Restore` pass `uffd.MmapSource(memfile)`. **No behavior change** — the File path is untouched
and the local-UFFD path serves identical bytes, just through the interface. Verify: `go test
./services/...` green (uffd unit tests adjusted to the interface); the UFFD e2e (`MSB_ORCH_FLAGS=
--uffd`) still 37/37. Nothing object-storage yet; this banks the precondition cleanly, exactly as
Stage 14a/b extracted interfaces before swapping behavior.

### Stage 15b — `pkg/storage` S3 + `bucketSource`; build uploads (buildID/alias); orchestrator materializes + streams ✅
Add the `minio` compose service + the `minio-go` dep. Reshape `pkg/storage` into the blob interface
with `S3` + `Local` impls and the `{buildID}/{file}` + `aliases/<name>` key scheme. Add
`bucketSource` (S3 Range per page + cache) implementing `uffd.PageSource`. Make `pkg/build` upload
its outputs to `{buildID}/…` and flip the alias. Add the orchestrator `--storage`/`--s3-*` flags;
resolve name → buildID via the alias; thread the provider into `fc.Spawn`/`fc.Restore`: materialize
rootfs/snapfile if missing; in `s3` mode serve the memfile via `bucketSource`. `conftest`/`dev-up`
bring up minio, seed `vendor/` (default + alias), and clear the local rootfs+snapfile (Decision 4).
Verify: `go test ./services/...` green (S3 variant runs when `MSB_TEST_S3_ENDPOINT` is set, else
skips); Python e2e green against MinIO with the memfile streamed over UFFD (sandboxes boot, run
code, keep state, expose ports). **Measure** restore-to-ready vs Stage 14 and record the honest
number; decide if Decision 6's prefetch is needed.

### Stage 15c — docs, defaults, dev-up, honest review ✅
Finalize this doc's status; update `CLAUDE.md` (the "Done" list + the storage line in core
architecture + common commands), `docs/ARCHITECTURE.md` (the artifacts state-seam line), and the
roadmap (move "the storage swaps go live" fully to done; the remaining fidelity gaps — NBD rootfs,
chunk/header/compression, COW layers, NFS cache — land in the roadmap as named future work, §11).
Confirm the **warm pool** (it rides `Restore`, so it streams too) and the **template build** path
(it now uploads + aliases) behave. Run the full e2e against Postgres + Redis + MinIO (target: still
37/37) and give the 🔴/🟡/🟢 self-review.

## 8. Keeping tests green (honest trade-offs)

- **The flip is the cost, again.** A plain `pytest` now also needs MinIO for the VM cases (it
  already needed docker, KVM, firecracker, passwordless-sudo networking, and — since Stage 14 —
  postgres + redis). One more provisioned service, not a new *kind*. The fixture brings it up so the
  developer experience stays "run pytest"; a box without docker **fails loudly** for the VM group.
- **Go units stay hermetic.** `go test ./services/...` needs no server: the `S3` storage test and
  the `bucketSource` test self-skip unless `MSB_TEST_S3_ENDPOINT` points at a live MinIO; `Local`
  (dir-as-bucket) and `MmapSource` cover the logic without one. The pure parts — page-offset math,
  the materialize-if-missing decision, the alias key derivation — stay KVM-/network-free.
- **UFFD is now on the default e2e path.** Because `s3` mode streams the memfile over UFFD, the
  default real-VM e2e now exercises the Stage-13 ioctl path (which 13b already proved works on this
  WSL2 box). `local-fs` + File remains the fallback if a box can't serve faults.
- **Behavioral parity, not perf.** The stage must not change the e2e count or any observable
  behavior; 37/37 against MinIO is the proof. No latency win is claimed (and per-page Range GETs
  make restore *slower* — the header says so).
- **Safety note carried forward.** MinIO runs with throwaway creds on loopback for this single-box
  learning setup. This remains a learning implementation, **not security-audited**; the sandbox is
  inbound-reachable / outbound-denied (Stage 12), and nothing here makes it safe to expose to
  untrusted input.

## 9. New dependencies (called out, per the roadmap's discipline)

| dependency | why | cgo? |
|---|---|---|
| `github.com/minio/minio-go/v7` | the idiomatic pure-Go S3 client; `*minio.Object` is an `io.ReaderAt`, mapping onto both materialize (whole read) and the UFFD page source (range read) | pure Go |
| docker image `minio/minio` (+ `minio/mc` for one-shot bucket-create) | test/dev provisioning only — not linked into any binary | n/a |

`minio-go` is pure Go, preserving the static-binary property every host service relies on (the same
reason Stage 14 chose `pgx`/`go-redis` and `pkg/store` chose `modernc.org/sqlite`).

## 10. What this completes

Stage 15 closes the roadmap's "decompose the monolith into E2B's component architecture" arc on the
**seam** axis: all three state stores (store, catalog, artifacts) now use the same *kind* of backend
E2B does (Postgres, Redis, S3), each behind an interface, each provisioned by compose. The page
source is genuinely pluggable end to end (mmap ↔ bucket), so the precondition for sourcing snapshot
memory from a peer node — not just a bucket — is in place. What remains is **(a)** the E2B-fidelity
work itemized in §11 (NBD rootfs, chunk/header/compression, COW layers, NFS cache), and **(b)** the
roadmap's "later — production fidelity" line (auth `X-API-Key`→team, real multi-host scheduling over
the now-shared catalog/store/bucket via `placement.BestOfK`, a TypeScript SDK, per-template resource
limits / start-ready commands).

## 11. Known divergences from E2B (verified against `e2b-dev/infra` source; deferred)

This stage is faithful on the **seam** (object storage + `StorageProvider` + UFFD-streamed memfile)
and deliberately simpler on the **mechanism**. Verified against `pkg/storage` and
`pkg/storage/header` in `e2b-dev/infra`, here is exactly where we match and where we defer, so the
doc neither overclaims fidelity nor hides the gaps:

| axis | E2B (real) | this stage | status |
|---|---|---|---|
| source of truth | object storage (GCS default; **AWS/S3**; Local) | MinIO (S3) | ✅ faithful (S3 is a real E2B provider) |
| interface | `StorageProvider{OpenBlob, OpenSeekable, GetDetails, UploadSignedURL, DeleteObjectsWithPrefix}` | `Upload/Open/ReadAt/Exists` | ✅ faithful in spirit (`OpenSeekable` ≙ our range read); simplified surface |
| key layout | immutable `{buildID}/{file}` | immutable `{buildID}/{file}` + bucket alias | ✅ faithful (alias = single-machine stand-in for DB resolution, Decision 8) |
| snapfile (VM state) | fetched whole (small, no header) | materialized whole | ✅ faithful |
| memfile | lazy via UFFD from storage, **chunked + compressed + `.header` page-state index + NFS cache** | lazy via UFFD, **per-page Range + simple local cache** | 🟡 same seam, simpler mechanism — **deferred** (item 2) |
| **rootfs** | lazy, **NBD userspace block device** over the same chunked storage (+ OverlayFS/squashfs base sharing) | **materialized whole** to the baked local path | 🔴 different mechanism — **deferred** (item 1) |
| build model | layered **copy-on-write** diffs; the `.header` maps each block to the build that owns it; storage holds only deltas | one flat artifact set per build | 🟡 **deferred** (item 3) |
| cross-node cache | `WrapInNFSCache` shares chunks between orchestrators | per-VM local cache only | 🟡 single-box — **deferred** (item 4) |

**Deferred fidelity work (candidate future stages), in rough priority:**
1. **NBD-streamed rootfs.** Serve `rootfs.ext4` as a userspace NBD (or FUSE block) device backed by
   the bucket, so the rootfs streams like the memfile and the baked-path problem dissolves (the
   rootfs stops being a baked host file). This is a whole subsystem — its own stage.
2. **Chunked + compressed memfile/rootfs with a `.header` index** (E2B's `pkg/storage/header`):
   per-block `NotPresent/Dirty/Zero` state, larger-than-page chunks, LZ4/zstd frames, prefetch.
3. **Copy-on-write layered builds**: each build a diff layer over its parent; the header resolves a
   byte to the owning build; storage holds only deltas (E2B's `BuildMap`/`Mapping`).
4. **Cross-node chunk cache** (E2B's NFS wrap) — only meaningful once multi-host lands.

None of these change the Stage-15 *seam*; they deepen the *mechanism* behind the same
`StorageProvider`/`PageSource` interfaces, which is why they slot in cleanly as later stages.
