package storage

// KVM-/network-free unit tests for the Stage 20 memfile copy-on-write mechanism, over the Local
// (dir-as-bucket) provider. The memfile's base is a Stage-17 v1 compacted object (published via
// PublishMemfile, the real `default` case) -- so these also cover the v1-header-has-no-owner path
// (memfileOwnedMapping) that the rootfs COW never hits. Reuses cowImg/cowWriteTemp/cowReadFile/
// cowReadObject from cow_test.go (same package).

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"microsandbox/services/pkg/storage/header"
)

// memPublishV1 publishes b as a Stage-17 compacted single-build memfile ({buildID}/memfile +
// {buildID}/memfile.header, v1) -- exactly how the seeded default template's memfile is stored.
func memPublishV1(t *testing.T, sp StorageProvider, buildID string, b []byte) {
	t.Helper()
	if err := PublishMemfile(context.Background(), sp, cowWriteTemp(t, b), buildID); err != nil {
		t.Fatalf("PublishMemfile %q: %v", buildID, err)
	}
}

// TestPublishAndMaterializeMemfileDiff: a v1 base + a child changing one block to data and zeroing
// another diffs to only the changed non-zero block, carries a v2 header owned over the base, resolves
// unchanged blocks back to the base's compacted object, and expands to the exact child memfile.
func TestPublishAndMaterializeMemfileDiff(t *testing.T) {
	ctx := context.Background()
	sp := NewLocal(t.TempDir())
	base := cowImg(1, 2, 0, 4, 5, 0, 7, 8)   // zero blocks 2,5 -> the base is compacted (gaps)
	child := cowImg(1, 2, 0, 99, 5, 0, 7, 0) // block3 4->99 (data), block7 8->0 (zeroed)

	memPublishV1(t, sp, "A", base)
	if err := PublishMemfileDiff(ctx, sp, "A", cowWriteTemp(t, child), "B"); err != nil {
		t.Fatalf("PublishMemfileDiff: %v", err)
	}

	// The stored diff holds exactly the one changed non-zero block; the zeroed block stores nothing.
	if diff := cowReadObject(t, sp, ArtifactKey("B", MemfileName)); len(diff) != int(header.DefaultBlockSize) {
		t.Fatalf("diff object = %d bytes, want one block (%d)", len(diff), header.DefaultBlockSize)
	}
	h, err := OpenMemfileHeader(ctx, sp, "B")
	if err != nil || h == nil {
		t.Fatalf("OpenMemfileHeader: err=%v nil=%v", err, h == nil)
	}
	if h.Metadata.Version != header.VersionLayered || h.Metadata.BuildId != "B" || h.Metadata.BaseBuildId != "A" || h.Metadata.Generation != 1 {
		t.Fatalf("layered metadata = %+v", h.Metadata)
	}
	// The flattened mapping reads unchanged blocks from A and the changed block from B (multi-owner).
	owners := map[string]bool{}
	for _, m := range h.Mapping {
		owners[m.Owner] = true
	}
	if !owners["A"] || !owners["B"] {
		t.Fatalf("B mapping owners = %v, want both A and B", owners)
	}

	dst := filepath.Join(t.TempDir(), "memfile")
	if err := MaterializeMemfileFull(ctx, sp, "B", dst); err != nil {
		t.Fatalf("MaterializeMemfileFull B: %v", err)
	}
	if got := cowReadFile(t, dst); !bytes.Equal(got, child) {
		t.Fatalf("expanded B != child")
	}
}

// TestMemfileDiffChain: A (v1) -> B (diff) -> C (diff over B) expands C exactly, reading from all three
// build objects, and C's chain metadata is correct (generation 2, base A).
func TestMemfileDiffChain(t *testing.T) {
	ctx := context.Background()
	sp := NewLocal(t.TempDir())
	a := cowImg(1, 2, 3, 4, 5, 6, 7, 8)
	b := cowImg(1, 2, 30, 4, 5, 6, 7, 8)  // change block2 vs A
	c := cowImg(1, 2, 30, 4, 5, 6, 70, 8) // change block6 vs B

	memPublishV1(t, sp, "A", a)
	if err := PublishMemfileDiff(ctx, sp, "A", cowWriteTemp(t, b), "B"); err != nil {
		t.Fatalf("publish B: %v", err)
	}
	if err := PublishMemfileDiff(ctx, sp, "B", cowWriteTemp(t, c), "C"); err != nil {
		t.Fatalf("publish C: %v", err)
	}

	dst := filepath.Join(t.TempDir(), "memfile")
	if err := MaterializeMemfileFull(ctx, sp, "C", dst); err != nil {
		t.Fatalf("MaterializeMemfileFull C: %v", err)
	}
	if got := cowReadFile(t, dst); !bytes.Equal(got, c) {
		t.Fatalf("expanded C != c")
	}

	h, err := OpenMemfileHeader(ctx, sp, "C")
	if err != nil {
		t.Fatal(err)
	}
	owners := map[string]bool{}
	for _, m := range h.Mapping {
		owners[m.Owner] = true
	}
	for _, want := range []string{"A", "B", "C"} {
		if !owners[want] {
			t.Fatalf("C mapping missing owner %q (multi-build read): %+v", want, h.Mapping)
		}
	}
	if h.Metadata.Generation != 2 || h.Metadata.BaseBuildId != "A" {
		t.Fatalf("C metadata = %+v, want generation 2 / base A", h.Metadata)
	}
}

