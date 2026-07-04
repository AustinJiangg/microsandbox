//go:build linux

package block

import (
	"fmt"
	"os"
	"sync"
)

// Cache is a per-VM writable layer over the read-only base: a sparse file sized to the whole device
// plus a per-block dirty bitset. Guest writes land here (the base objects, shared across VMs, are never
// mutated); a read of a block the guest has written comes from here, any other block from the base
// (the Overlay decides, using isDirty). This is E2B's block.Cache.
//
// Only whole blocks are tracked dirty. The NBD export announces BlockSize to the kernel, so guest I/O
// is block-aligned and block-multiple -- a write always covers whole blocks, so marking every touched
// block dirty never leaves a block half-written-half-sparse (which would lose the base's bytes for the
// untouched half). Sub-block writes would need read-modify-write against the base; we rely on the
// alignment invariant instead (documented, not enforced).
type Cache struct {
	f         *os.File
	size      int64 // logical device size
	numBlocks int64

	mu    sync.RWMutex
	dirty []bool // per block: has the guest written it
}

// NewCache creates (or truncates) a sparse backing file at path, sized to the device. The file is
// sparse: Truncate reserves the size without allocating blocks, so an untouched cache costs almost
// nothing and only written blocks occupy disk. The caller removes the file on VM teardown (Close only
// closes the handle).
func NewCache(path string, size int64) (*Cache, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, fmt.Errorf("block: create cache %s: %w", path, err)
	}
	if err := f.Truncate(size); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("block: size cache to %d: %w", size, err)
	}
	return &Cache{
		f:         f,
		size:      size,
		numBlocks: (size + BlockSize - 1) / BlockSize,
		dirty:     make([]bool, (size+BlockSize-1)/BlockSize),
	}, nil
}

// isDirty reports whether the block containing logical offset off has been written.
func (c *Cache) isDirty(off int64) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.dirty[off/BlockSize]
}

// ReadAt reads written bytes straight from the backing file. The Overlay only calls this for offsets it
// has confirmed dirty, so the bytes are the guest's own writes (never sparse-zero holes).
func (c *Cache) ReadAt(p []byte, off int64) (int, error) { return c.f.ReadAt(p, off) }

// WriteAt writes the guest's bytes to the backing file and marks every block the write touches dirty,
// so a later read of those blocks is served from here rather than the base. Safe for concurrent use
// (the kernel binds several NBD sockets): pwrite is atomic per call and the dirty bitset is mutex-guarded.
func (c *Cache) WriteAt(p []byte, off int64) (int, error) {
	n, err := c.f.WriteAt(p, off)
	if n > 0 {
		c.mu.Lock()
		for b := off / BlockSize; b <= (off+int64(n)-1)/BlockSize; b++ {
			c.dirty[b] = true
		}
		c.mu.Unlock()
	}
	return n, err
}

// Close closes the backing file handle (the file itself is removed by the caller on teardown).
func (c *Cache) Close() error { return c.f.Close() }

// Diff is the export of a Cache's dirtied state, the raw material for a new COW layer (Stage 20's
// producer turns it into a header mapping + an uploaded object). Dirty[b] is true for every block the
// guest wrote; Empty[b] is true for a dirty block that is all-zeros (it maps to a zero-owner run, stored
// as nothing). Data is the concatenation, in ascending block order, of exactly the dirty non-empty
// blocks -- so a run's bytes in Data sit at the physical offset a header mapping will point owners at.
type Diff struct {
	Data      []byte
	Dirty     []bool
	Empty     []bool
	BlockSize int64
}

// ExportToDiff walks the dirty blocks and emits the diff: a dirty block that is all-zeros is recorded
// Empty (not stored), any other dirty block is appended to Data. Untouched blocks are absent from both,
// meaning "inherit the base" once merged. It reads each dirty block back from the backing file, so it
// reflects the final state at pause time. E2B's cache.ExportToDiff.
func (c *Cache) ExportToDiff() (*Diff, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	d := &Diff{
		Dirty:     make([]bool, c.numBlocks),
		Empty:     make([]bool, c.numBlocks),
		BlockSize: BlockSize,
	}
	copy(d.Dirty, c.dirty)

	buf := make([]byte, BlockSize)
	for b := int64(0); b < c.numBlocks; b++ {
		if !c.dirty[b] {
			continue
		}
		off := b * BlockSize
		block := buf
		if off+BlockSize > c.size { // the last block may be short of a full BlockSize
			block = buf[:c.size-off]
		}
		if err := readAtFull(c.f, block, off); err != nil {
			return nil, fmt.Errorf("block: export read block %d: %w", b, err)
		}
		if isZero(block) {
			d.Empty[b] = true
			continue
		}
		d.Data = append(d.Data, block...)
	}
	return d, nil
}
