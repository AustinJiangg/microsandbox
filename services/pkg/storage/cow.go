package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"microsandbox/services/pkg/storage/header"
)

// layeredAssembleChunk is the copy granularity when assembling a layered rootfs from its build objects.
const layeredAssembleChunk = 1 << 20 // 1 MiB

// OpenRootfsHeader fetches and parses {buildID}/rootfs.ext4.header, returning (nil, nil) when it is
// absent -- a non-layered (whole-object) rootfs, where the caller materializes the whole object.
func OpenRootfsHeader(ctx context.Context, sp StorageProvider, buildID string) (*header.Header, error) {
	return openHeader(ctx, sp, ArtifactKey(buildID, RootfsHeaderName))
}

// PublishRootfsDiff stores childRootfsPath as a copy-on-write diff over the base build's rootfs: it
// uploads {childBuildID}/rootfs.ext4 holding only the child's changed (non-zero) blocks and
// {childBuildID}/rootfs.ext4.header carrying the flattened mapping that points unchanged ranges at the
// base's (and its ancestors') objects. The boot path reassembles the full rootfs via MaterializeLayered.
//
// It materializes the base's full rootfs to a temp file to diff against (assembling it if the base is
// itself layered), so a chain of layers composes -- the child's flattened header references whichever of
// its ancestors' objects last wrote each range. The diff is correct for ANY sizes; the build pipeline
// (Stage 18c) pins the child to the base's size so the diff stays small (Decision 8 in STAGE18_DESIGN.md).
func PublishRootfsDiff(ctx context.Context, sp StorageProvider, baseBuildID, childRootfsPath, childBuildID string) error {
	// 1) Assemble the base's full rootfs and resolve its flattened mapping/metadata.
	baseDir, err := os.MkdirTemp("", "msb-base-rootfs-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(baseDir)
	baseRootfs := filepath.Join(baseDir, "rootfs.ext4")
	if err := MaterializeLayered(ctx, sp, baseBuildID, baseRootfs); err != nil {
		return fmt.Errorf("materialize base %q: %w", baseBuildID, err)
	}
	baseInfo, err := os.Stat(baseRootfs)
	if err != nil {
		return err
	}
	baseHdr, err := OpenRootfsHeader(ctx, sp, baseBuildID)
	if err != nil {
		return err
	}
	// A non-layered base resolves to one full run owned by itself (identity storage offset, mirroring
	// header.NewHeader); a layered base contributes its already-flattened mapping and chain root.
	baseMapping := header.SingleBuildMapping(uint64(baseInfo.Size()), baseBuildID)
	baseGen, chainRoot := uint64(0), baseBuildID
	if baseHdr != nil {
		baseMapping = baseHdr.Mapping
		baseGen = baseHdr.Metadata.Generation
		chainRoot = baseHdr.Metadata.BaseBuildId
	}

	// 2) Diff the child against the assembled base: only changed (non-zero) blocks are written to the
	//    diff object; changed-to-zero blocks become zero-owner runs; unchanged blocks resolve to the base.
	childInfo, err := os.Stat(childRootfsPath)
	if err != nil {
		return err
	}
	baseF, err := os.Open(baseRootfs)
	if err != nil {
		return err
	}
	defer baseF.Close()
	childF, err := os.Open(childRootfsPath)
	if err != nil {
		return err
	}
	defer childF.Close()
	diffTmp, err := os.CreateTemp("", "msb-rootfs-diff-*")
	if err != nil {
		return err
	}
	defer os.Remove(diffTmp.Name())
	childDiff, derr := header.BuildDiff(baseF, childF, childInfo.Size(), int64(header.DefaultBlockSize), childBuildID, diffTmp)
	if cerr := diffTmp.Close(); derr == nil {
		derr = cerr
	}
	if derr != nil {
		return fmt.Errorf("diff child rootfs: %w", derr)
	}

	// 3) Flatten the child's diff onto the base, build the v2 header, upload the diff object + header.
	flat := header.NormalizeMappings(header.MergeMappings(baseMapping, childDiff))
	h := header.Header{
		Metadata: header.Metadata{
			Version:     header.VersionLayered,
			BlockSize:   header.DefaultBlockSize,
			Size:        uint64(childInfo.Size()),
			Generation:  baseGen + 1,
			BuildId:     childBuildID,
			BaseBuildId: chainRoot,
		},
		Mapping: flat,
	}
	if err := uploadLocalFile(ctx, sp, diffTmp.Name(), ArtifactKey(childBuildID, RootfsName)); err != nil {
		return fmt.Errorf("upload rootfs diff: %w", err)
	}
	hb := h.Serialize()
	if err := sp.Upload(ctx, ArtifactKey(childBuildID, RootfsHeaderName), bytes.NewReader(hb), int64(len(hb))); err != nil {
		return fmt.Errorf("upload rootfs header: %w", err)
	}
	return nil
}

// DiffBlocks is a writable rootfs overlay's exported diff (block.Cache.ExportToDiff) in a
// storage-package-local form, so pkg/storage stays free of pkg/block (which is linux/nbd-gated). Data is
// the dirty non-empty blocks concatenated in ascending block order, Dirty/Empty are per-block flags, and
// Size is the logical device size. The Stage-22 producer fills it from the overlay after the layer's
// command runs in-guest (docs/STAGE22_DESIGN.md); the orchestrator maps block.Diff into it field for field.
type DiffBlocks struct {
	Data      []byte
	Dirty     []bool
	Empty     []bool
	BlockSize int64
	Size      int64
}

// PublishRootfsDiffBlocks stores a Stage-22 layer's rootfs as a copy-on-write diff over baseBuildID from
// the writable overlay's exported dirty blocks -- the disk half of the one-run-two-diffs producer. Unlike
// PublishRootfsDiff (a base-vs-child content compare that must materialize the whole base to diff against),
// this consumes the overlay's ExportToDiff directly: it needs only the base's HEADER (for the flattened
// owner chain), never the base's bytes, so a `RUN` that touched a few blocks uploads a few blocks -- E2B's
// cache.ExportToDiff -> header path. It uploads {childBuildID}/rootfs.ext4 = d.Data (the changed non-zero
// blocks) and {childBuildID}/rootfs.ext4.header (the flattened v2 mapping pointing unchanged ranges at the
// base's objects and zeroed ranges at no owner). The boot path reassembles via MaterializeLayered, exactly
// as for a PublishRootfsDiff-produced child -- only the production differs.
func PublishRootfsDiffBlocks(ctx context.Context, sp StorageProvider, baseBuildID, childBuildID string, d DiffBlocks) error {
	// The child's own diff runs, in BuildDiff's exact form (the shared diffAccumulator), owned by the child.
	childDiff := header.MappingFromDirty(d.Dirty, d.Empty, d.BlockSize, d.Size, childBuildID)

	// Resolve the base's flattened mapping/metadata from its header alone (no base bytes needed): a
	// non-layered base is one full run owned by itself; a layered base contributes its flattened chain.
	baseHdr, err := OpenRootfsHeader(ctx, sp, baseBuildID)
	if err != nil {
		return err
	}
	baseMapping := header.SingleBuildMapping(uint64(d.Size), baseBuildID)
	baseGen, chainRoot := uint64(0), baseBuildID
	if baseHdr != nil {
		baseMapping = baseHdr.Mapping
		baseGen = baseHdr.Metadata.Generation
		chainRoot = baseHdr.Metadata.BaseBuildId
	}

	flat := header.NormalizeMappings(header.MergeMappings(baseMapping, childDiff))
	h := header.Header{
		Metadata: header.Metadata{
			Version:     header.VersionLayered,
			BlockSize:   uint64(d.BlockSize),
			Size:        uint64(d.Size),
			Generation:  baseGen + 1,
			BuildId:     childBuildID,
			BaseBuildId: chainRoot,
		},
		Mapping: flat,
	}
	if err := sp.Upload(ctx, ArtifactKey(childBuildID, RootfsName), bytes.NewReader(d.Data), int64(len(d.Data))); err != nil {
		return fmt.Errorf("upload rootfs diff blocks: %w", err)
	}
	hb := h.Serialize()
	if err := sp.Upload(ctx, ArtifactKey(childBuildID, RootfsHeaderName), bytes.NewReader(hb), int64(len(hb))); err != nil {
		return fmt.Errorf("upload rootfs header: %w", err)
	}
	return nil
}

// MaterializeLayered makes buildID's full rootfs available at the local path dst. If buildID carries a
// rootfs header the rootfs is layered: dst is assembled by copying each run from its owning build's
// object (a per-owner reader cache keeps each {owner}/rootfs.ext4 open once -- E2B's DiffStore analogue),
// with gaps and zero-owner runs left as the file's zero fill. Without a header it is a non-layered build,
// downloaded whole (the Stage-15 path). dst already present is a cache hit (the baked path is the cache).
// The write is atomic (temp + rename) so a concurrent boot never sees a partial rootfs.
func MaterializeLayered(ctx context.Context, sp StorageProvider, buildID, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return nil // cache hit: the baked local path already holds this rootfs (same rule as Materialize)
	}
	h, err := OpenRootfsHeader(ctx, sp, buildID)
	if err != nil {
		return err
	}
	if h == nil {
		return Materialize(ctx, sp, ArtifactKey(buildID, RootfsName), dst) // non-layered: whole-object download
	}
	return assembleMapping(ctx, sp, h.Mapping, int64(h.Metadata.Size), dst, RootfsName)
}

