//go:build linux

package block

import (
	"fmt"
	"io"
)

// Overlay is the writable COW block device the orchestrator binds to /dev/nbdX: a read-only base
// (the layered rootfs streamed from object storage) under a per-VM writable Cache. It satisfies
// nbd.Provider structurally (ReadAt/WriteAt/Size), so nbd.Bind drives it. E2B's block.Overlay:
// read cache-first-then-base, write cache-only.
type Overlay struct {
	base  ReadSource
	cache *Cache
}

// NewOverlay layers cache over base. The two must describe the same device size; a mismatch is a
// wiring bug (the Cache is sized from the base's logical size in Stage 21c).
func NewOverlay(base ReadSource, cache *Cache) (*Overlay, error) {
	if base.Size() != cache.size {
		return nil, fmt.Errorf("block: overlay base size %d != cache size %d", base.Size(), cache.size)
	}
	return &Overlay{base: base, cache: cache}, nil
}

// Size is the logical device size reported to the NBD kernel client.
func (o *Overlay) Size() int64 { return o.base.Size() }

// ReadAt fills p at off block by block: a block the guest has written comes from the Cache, every other
// block from the base. Iterating per block lets one request span written and unwritten regions (the
// kernel's block-aligned I/O means each iteration stays within one block, so a single isDirty decides it).
func (o *Overlay) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= o.base.Size() {
		return 0, io.EOF
	}
	end := off + int64(len(p))
	if end > o.base.Size() {
		end = o.base.Size()
	}
	for cur := off; cur < end; {
		blkEnd := (cur/BlockSize + 1) * BlockSize
		if blkEnd > end {
			blkEnd = end
		}
		seg := p[cur-off : blkEnd-off]
		src := io.ReaderAt(o.base)
		if o.cache.isDirty(cur) {
			src = o.cache
		}
		if err := readAtFull(src, seg, cur); err != nil {
			return int(cur - off), err
		}
		cur = blkEnd
	}
	return int(end - off), nil
}

// WriteAt lands the guest's write in the Cache only; the shared base objects are never mutated. The
// dirtied blocks become the VM's rootfs diff on export (ExportToDiff), the Stage-20 producer's input.
func (o *Overlay) WriteAt(p []byte, off int64) (int, error) { return o.cache.WriteAt(p, off) }

// ExportToDiff returns the blocks the guest wrote over this VM's life (for the Stage-20 layer producer).
func (o *Overlay) ExportToDiff() (*Diff, error) { return o.cache.ExportToDiff() }

// Close releases the base (its owner readers + chunk caches) and the cache file handle.
func (o *Overlay) Close() error {
	berr := o.base.Close()
	cerr := o.cache.Close()
	if berr != nil {
		return berr
	}
	return cerr
}
