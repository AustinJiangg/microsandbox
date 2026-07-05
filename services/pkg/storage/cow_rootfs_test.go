package storage

// KVM-/network-free unit test for the Stage 22 rootfs copy-on-write producer input
// (PublishRootfsDiffBlocks), over the Local (dir-as-bucket) provider. It is the disk twin of
// cow_memfile_test.go's PublishMemfileDiff round trip, but fed the overlay's exported dirty blocks
// (block.Cache.ExportToDiff, here hand-built as DiffBlocks) instead of a content compare. Reuses
// cowImg/cowReadFile/cowReadObject from cow_test.go (same package).

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"

	"microsandbox/services/pkg/storage/header"
)

// TestPublishRootfsDiffBlocks: a non-layered base + an overlay diff that wrote one block to data and one
// to zeros stores only the changed non-zero block, carries a v2 header owned over the base, and
// reassembles (MaterializeLayered) to the exact child rootfs -- base block untouched, written block from
// the child object, zeroed block as zero fill. This exercises the whole Option-B path (the shared
// diffAccumulator via MappingFromDirty, MergeMappings onto the base header, the diff object upload).
func TestPublishRootfsDiffBlocks(t *testing.T) {
	ctx := context.Background()
	sp := NewLocal(t.TempDir())
	bs := int64(header.DefaultBlockSize)

	// Base A: a non-layered whole-object rootfs (like the seeded default), 3 blocks 1/2/3.
	base := cowImg(1, 2, 3)
	if err := sp.Upload(ctx, ArtifactKey("A", RootfsName), bytes.NewReader(base), int64(len(base))); err != nil {
		t.Fatalf("upload base rootfs: %v", err)
	}

	// The overlay reports: block1 written to 9s (dirty, non-empty), block2 written to zeros (dirty, empty).
	// Data holds only the one dirty non-empty block, in ascending block order.
	d := DiffBlocks{
		Data:      cowImg(9),
		Dirty:     []bool{false, true, true},
		Empty:     []bool{false, false, true},
		BlockSize: bs,
		Size:      int64(len(base)),
	}
	if err := PublishRootfsDiffBlocks(ctx, sp, "A", "B", d); err != nil {
		t.Fatalf("PublishRootfsDiffBlocks: %v", err)
	}

	// Only the one changed non-zero block is stored; the zeroed block costs nothing.
	if got := cowReadObject(t, sp, ArtifactKey("B", RootfsName)); len(got) != int(bs) {
		t.Fatalf("stored diff = %d bytes, want one block (%d)", len(got), bs)
	}
	h, err := OpenRootfsHeader(ctx, sp, "B")
	if err != nil || h == nil {
		t.Fatalf("OpenRootfsHeader B: err=%v nil=%v", err, h == nil)
	}
	if h.Metadata.Version != header.VersionLayered || h.Metadata.BuildId != "B" || h.Metadata.BaseBuildId != "A" || h.Metadata.Generation != 1 {
		t.Fatalf("layered metadata = %+v", h.Metadata)
	}
	// The flattened mapping reads the unchanged block from A and the changed block from B (multi-owner).
	owners := map[string]bool{}
	for _, m := range h.Mapping {
		owners[m.Owner] = true
	}
	if !owners["A"] || !owners["B"] {
		t.Fatalf("B mapping owners = %v, want both A and B", owners)
	}

	// Reassemble the child's full rootfs: base block0 (1s), child block1 (9s), zeroed block2 (0s).
	dst := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := MaterializeLayered(ctx, sp, "B", dst); err != nil {
		t.Fatalf("MaterializeLayered B: %v", err)
	}
	if got, want := cowReadFile(t, dst), cowImg(1, 9, 0); !bytes.Equal(got, want) {
		t.Fatalf("assembled child rootfs != expected (base+overlay-diff)")
	}
}