// assembleMapping writes the runs of mapping into a fresh file at dst, sized to size: it truncates to
// the full logical size (so gaps and zero-owner runs cost nothing -- served by the file's own zero
// fill) and copies each present run from its owning build's fileName object. The write is atomic (temp +
// rename), so a concurrent reader never sees a partial file. Shared by MaterializeLayered (fileName =
// rootfs.ext4) and MaterializeMemfileFull (fileName = memfile) -- Stage 20 keeps the two symmetric.
func assembleMapping(ctx context.Context, sp StorageProvider, mapping header.Mapping, size int64, dst, fileName string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	// A UNIQUE temp file per call (not a fixed dst+".tmp"), so concurrent materializations of the same dst
	// don't clobber each other's temp and fail the rename (same race as downloadObject).
	f, err := os.CreateTemp(filepath.Dir(dst), filepath.Base(dst)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	// Size the file to the full logical size up front; gaps and zero-owner runs are then served by the
	// file's own zero fill, so only present runs cost a read + write.
	if err := f.Truncate(size); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := assembleRuns(ctx, sp, mapping, f, fileName); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst) // atomic publish at the baked path
}

// assembleRuns copies each present run of mapping into f at its logical offset, reading from the run's
// owning build's fileName object (rootfs.ext4 or memfile). A per-owner reader cache opens each
// {owner}/{fileName} once. A zero-owner ("") run is skipped -- the truncated file is already zero there.
func assembleRuns(ctx context.Context, sp StorageProvider, mapping header.Mapping, f *os.File, fileName string) error {
	cache := map[string]RangeReader{}
	defer func() {
		for _, r := range cache {
			r.Close()
		}
	}()
	readerFor := func(owner string) (RangeReader, error) {
		if r, ok := cache[owner]; ok {
			return r, nil
		}
		r, err := sp.OpenReaderAt(ctx, ArtifactKey(owner, fileName))
		if err != nil {
			return nil, fmt.Errorf("open owner %q %s: %w", owner, fileName, err)
		}
		cache[owner] = r
		return r, nil
	}
	buf := make([]byte, layeredAssembleChunk)
	for _, run := range mapping {
		if run.Owner == "" {
			continue // zeros: already present as the file's zero fill
		}
		r, err := readerFor(run.Owner)
		if err != nil {
			return err
		}
		if err := copyRange(r, f, int64(run.BuildStorageOffset), int64(run.Offset), int64(run.Length), buf); err != nil {
			return fmt.Errorf("assemble run @logical %d from %q: %w", run.Offset, run.Owner, err)
		}
	}
	return nil
}