// TestPublishMemfileDiffIdentical: a child equal to the base stores a zero-byte diff (the maximal COW
// win -- the child shares all the base's RAM) and still expands back to the base.
func TestPublishMemfileDiffIdentical(t *testing.T) {
	ctx := context.Background()
	sp := NewLocal(t.TempDir())
	a := cowImg(1, 2, 0, 4)
	memPublishV1(t, sp, "A", a)
	if err := PublishMemfileDiff(ctx, sp, "A", cowWriteTemp(t, a), "B"); err != nil {
		t.Fatalf("PublishMemfileDiff: %v", err)
	}
	if diff := cowReadObject(t, sp, ArtifactKey("B", MemfileName)); len(diff) != 0 {
		t.Fatalf("identical child: diff object = %d bytes, want 0", len(diff))
	}
	dst := filepath.Join(t.TempDir(), "memfile")
	if err := MaterializeMemfileFull(ctx, sp, "B", dst); err != nil {
		t.Fatalf("MaterializeMemfileFull: %v", err)
	}
	if got := cowReadFile(t, dst); !bytes.Equal(got, a) {
		t.Fatalf("expanded identical B != a")
	}
}

// TestMaterializeMemfileFullExpandsV1: a v1 compacted memfile (present blocks only) expands to the full
// memfile with its zero gaps restored -- the diff-time expander over a Stage-17 object.
func TestMaterializeMemfileFullExpandsV1(t *testing.T) {
	ctx := context.Background()
	sp := NewLocal(t.TempDir())
	a := cowImg(0, 2, 3, 0, 0, 6) // leading/interior/none-trailing zeros
	memPublishV1(t, sp, "A", a)
	dst := filepath.Join(t.TempDir(), "memfile")
	if err := MaterializeMemfileFull(ctx, sp, "A", dst); err != nil {
		t.Fatalf("MaterializeMemfileFull v1: %v", err)
	}
	if got := cowReadFile(t, dst); !bytes.Equal(got, a) {
		t.Fatalf("expanded v1 != a")
	}
}

// TestMaterializeMemfileFullNoHeader: a pre-Stage-17 raw memfile (no header) expands via a whole-object
// download (the backward-compatible fallback).
func TestMaterializeMemfileFullNoHeader(t *testing.T) {
	ctx := context.Background()
	sp := NewLocal(t.TempDir())
	raw := cowImg(1, 0, 3)
	if err := sp.Upload(ctx, ArtifactKey("A", MemfileName), bytes.NewReader(raw), int64(len(raw))); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "memfile")
	if err := MaterializeMemfileFull(ctx, sp, "A", dst); err != nil {
		t.Fatalf("MaterializeMemfileFull (no header): %v", err)
	}
	if got := cowReadFile(t, dst); !bytes.Equal(got, raw) {
		t.Fatalf("whole-object fallback != raw")
	}
}

// TestPublishMemfileDiffSizeMismatch: a child memfile of a different size than the base is a clear
// build-config error, not a silent diff-to-everything (mem_size_mib must be fixed across a re-snapshot).
func TestPublishMemfileDiffSizeMismatch(t *testing.T) {
	ctx := context.Background()
	sp := NewLocal(t.TempDir())
	memPublishV1(t, sp, "A", cowImg(1, 2, 3, 4))
	err := PublishMemfileDiff(ctx, sp, "A", cowWriteTemp(t, cowImg(1, 2, 3)), "B")
	if err == nil || !strings.Contains(err.Error(), "equal sizes") {
		t.Fatalf("size mismatch: err = %v, want an 'equal sizes' error", err)
	}
	// Nothing should have been published for the failed child.
	if ok, _ := sp.Exists(ctx, ArtifactKey("B", MemfileName)); ok {
		t.Fatalf("failed diff left a memfile object for B")
	}
}
