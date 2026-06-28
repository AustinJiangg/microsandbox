package storage

// KVM-/network-free unit tests for the Stage 18 rootfs copy-on-write mechanism, over the Local
// (dir-as-bucket) provider: a base uploaded whole + children published as diffs, then assembled back.

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"microsandbox/services/pkg/storage/header"
)

// cowImg builds a rootfs image of DefaultBlockSize-sized blocks, block i filled with vals[i] (a 0 value
// yields a zero block, so a "zeroed" change is checkable).
func cowImg(vals ...byte) []byte {
	bs := int(header.DefaultBlockSize)
	out := make([]byte, 0, bs*len(vals))
	for _, v := range vals {
		blk := make([]byte, bs)
		for i := range blk {
			blk[i] = v
		}
		out = append(out, blk...)
	}
	return out
}

func cowWriteTemp(t *testing.T, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "img.ext4")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func cowReadFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func cowReadObject(t *testing.T, sp StorageProvider, key string) []byte {
	t.Helper()
	rc, err := sp.Open(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func cowUploadWhole(t *testing.T, sp StorageProvider, buildID string, b []byte) {
	t.Helper()
	if err := sp.Upload(context.Background(), ArtifactKey(buildID, RootfsName), bytes.NewReader(b), int64(len(b))); err != nil {
		t.Fatal(err)
	}
}

// TestPublishAndMaterializeRootfsDiff: a child that changes one block to data and zeroes another diffs to
// only the changed non-zero block (the zeroed block costs nothing), carries a v2 header owned over the
// base, and assembles back to the exact child.
func TestPublishAndMaterializeRootfsDiff(t *testing.T) {
	ctx := context.Background()
	sp := NewLocal(t.TempDir())
	base := cowImg(1, 2, 3, 4, 5, 6, 7, 8)
	child := cowImg(1, 2, 99, 4, 0, 6, 7, 8) // block2 3->99 (data), block4 5->0 (zeroed)

	cowUploadWhole(t, sp, "A", base) // a non-layered base, like the seeded default (no header)
	if err := PublishRootfsDiff(ctx, sp, "A", cowWriteTemp(t, child), "B"); err != nil {
		t.Fatalf("PublishRootfsDiff: %v", err)
	}

	// The stored diff holds exactly the one changed non-zero block; the zeroed block stores nothing.
	if diff := cowReadObject(t, sp, ArtifactKey("B", RootfsName)); len(diff) != int(header.DefaultBlockSize) {
		t.Fatalf("diff object = %d bytes, want one block (%d)", len(diff), header.DefaultBlockSize)
	}
	h, err := OpenRootfsHeader(ctx, sp, "B")
	if err != nil || h == nil {
		t.Fatalf("OpenRootfsHeader: err=%v nil=%v", err, h == nil)
	}
	if h.Metadata.Version != header.VersionLayered || h.Metadata.BuildId != "B" || h.Metadata.BaseBuildId != "A" || h.Metadata.Generation != 1 {
		t.Fatalf("layered metadata = %+v", h.Metadata)
	}

	dst := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := MaterializeLayered(ctx, sp, "B", dst); err != nil {
		t.Fatalf("MaterializeLayered: %v", err)
	}
	if got := cowReadFile(t, dst); !bytes.Equal(got, child) {
		t.Fatalf("assembled B != child")
	}
}

// TestMaterializeLayeredChain: A (whole) -> B (diff) -> C (diff over B) assembles C exactly, reading from
// all three build objects (the multi-build read), and C's chain metadata is correct.
func TestMaterializeLayeredChain(t *testing.T) {
	ctx := context.Background()
	sp := NewLocal(t.TempDir())
	a := cowImg(1, 2, 3, 4, 5, 6, 7, 8)
	b := cowImg(1, 2, 30, 4, 5, 6, 7, 8)  // change block2 vs A
	c := cowImg(1, 2, 30, 4, 5, 6, 70, 8) // change block6 vs B

	cowUploadWhole(t, sp, "A", a)
	if err := PublishRootfsDiff(ctx, sp, "A", cowWriteTemp(t, b), "B"); err != nil {
		t.Fatalf("publish B: %v", err)
	}
	if err := PublishRootfsDiff(ctx, sp, "B", cowWriteTemp(t, c), "C"); err != nil {
		t.Fatalf("publish C: %v", err)
	}

	dst := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := MaterializeLayered(ctx, sp, "C", dst); err != nil {
		t.Fatalf("MaterializeLayered C: %v", err)
	}
	if got := cowReadFile(t, dst); !bytes.Equal(got, c) {
		t.Fatalf("assembled C != c")
	}

	h, err := OpenRootfsHeader(ctx, sp, "C")
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

// TestMaterializeLayeredNoHeaderFallback: a non-layered build (no rootfs header) downloads whole.
func TestMaterializeLayeredNoHeaderFallback(t *testing.T) {
	ctx := context.Background()
	sp := NewLocal(t.TempDir())
	a := cowImg(1, 2, 3)
	cowUploadWhole(t, sp, "A", a)
	dst := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := MaterializeLayered(ctx, sp, "A", dst); err != nil {
		t.Fatalf("MaterializeLayered (no header): %v", err)
	}
	if got := cowReadFile(t, dst); !bytes.Equal(got, a) {
		t.Fatalf("whole-object fallback != a")
	}
}

// TestPublishRootfsDiffIdentical: a child equal to the base stores a zero-byte diff and still assembles
// back to the base (its flattened mapping is entirely base-owned).
func TestPublishRootfsDiffIdentical(t *testing.T) {
	ctx := context.Background()
	sp := NewLocal(t.TempDir())
	a := cowImg(1, 2, 3, 4)
	cowUploadWhole(t, sp, "A", a)
	if err := PublishRootfsDiff(ctx, sp, "A", cowWriteTemp(t, a), "B"); err != nil {
		t.Fatalf("PublishRootfsDiff: %v", err)
	}
	if diff := cowReadObject(t, sp, ArtifactKey("B", RootfsName)); len(diff) != 0 {
		t.Fatalf("identical child: diff object = %d bytes, want 0", len(diff))
	}
	dst := filepath.Join(t.TempDir(), "rootfs.ext4")
	if err := MaterializeLayered(ctx, sp, "B", dst); err != nil {
		t.Fatalf("MaterializeLayered: %v", err)
	}
	if got := cowReadFile(t, dst); !bytes.Equal(got, a) {
		t.Fatalf("assembled identical B != a")
	}
}
