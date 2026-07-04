//go:build linux

package block

import (
	"bytes"
	"io"
	"path/filepath"
	"testing"

	"microsandbox/services/pkg/uffd"
)

// memBase is an in-memory ReadSource: the KVM-free, storage-free stand-in for the layered rootfs base,
// so the Overlay's cache-first-then-base dispatch can be tested without object storage.
type memBase struct{ data []byte }

func (m *memBase) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(m.data)) {
		return 0, io.EOF
	}
	n := copy(p, m.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
func (m *memBase) Size() int64  { return int64(len(m.data)) }
func (m *memBase) Close() error { return nil }

func newOverlay(t *testing.T, base ReadSource) *Overlay {
	t.Helper()
	c, err := NewCache(filepath.Join(t.TempDir(), "cache"), base.Size())
	if err != nil {
		t.Fatalf("NewCache: %v", err)
	}
	o, err := NewOverlay(base, c)
	if err != nil {
		t.Fatalf("NewOverlay: %v", err)
	}
	t.Cleanup(func() { _ = o.Close() }) // closes both base and cache
	return o
}

func TestOverlayUnwrittenBlockFallsThroughToBase(t *testing.T) {
	// A base with a distinct byte per block; nothing written, so every read comes from the base.
	base := &memBase{data: make([]byte, 3*BlockSize)}
	for b := 0; b < 3; b++ {
		for i := 0; i < BlockSize; i++ {
			base.data[b*BlockSize+i] = byte(b + 1)
		}
	}
	o := newOverlay(t, base)

	got := make([]byte, 3*BlockSize)
	if _, err := o.ReadAt(got, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got, base.data) {
		t.Fatal("overlay did not fall through to the base for unwritten blocks")
	}
}

func TestOverlayWriteReadBackLeavesBaseUntouched(t *testing.T) {
	base := &memBase{data: bytes.Repeat([]byte{0x11}, 3*BlockSize)}
	baseCopy := append([]byte(nil), base.data...)
	o := newOverlay(t, base)

	// Write block 1 through the overlay.
	written := bytes.Repeat([]byte{0x22}, BlockSize)
	if _, err := o.WriteAt(written, BlockSize); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	// Read all three blocks: block 1 is the write, blocks 0 and 2 still the base.
	got := make([]byte, 3*BlockSize)
	if _, err := o.ReadAt(got, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got[0:BlockSize], base.data[0:BlockSize]) {
		t.Fatal("block 0 changed; expected the base")
	}
	if !bytes.Equal(got[BlockSize:2*BlockSize], written) {
		t.Fatal("block 1 did not return the written bytes")
	}
	if !bytes.Equal(got[2*BlockSize:], base.data[2*BlockSize:]) {
		t.Fatal("block 2 changed; expected the base")
	}
	// The shared base object must be untouched (writes are cache-only).
	if !bytes.Equal(base.data, baseCopy) {
		t.Fatal("overlay write mutated the base")
	}
}

// TestOverlayReadSpanningWrittenAndUnwritten reads one buffer that crosses a written block and an
// unwritten one, exercising the per-block dispatch inside a single ReadAt.
func TestOverlayReadSpanningWrittenAndUnwritten(t *testing.T) {
	base := &memBase{data: bytes.Repeat([]byte{0x11}, 2*BlockSize)}
	o := newOverlay(t, base)

	written := bytes.Repeat([]byte{0x22}, BlockSize)
	if _, err := o.WriteAt(written, 0); err != nil { // write block 0 only
		t.Fatalf("WriteAt: %v", err)
	}
	got := make([]byte, 2*BlockSize)
	if _, err := o.ReadAt(got, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got[:BlockSize], written) {
		t.Fatal("block 0 (written) not from cache")
	}
	if !bytes.Equal(got[BlockSize:], base.data[BlockSize:]) {
		t.Fatal("block 1 (unwritten) not from base")
	}
}

// TestLayeredBaseAssemblesAcrossOwners builds the real COW base (block.NewLayeredBase over
// uffd.NewLayeredSource) with two owning "builds" backed by in-memory objects plus a zero-owner gap,
// and reads it through an Overlay -- proving the base resolves each run to its owner and serves gaps as
// zeros, the disk-side of the Stage-20a layered read.
func TestLayeredBaseAssemblesAcrossOwners(t *testing.T) {
	blockA := bytes.Repeat([]byte{0xaa}, BlockSize) // owner "A", logical block 0
	blockB := bytes.Repeat([]byte{0xbb}, BlockSize) // owner "B", logical block 1
	objects := map[string][]byte{"A": blockA, "B": blockB}
	open := func(owner string) (io.ReaderAt, func() error, error) {
		return bytes.NewReader(objects[owner]), func() error { return nil }, nil
	}
	// Logical layout: block 0 <- A@0, block 1 <- B@0, block 2 <- gap (zeros, no owner).
	size := int64(3 * BlockSize)
	extents := []uffd.Extent{
		{Logical: 0, Length: BlockSize, Physical: 0, Owner: "A"},
		{Logical: BlockSize, Length: BlockSize, Physical: 0, Owner: "B"},
	}
	base := NewLayeredBase(extents, size, 0, open)
	o := newOverlay(t, base)

	got := make([]byte, size)
	if _, err := o.ReadAt(got, 0); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if !bytes.Equal(got[0:BlockSize], blockA) {
		t.Fatal("block 0 != owner A's bytes")
	}
	if !bytes.Equal(got[BlockSize:2*BlockSize], blockB) {
		t.Fatal("block 1 != owner B's bytes")
	}
	if !bytes.Equal(got[2*BlockSize:], make([]byte, BlockSize)) {
		t.Fatal("block 2 (gap) != zeros")
	}
}
