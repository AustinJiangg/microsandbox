# Stage 19 design: the rootfs COW payoff — layout-preserving layered builds

> Status: **design (proposed).** A direct follow-on to Stage 18, which banked E2B's COW *mechanism* (the
> per-entry build owner, `MergeMappings`, the multi-build assemble) but measured a **bounded** size win: a real
> `derived` build (default + one `RUN` layer) stored a **278.8 MiB** diff over its 576 MiB base — only ~2.07×,
> not the ~40× the lab table predicted. Stage 18d root-caused that honestly (`docs/STAGE18_DESIGN.md` status
> block + Decision 8 amendment): `docker build … RUN` then `docker export | mkfs.ext4 -d` **re-creates the
> filesystem from scratch**, and adding a layer reshuffles the ext4 block layout, so ~half the *content* blocks
> move — spurious relocations, not real delta. This stage closes that gap so the COW machinery actually pays off.
>
> Read `docs/STAGE18_DESIGN.md` (the COW mechanism this completes) and `docs/STAGE17_DESIGN.md` (the header)
> first. This stage changes **only how a layered child's rootfs image is produced** — the header format, the
> merge algebra, `PublishRootfsDiff`, `MaterializeLayered`, the boot path, the API/SDK are all **unchanged**.

## 1. The measured root cause and the fix (the heart of the stage)

E2B's rootfs diff is small because a layer is the **same block device mutated in place**: it copies the parent's
disk, writes only the changed files, and the unchanged files keep their exact block offsets — so a block compare
is the genuine delta. Our Stage-18 pipeline instead **re-creates** the filesystem each build (`mkfs.ext4 -d` over
a fresh `docker export`), and `mkfs` allocates blocks by walking the extracted tree; adding a `RUN` layer perturbs
the extraction/allocation order enough that ~half the shared files land at new offsets. The size-pin (Stage 18
Decision 8) fixed *size* divergence but not *layout* divergence.

**Measured on this box (the decision basis, mirroring how Stage 18 Decision 8 was set):**

| how the child rootfs is produced | changed 4 KiB blocks vs base | ~size |
|---|---|---|
| re-`mkfs.ext4 -d` from a fresh export, **+1 `RUN` layer** (Stage 18 today) | **71,364 / 147,456** | ~278 MiB |
| re-`mkfs.ext4 -d` of the **same image** (no extra layer) — mkfs *is* deterministic | ~4,375 | ~17 MiB |
| **copy the base image, write the added file in place** (`debugfs`, no re-mkfs) | **6** | ~24 KiB |

The fix is therefore: **a layered build does not re-mkfs.** It **copies the base template's rootfs image** and
**applies only the child's filesystem delta into that copy in place**, so every unchanged file keeps its block —
the single-machine analogue of E2B's in-place block-device layer. `header.BuildDiff` then stores ~the genuine
delta (tens of KiB–few MiB), not ~half the disk.

## 2. The mechanism: in-place delta application (chosen: `debugfs`; alternative: loop-mount + rsync)

Applying the delta into an ext4 image needs one of two mechanisms. Both were considered; the chosen one is
measured-validated and keeps `build-rootfs.sh`'s standing **"without root"** property.

**Chosen — `debugfs` (e2fsprogs, already a dependency via `mkfs.ext4`; unprivileged, no mount/loop/root).**
`debugfs -w` edits an ext4 image file directly: `write <src> <path>` adds/overwrites a file, `rm <path>` deletes,
`mkdir`/`symlink`/`ln` cover the rest, and `sif` (set_inode_field) fixes mode/uid/gid/mtime. Validated above:
writing one file changed **6 blocks** and the file read back correctly; `debugfs write` **preserves the source
file's mode** (0755 confirmed) and defaults owner to root:root (the common case for `RUN`-installed files; `sif`
patches the rest). A batch command file (`debugfs -w -f cmds img`) applies the whole delta in one pass.

**Alternative — loop-mount the base copy + `rsync -aHAX --delete staging/ mnt/` (full metadata fidelity, but
needs root + a loop device).** The orchestrator runs as root since Stage 12, so this is viable in production, and
`rsync` handles perms/uid/gid/symlinks/xattrs natively. **Rejected as the primary** because (a) it breaks the
unprivileged-build property for no measured fidelity need (debugfs preserved mode in test), and (b) a fresh
rw mount+unmount rewrites the journal/superblock (a fixed ~journal-size churn) on every build, adding diff that
debugfs avoids (debugfs never mounts, so no journal replay). Kept documented as the fallback if a delta ever
needs metadata fidelity debugfs can't express.

## 3. Where the delta comes from