// copyRange copies length bytes from src@srcOff to dst@dstOff in buf-sized chunks. A short read at the
// owner object's end (io.EOF) stops the copy, leaving the remainder as dst's zero fill -- tolerant of an
// owner object shorter than the logical size (the size-mismatch case Decision 8 lets the build pipeline
// avoid). A no-progress read with no error is a stuck source, reported rather than looped forever.
func copyRange(src io.ReaderAt, dst io.WriterAt, srcOff, dstOff, length int64, buf []byte) error {
	for length > 0 {
		n := int64(len(buf))
		if n > length {
			n = length
		}
		got, rerr := src.ReadAt(buf[:n], srcOff)
		if got > 0 {
			if _, werr := dst.WriteAt(buf[:got], dstOff); werr != nil {
				return werr
			}
			length -= int64(got)
			srcOff += int64(got)
			dstOff += int64(got)
		}
		if rerr == io.EOF {
			return nil // owner object ended early -> the tail stays zero
		}
		if rerr != nil {
			return rerr
		}
		if got == 0 {
			return io.ErrUnexpectedEOF // no progress and no error: a stuck source
		}
	}
	return nil
}

// --- Stage 20: memfile copy-on-write -------------------------------------------------------------
//
// The memfile (guest RAM) mirrors the rootfs COW above, differing only in transport (the boot path
// serves it lazily over UFFD via uffd.NewLayeredSource, never assembling it whole) and in production
// (a live-VM re-snapshot, not a docker+debugfs delta -- see docs/STAGE20_DESIGN.md). The algebra is
// identical: BuildDiff classifies each block vs the base (changed-non-zero -> child-owned, changed-to-
// zero -> zero-owner override, unchanged -> resolves to base), MergeMappings flattens onto the base,
// and a v2 header records the flattened owners. MaterializeMemfileFull is the diff-time expander (not a
// boot path): it exists only so a child's full memfile can be diffed against the base's full memfile.

