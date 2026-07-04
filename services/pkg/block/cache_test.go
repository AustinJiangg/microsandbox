//go:build linux

package block

import (
	"bytes"
	"path/filepath"
	"testing"
)

func newTestCache(t *testing.T, size int64) *Cache {
	t.Helper()
	c, err := NewCache(filepath.Join(t.TempDir(), "cache"), size)
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestCacheWriteReadBackAndDirty(t *testing.T) {
	c := newTestCache(t, 4*BlockSize)

	if c.isDirty(0) {
		t.Fatal("block 0 dirty before any write")
	}
	want := bytes.Repeat([]byte{0x5a}, BlockSize)
	if _, err := c.WriteAt(want, BlockSize); err != nil { // write block 1
		t.Fatalf("WriteAt: %v", err)
	}
	if !c.isDirty(BlockSize) {
		t.Fatal("block 1 not dirty after write")
	}
	if c.isDirty(0) || c.isDirty(2*BlockSize) {
		t.Fatal("an untouched block reads dirty")
	}
	got := make([]byte, BlockSize)
	if _, err := c.ReadAt(got, BlockSize); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("cache read did not return the written bytes")
	}
}

func TestExportToDiffYieldsDirtiedBlocks(t *testing.T) {
	c := newTestCache(t, 4*BlockSize)

	nonZero := bytes.Repeat([]byte{0xab}, BlockSize)
	if _, err := c.WriteAt(nonZero, BlockSize); err != nil { // block 1: non-empty
		t.Fatalf("WriteAt block 1: %v", err)
	}
	if _, err := c.WriteAt(make([]byte, BlockSize), 3*BlockSize); err != nil { // block 3: all-zero
		t.Fatalf("WriteAt block 3: %v", err)
	}

	d, err := c.ExportToDiff()
	if err != nil {
		t.Fatalf("ExportToDiff: %v", err)
	}
	// Exactly blocks 1 and 3 dirty; block 3 (all-zero) is empty, block 1 is not.
	wantDirty := []bool{false, true, false, true}
	wantEmpty := []bool{false, false, false, true}
	for b := range wantDirty {
		if d.Dirty[b] != wantDirty[b] || d.Empty[b] != wantEmpty[b] {
			t.Fatalf("block %d: Dirty=%v Empty=%v, want Dirty=%v Empty=%v", b, d.Dirty[b], d.Empty[b], wantDirty[b], wantEmpty[b])
		}
	}
	// Data holds only the non-empty dirty block (block 1); the empty block 3 stores nothing.
	if !bytes.Equal(d.Data, nonZero) {
		t.Fatalf("Data = %d bytes, want the single non-empty block (%d bytes)", len(d.Data), len(nonZero))
	}
}

// TestExportToDiffShortLastBlock covers a device size that is not a whole number of blocks: the final
// block is shorter than BlockSize, and the diff must store exactly those bytes.
func TestExportToDiffShortLastBlock(t *testing.T) {
	const size = BlockSize + 904 // two blocks, the second short
	c := newTestCache(t, size)

	tail := bytes.Repeat([]byte{0x7}, 904)
	if _, err := c.WriteAt(tail, BlockSize); err != nil {
		t.Fatalf("WriteAt short block: %v", err)
	}
	d, err := c.ExportToDiff()
	if err != nil {
		t.Fatalf("ExportToDiff: %v", err)
	}
	if d.Dirty[0] || !d.Dirty[1] {
		t.Fatalf("Dirty = %v, want only block 1", d.Dirty)
	}
	if !bytes.Equal(d.Data, tail) {
		t.Fatalf("Data = %d bytes, want the 904-byte short block", len(d.Data))
	}
}
