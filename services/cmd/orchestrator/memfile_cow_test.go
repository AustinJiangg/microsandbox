package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"microsandbox/services/pkg/storage"
	"microsandbox/services/pkg/storage/header"
)

// KVM-free end-to-end test of the Stage 20 memfile read path: it stitches the producer side
// (storage.PublishMemfile v1 base + storage.PublishMemfileDiff v2 child) to the boot side
// (server.layeredMemSource -> uffd multi-owner source) over a Local (dir-as-bucket) provider, so the
// exact seam a real-VM restore would be the first to exercise -- "the v2 header the producer wrote is
// consumed correctly, reading unchanged pages from the base and changed pages from the child" -- is
// covered without a VM. It complements pkg/storage/cow_memfile_test.go (the algebra) and
// pkg/uffd/source_layered_test.go (the source in isolation).

// memBlocks builds a memfile of one DefaultBlockSize block per value, each block filled with that byte
// (0 => an all-zero block, which compaction drops). This mirrors how memfile COW scans in fixed blocks.
func memBlocks(vals ...byte) []byte {
	bs := int(header.DefaultBlockSize)
	out := make([]byte, len(vals)*bs)
	for i, v := range vals {
		if v != 0 {
			for j := 0; j < bs; j++ {
				out[i*bs+j] = v
			}
		}
	}
	return out
}

func writeTemp(t *testing.T, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "memfile")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestLayeredMemSourceReadsBackChild: publish a v1 base (A) and a COW diff child (B) that changes one
// block and zeroes another, then read the WHOLE memfile back through server.layeredMemSource and assert
// it reconstructs the child exactly -- unchanged blocks fetched from A's object, the changed block from
// B's, the zeroed block served as zeros with no owner.
func TestLayeredMemSourceReadsBackChild(t *testing.T) {
	ctx := context.Background()
	sp := storage.NewLocal(t.TempDir())
	base := memBlocks(1, 2, 0, 4, 5, 0, 7, 8)   // A: zero blocks 2,5 -> compacted (gaps)
	child := memBlocks(1, 2, 0, 99, 5, 0, 7, 0) // B: block3 4->99 (data), block7 8->0 (zeroed)

	if err := storage.PublishMemfile(ctx, sp, writeTemp(t, base), "A"); err != nil {
		t.Fatalf("PublishMemfile A: %v", err)
	}
	if err := storage.PublishMemfileDiff(ctx, sp, "A", writeTemp(t, child), "B"); err != nil {
		t.Fatalf("PublishMemfileDiff B: %v", err)
	}

	hdr, err := storage.OpenMemfileHeader(ctx, sp, "B")
	if err != nil || hdr == nil {
		t.Fatalf("OpenMemfileHeader B: err=%v nil=%v", err, hdr == nil)
	}
	if hdr.Metadata.Version < header.VersionLayered {
		t.Fatalf("B header version = %d, want >= %d (layered)", hdr.Metadata.Version, header.VersionLayered)
	}

	s := &server{storage: sp}
	src := s.layeredMemSource(hdr)
	defer src.Close()

	got := make([]byte, len(child))
	if _, err := src.ReadAt(got, 0); err != nil {
		t.Fatalf("ReadAt whole memfile: %v", err)
	}
	if !bytes.Equal(got, child) {
		t.Fatalf("layered read != child memfile")
	}

	// Page-at-a-time faults (how UFFD actually drives it) must reconstruct the child too, including the
	// zero-owner block (block 7) served without any fetch.
	bs := int64(header.DefaultBlockSize)
	for off := int64(0); off < int64(len(child)); off += bs {
		page := make([]byte, bs)
		if _, err := src.ReadAt(page, off); err != nil {
			t.Fatalf("ReadAt page @%d: %v", off, err)
		}
		if !bytes.Equal(page, child[off:off+bs]) {
			t.Fatalf("page @%d mismatch", off)
		}
	}
}