// memfileOwnedMapping resolves a parsed memfile header's runs to owned runs for COW assembly/merge. A
// v1 (Stage 17) memfile header lists only present runs with NO owner -- they live in the build's own
// compacted {buildID}/memfile object, so we stamp buildID as their owner (its gaps stay uncovered and
// read as zeros). A v2 (layered, Stage 20) header already carries per-run owners across the chain
// (including "" zero-owner runs), so it is used as-is. This is the one place the memfile diverges from
// the rootfs, which has no v1 (unowned) header form.
func memfileOwnedMapping(h *header.Header, buildID string) header.Mapping {
	if h.Metadata.Version >= header.VersionLayered {
		return h.Mapping
	}
	out := make(header.Mapping, len(h.Mapping))
	for i, m := range h.Mapping {
		m.Owner = buildID
		out[i] = m
	}
	return out
}

// MaterializeMemfileFull writes buildID's FULL (uncompacted) memfile to dst, expanding whatever it is
// stored as: a Stage-17 compacted single-build memfile (v1 header -- present runs read from
// {buildID}/memfile, gaps zero), a COW-layered memfile (v2 header -- each run read from its owning
// build's object, zero-owner runs and gaps zero), or a pre-Stage-17 raw memfile (no header -- copied
// whole). It is the memfile analogue of MaterializeLayered, but the memfile is never *booted* from a
// whole file -- this exists only so PublishMemfileDiff can diff a child's full memfile against the
// base's full memfile at build time. dst is always (re)written (no cache-hit skip): the caller passes a
// fresh temp path.
func MaterializeMemfileFull(ctx context.Context, sp StorageProvider, buildID, dst string) error {
	h, err := OpenMemfileHeader(ctx, sp, buildID)
	if err != nil {
		return err
	}
	if h == nil {
		return downloadObject(ctx, sp, ArtifactKey(buildID, MemfileName), dst) // pre-Stage-17 raw memfile
	}
	return assembleMapping(ctx, sp, memfileOwnedMapping(h, buildID), int64(h.Metadata.Size), dst, MemfileName)
}

