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
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	// Size the file to the full logical size up front; gaps and zero-owner runs are then served by the
	// file's own zero fill, so only present runs cost a read + write.
	if err := f.Truncate(int64(h.Metadata.Size)); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := assembleRuns(ctx, sp, h.Mapping, f); err != nil {
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
// owning build's rootfs object. A per-owner reader cache opens each {owner}/rootfs.ext4 once. A
// zero-owner ("") run is skipped -- the truncated file is already zero there.
func assembleRuns(ctx context.Context, sp StorageProvider, mapping header.Mapping, f *os.File) error {
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
		r, err := sp.OpenReaderAt(ctx, ArtifactKey(owner, RootfsName))
		if err != nil {
			return nil, fmt.Errorf("open owner %q rootfs: %w", owner, err)
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
