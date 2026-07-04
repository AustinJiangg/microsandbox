//go:build linux

// Package block is the copy-on-write block stack behind an NBD-served rootfs (Stage 21). It is the
// disk-side twin of the memfile's COW read path: a read-only base that resolves each logical offset
// through the pkg/storage/header COW mapping to the owning build's chunked bucket object, plus a
// per-VM writable Overlay so guest disk writes land in a private sparse cache and the shared base
// objects are never mutated. See docs/STAGE21_DESIGN.md (E2B's pkg/block: Overlay + Cache).
//
// The Overlay satisfies nbd.Provider structurally (ReadAt/WriteAt/Size), so pkg/nbd binds it to a
// /dev/nbdX without either package importing the other; they meet only where the orchestrator wires
// them (Stage 21c). This package stays storage-free the same way pkg/uffd does -- the owner->object
// resolution arrives as an injected uffd.OpenFunc, so nothing here imports pkg/storage or minio.
//
// Everything is testable without the nbd module or KVM: the Overlay/Cache run over temp files and an
// in-memory base, exercised in overlay_test.go and cache_test.go.
package block

import (
	"io"

	"microsandbox/services/pkg/uffd"
)

// BlockSize is the COW granularity: 4 KiB, matching the NBD logical block size (nbd.nbdBlockSize) and
// the pkg/storage/header block size, so a kernel block request, a cache dirty bit, and a header run all
// align. The kernel issues block-aligned, block-multiple I/O once the export announces this size, which
// is the invariant the Cache relies on to track dirtiness per whole block (see cache.go).
const BlockSize = 4096

// ReadSource is the Overlay's read-only base: a sized, random-access byte source that the Overlay falls
// through to for any block the guest has not written. It is closed with the Overlay. uffd's layered COW
// source supplies the ReadAt half (NewLayeredBase adds Size).
type ReadSource interface {
	io.ReaderAt
	Size() int64
	io.Closer
}

// layeredBase adapts the Stage-20a multi-owner page source (uffd.NewLayeredSource) into a ReadSource by
// pairing it with the logical device size. The layered source already does exactly what an NBD base
// read needs -- resolve a logical offset to its owning build and range-read that build's compacted
// object through a chunk cache, serving gaps/zero-owner runs as zeros with no fetch -- just addressed
// by a block read here instead of a page fault.
type layeredBase struct {
	src  uffd.PageSource
	size int64
}

// NewLayeredBase builds the read-only COW base for an NBD rootfs. extents is the rootfs header's v2
// mapping converted to storage-free runs (each run's Owner = the build whose {owner}/rootfs object
// holds it, "" = zeros); size is the logical rootfs size; chunkSize is each owner's range-read
// granularity (0 = uffd.DefaultChunkSize); open resolves an owner to its object (the orchestrator wires
// it to object storage in Stage 21c). A single-build (non-layered) rootfs is just the case where every
// run shares one owner.
func NewLayeredBase(extents []uffd.Extent, size, chunkSize int64, open uffd.OpenFunc) ReadSource {
	return &layeredBase{src: uffd.NewLayeredSource(extents, size, chunkSize, open), size: size}
}

func (b *layeredBase) ReadAt(p []byte, off int64) (int, error) { return b.src.ReadAt(p, off) }
func (b *layeredBase) Size() int64                             { return b.size }
func (b *layeredBase) Close() error                            { return b.src.Close() }

// readAtFull fills p from r starting at off, looping because an io.ReaderAt may return fewer bytes per
// call than requested (the chunked base returns at most one chunk). A final io.EOF that still filled p
// is success; a no-progress read is an unexpected short source.
func readAtFull(r io.ReaderAt, p []byte, off int64) error {
	for n := 0; n < len(p); {
		m, err := r.ReadAt(p[n:], off+int64(n))
		n += m
		if n >= len(p) {
			return nil
		}
		if m == 0 {
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			return err
		}
		if err != nil && err != io.EOF {
			return err
		}
	}
	return nil
}

// isZero reports whether p is all zero bytes -- the test for an "empty" dirty block in ExportToDiff (a
// block the guest wrote all-zeros to maps to a zero-owner run, stored as nothing, not as 4 KiB of zeros).
func isZero(p []byte) bool {
	for _, b := range p {
		if b != 0 {
			return false
		}
	}
	return true
}