// PublishMemfileDiff stores childMemfilePath as a copy-on-write diff over the base build's memfile (the
// memfile analogue of PublishRootfsDiff, Stage 20). It uploads {childBuildID}/memfile holding only the
// child's changed (non-zero) RAM blocks and {childBuildID}/memfile.header carrying the flattened v2
// mapping that points unchanged ranges at the base's (and its ancestors') objects. The boot path serves
// it lazily over UFFD via the multi-owner page source (uffd.NewLayeredSource, Stage 20a).
//
// It materializes the base's full memfile to a temp file to diff against (expanding it if the base is
// itself compacted or layered), so a chain of layers composes -- the child's flattened header references
// whichever of its ancestors' objects last wrote each range. The child memfile MUST be the SAME logical
// size as the base: a re-snapshot of the base VM keeps mem_size_mib fixed, so it is (a mismatch is a
// build-config error, reported rather than silently diffing to ~everything).
func PublishMemfileDiff(ctx context.Context, sp StorageProvider, baseBuildID, childMemfilePath, childBuildID string) error {
	// 1) Materialize the base's full memfile and resolve its flattened mapping/metadata.
	baseDir, err := os.MkdirTemp("", "msb-base-memfile-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(baseDir)
	baseMemfile := filepath.Join(baseDir, "memfile")
	if err := MaterializeMemfileFull(ctx, sp, baseBuildID, baseMemfile); err != nil {
		return fmt.Errorf("materialize base memfile %q: %w", baseBuildID, err)
	}
	baseInfo, err := os.Stat(baseMemfile)
	if err != nil {
		return err
	}
	childInfo, err := os.Stat(childMemfilePath)
	if err != nil {
		return err
	}
	if baseInfo.Size() != childInfo.Size() {
		return fmt.Errorf("memfile COW needs equal sizes: base %q is %d bytes, child %q is %d"+
			" (the re-snapshot must keep mem_size_mib fixed)", baseBuildID, baseInfo.Size(), childBuildID, childInfo.Size())
	}
	baseHdr, err := OpenMemfileHeader(ctx, sp, baseBuildID)
	if err != nil {
		return err
	}
	// A no-header base resolves to one full run owned by itself (a raw whole memfile); a v1 base's
	// present runs are owned by itself (its compacted object), gen 0 / chain root = itself; a v2 base
	// contributes its already-flattened mapping and chain root.
	baseMapping := header.SingleBuildMapping(uint64(baseInfo.Size()), baseBuildID)
	baseGen, chainRoot := uint64(0), baseBuildID
	if baseHdr != nil {
		baseMapping = memfileOwnedMapping(baseHdr, baseBuildID)
		if baseHdr.Metadata.Version >= header.VersionLayered {
			baseGen = baseHdr.Metadata.Generation
			chainRoot = baseHdr.Metadata.BaseBuildId
		}
	}

	// 2) Diff the child against the assembled base: only changed (non-zero) blocks are written to the
	//    diff object; changed-to-zero blocks become zero-owner runs; unchanged blocks resolve to the base.
	baseF, err := os.Open(baseMemfile)
	if err != nil {
		return err
	}
	defer baseF.Close()
	childF, err := os.Open(childMemfilePath)
	if err != nil {
		return err
	}
	defer childF.Close()
	diffTmp, err := os.CreateTemp("", "msb-memfile-diff-*")
	if err != nil {
		return err
	}
	defer os.Remove(diffTmp.Name())
	childDiff, derr := header.BuildDiff(baseF, childF, childInfo.Size(), int64(header.DefaultBlockSize), childBuildID, diffTmp)
	if cerr := diffTmp.Close(); derr == nil {
		derr = cerr
	}
	if derr != nil {
		return fmt.Errorf("diff child memfile: %w", derr)
	}

	// 3) Flatten the child's diff onto the base, build the v2 header, upload the diff object + header.
	flat := header.NormalizeMappings(header.MergeMappings(baseMapping, childDiff))
	h := header.Header{
		Metadata: header.Metadata{
			Version:     header.VersionLayered,
			BlockSize:   header.DefaultBlockSize,
			Size:        uint64(childInfo.Size()),
			Generation:  baseGen + 1,
			BuildId:     childBuildID,
			BaseBuildId: chainRoot,
		},
		Mapping: flat,
	}
	if err := uploadLocalFile(ctx, sp, diffTmp.Name(), ArtifactKey(childBuildID, MemfileName)); err != nil {
		return fmt.Errorf("upload memfile diff: %w", err)
	}
	hb := h.Serialize()
	if err := sp.Upload(ctx, ArtifactKey(childBuildID, HeaderName), bytes.NewReader(hb), int64(len(hb))); err != nil {
		return fmt.Errorf("upload memfile header: %w", err)
	}
	return nil
}

// RootfsPathName records the absolute host rootfs path a build's snapshot bakes into its drive config
// (Stage 20). A layered child's vmstate is a re-snapshot of its BASE, so Firecracker bakes the base
// template's rootfs path, not the child's; the restore reads this to bind the child's NBD device over
// exactly the path FC will open. Absent for non-layered builds (the restore binds over the template's
// own path).
const RootfsPathName = "rootfs.path"

// PublishSnapfile uploads a local Firecracker vmstate file as {buildID}/snapfile (E2B's name for the VM
// device/CPU state). The Stage-20 live-VM producer uses it for a layered child's re-snapshotted vmstate;
// the non-layered path uploads the snapfile inline in pkg/build.
func PublishSnapfile(ctx context.Context, sp StorageProvider, vmstatePath, buildID string) error {
	return uploadLocalFile(ctx, sp, vmstatePath, ArtifactKey(buildID, SnapfileName))
}

// PublishRootfsBakedPath records path (the absolute host rootfs path buildID's snapshot bakes) as
// {buildID}/rootfs.path, so a restore can bind the NBD device over it (Stage 20). See RootfsPathName.
func PublishRootfsBakedPath(ctx context.Context, sp StorageProvider, buildID, path string) error {
	return sp.Upload(ctx, ArtifactKey(buildID, RootfsPathName), bytes.NewReader([]byte(path)), int64(len(path)))
}

// OpenRootfsBakedPath returns the baked rootfs path recorded for buildID, or "" if none (a non-layered
// build or a pre-Stage-20 bucket -- the restore then binds over the template's own tmpl.Rootfs).
func OpenRootfsBakedPath(ctx context.Context, sp StorageProvider, buildID string) (string, error) {
	key := ArtifactKey(buildID, RootfsPathName)
	ok, err := sp.Exists(ctx, key)
	if err != nil || !ok {
		return "", err
	}
	rc, err := sp.Open(ctx, key)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