The child's filesystem delta over its base is exactly what its Docker `RUN` layers changed. `docker diff` of a
container created from the child image emits it as `A <path>` (added), `C <path>` (changed), `D <path>` (deleted),
relative to the image's `FROM` base. Mapping to a `debugfs` command file:

- `A`/`C` regular file → `rm <path>` (if `C`) then `write <staging><path> <path>`; `sif` mode/uid/gid from the staged file's stat.
- `A` directory → `mkdir <path>` (+ `sif` mode).
- `A`/`C` symlink → `symlink <target> <path>`.
- `D` → `rm <path>` (or `rmdir`).

**Constraint (Decision 3):** the delta is meaningful only if the child image's `FROM` is the **base template's
image**, so `docker diff`'s "changes vs `FROM`" equals "changes vs the base rootfs". For `base="default"` the
recipe is `FROM microsandbox-agent` (default's own source), which holds. We **document and validate** this
coupling rather than auto-rewrite the recipe's `FROM` (a later refinement); a mismatch is still *correct* (it just
falls back to a larger diff — the Stage-18 mechanism tolerates any delta) but loses the size win.

## 4. Target architecture (what moves)

```
  Stage 18 (today) — layered child:                Stage 19 — layout-preserving layered child:
    docker build (FROM base-img + RUN)               docker build (FROM base-img + RUN)
    build-rootfs.sh, size-pinned to base             materialize base template's rootfs -> copy to OUT  (OUT == base, 0 diff)
      = docker export | mkfs.ext4 -d  (FRESH FS)     docker diff <child container>  -> A/C/D delta
    PublishRootfsDiff(base, child)                   apply delta into OUT in place via debugfs (write/rm/mkdir/sif)
      -> diff ~= half the content (layout moved)     PublishRootfsDiff(base, OUT, child)
                                                       -> diff ~= the genuine delta (layout preserved)

  header / merge / MaterializeLayered / boot / API / SDK  ── UNCHANGED
```

| Component | Change |
|---|---|
| `scripts/` | a layered rootfs build path: copy the base image, `docker diff` the child, apply the delta via `debugfs` (a new `build-rootfs-layered.sh`, or a `--base <img>` mode of `build-rootfs.sh`). The daemon/`init` already live in the base copy, so they are not re-injected (no spurious ~20 MiB daemon rewrite). |
| `services/pkg/build` | a `base`-set build materializes the base rootfs, runs the **layered** builder against it (not the re-mkfs path), then `PublishRootfsDiff` as today. The Stage-18 **size-pin is subsumed** (the copy is byte-for-byte the base's size) and retired for layered builds. |
| `services/pkg/storage` | **none** — `PublishRootfsDiff`/`MaterializeLayered`/header all unchanged; they just receive a layout-aligned child and so store a small diff. |
| orchestrator / api / SDK | **none** — `from`/`base`, the boot assemble, the wire are all unchanged. |
| deps | **none** — `debugfs` ships with `e2fsprogs` (already required for `mkfs.ext4`). |

## 5. Key design decisions

### Decision 1 — mutate a copy of the base image in place; never re-mkfs a layered child
The validated core (table in §1): re-mkfs moves ~half the blocks on any layer change; copy-and-mutate moves only
the delta's blocks. So a layered build copies the base rootfs and edits it. This is the single-machine analogue of
E2B's in-place block-device layer and is what makes the Stage-18 COW header actually pay off.

### Decision 2 — `debugfs`, not loop-mount, as the in-place editor (keep builds unprivileged)
Measured: `debugfs` preserves mode, needs no mount/loop/root, and adds no journal churn (§2). It keeps
`build-rootfs.sh`'s stated "entirely without root" property for layered builds too. Loop-mount + rsync stays the
documented fallback for any future metadata fidelity gap.

### Decision 3 — the child recipe must `FROM` the base template's image (documented coupling, not auto-rewrite)
`docker diff` gives changes-vs-`FROM`; that equals changes-vs-base only when `FROM` is the base. We validate this
in the e2e (`base="default"`, `FROM microsandbox-agent`) and document it; a mismatch degrades the size win but not
correctness (the merge algebra tolerates any delta). Auto-deriving `FROM` from `base` is a later refinement.

### Decision 4 — the parity oracle stays behavioral; the win is now a real, asserted number
Stage 18's e2e proved a layered template boots and carries content. This stage re-runs that and **additionally
asserts the stored diff is materially smaller** — the COW win Stage 18 could only report out-of-band. Because the
e2e has no S3 client, the size assertion is taken via the orchestrator/build measuring the published diff size and
logging it (or a Go probe in the e2e harness); the behavioral assertions (package imports, code runs) stay.

## 6. Independently verifiable sub-steps (KVM-free first, the house discipline)

### Stage 19a — the layout-preserving layered rootfs builder (no pkg/build/VM wiring)
Add the layered build path in `scripts/` (copy base image → `docker diff` child → `debugfs` apply). Validate it
**standalone**, KVM-free, with a synthetic fixture: build a tiny base ext4 image, a child = base + a known
file/dir/symlink (+ a deletion), run the new path, and assert (a) the child contains the expected files with the
right modes (via `debugfs stat`/`cat`), and (b) the block diff vs the base is small (the genuine delta, not the
whole content). Nothing in `pkg/build`/orchestrator changes.

### Stage 19b — wire `pkg/build` to use it for layered builds
A `base`-set `Build` materializes the base rootfs and runs the **layered** builder against it instead of the
re-mkfs + size-pin path; `PublishRootfsDiff` unchanged. Retire/short-circuit the Stage-18 size-pin for layered
builds (the copy guarantees the size). Unit-test the command sequence (the injectable executor asserts the layered
path is taken for `base != ""`, the whole-upload path for `base == ""`). `go test ./services/...` green.

### Stage 19c — real-VM e2e + measured win + docs + honest review
Re-run `test_layered_template_via_api` (build `derived` over `default`, boot, content carried, code runs) and add
the **measured size assertion** (the stored `derived` rootfs diff is now a small fraction of the base, vs Stage
18's 48%). Report the real bytes. Update `docs/STAGE18_DESIGN.md` (the gap is now closed), this doc's status, the
roadmap (the "layout preservation" divergence → done), CLAUDE.md, ARCHITECTURE.md. Full e2e re-run; 🟢 review.

## 7. Keeping tests green (honest trade-offs)

- **No new provisioning / no new dependency.** `debugfs` ships with `e2fsprogs` (already required). `docker diff`
  uses the docker the build already needs. A plain `pytest` needs exactly what Stage 18 needed.
- **The 19a fixture is KVM-free** (small ext4 images, `debugfs`, block-count math) — the 17a/18a discipline of
  banking the mechanism under unit-level tests before any VM path.
- **The win is concrete and asserted**, not reported out-of-band like Stage 18 — the headline deliverable is
  turning the 48% diff into the genuine delta, with the measured number in the e2e + docs.
- **Behavioral parity is preserved** (the boot/content/code assertions are unchanged); a layered rootfs assembled
  from a *smaller* diff must still boot and run identically.
- **Honest residual:** `debugfs` defaults non-root file ownership to root and needs `sif` for the rare non-root
  file; symlinks/special files are handled explicitly. If a real recipe ever needs metadata `debugfs` can't
  express, the loop-mount + rsync fallback (Decision 2) is the escape hatch. The size win also depends on the
  recipe `FROM`-ing the base (Decision 3).

## 8. New dependencies

**None.** `debugfs` is part of `e2fsprogs` (the package that provides `mkfs.ext4`, already required to build any
rootfs). No new Go module, no new service.

## 9. What this completes

Stage 18 banked the COW *header + algebra + assemble*; Stage 19 makes it **pay off** by producing the layered
child the way E2B does — mutating a copy of the base in place rather than re-creating the filesystem — so a derived
template's stored rootfs is ~its genuine delta. With this, the rootfs COW story is complete end to end (store the
delta, assemble at boot, boot a real VM). The remaining storage-depth items are unchanged: **NBD-streamed rootfs**
(serve this same layered header lazily instead of assembling whole), **memfile COW** (now Stage 20 — it needs
live-VM re-snapshotting; a build-time block compare is meaningless for RAM), and a **cross-node chunk cache**.

## 10. Known divergences from E2B (verified against `e2b-dev/infra`)

| axis | E2B (real) | this stage | status |
|---|---|---|---|
| layer production | mutate a **persisted block device** in place (overlay/loop) | copy the base image + **`debugfs` in-place edit** | 🟢 same effect (unprivileged, no mount) |
| delta source | runtime/overlay diff of the layer | `docker diff` of the child container | 🟡 analogue (build-time, Decision 3) |
| metadata fidelity | full (block device / rsync) | `debugfs write` (mode preserved) + `sif`; loop-mount fallback | 🟡 covers the common case; fallback documented |
| `FROM` ↔ base coupling | resolved automatically | documented + e2e-validated, not auto-rewritten | 🟡 refinement deferred |
| memfile layering | rootfs **and** memfile | rootfs only (memfile = Stage 20) | 🔴 deferred (RAM diff needs live-VM re-snapshot) |
