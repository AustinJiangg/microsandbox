//go:build linux

package uffd

import (
	"fmt"
	"io"
	"sort"
)

// Extent maps one present (non-zero) logical run of the memfile to its bytes in the compacted object.
// It is the storage-free form of pkg/storage/header.BuildMap -- plain ints, so pkg/uffd consumes the
// memfile index without importing pkg/storage/minio (the orchestrator converts the parsed header's
// mapping to []Extent). See docs/STAGE17_DESIGN.md, Decision 4.
type Extent struct {
	Logical  int64 // logical byte offset in the (uncompacted) memfile
	Length   int64 // run length in bytes
	Physical int64 // byte offset of this run inside Owner's compacted object
	// Owner is the build that owns this run ("" = zero-owner, served as zeros). Stage 20: ignored by the
	// single-build mappedSource (which reads its one object); set + used by the layered source (COW memfile).
	Owner string
}

// mappedSource is the Stage 17 PageSource: a chunked reader over a COMPACTED memfile (only the present
// blocks are stored), plus an extent map that remaps each logical offset to its physical offset. A
// fault in a gap (no extent covers it) is served as zeros with NO read -- the win over Stage 15's
// chunkedSource, which fetched (and stored) the snapshot's vast zero regions too. Present runs still
// come through the chunk cache, so a touched island is fetched in big range reads, not page-at-a-time.
type mappedSource struct {
	phys    PageSource // chunked cache over the compacted object, addressed by PHYSICAL offset
	extents []Extent   // present runs, sorted ascending by Logical, non-overlapping (from header.Build)
	size    int64      // total LOGICAL memfile size; [last extent end, size) and any gap read as zeros
}

// NewMappedSource builds a PageSource over the compacted object ra (closed via closer) using extents
// (the converted memfile header) and the logical size. chunkSize is the physical range-read granularity
// (0 = DefaultChunkSize). extents must be sorted by Logical and non-overlapping, as header.Build emits.
func NewMappedSource(ra io.ReaderAt, closer func() error, extents []Extent, size, chunkSize int64) PageSource {
	return &mappedSource{phys: NewChunkedSource(ra, closer, chunkSize), extents: extents, size: size}
}

// ReadAt fills p at logical offset off, walking the extents/gaps it overlaps: a present segment is read
// from the compacted object at the remapped physical offset; a gap segment is zero-filled with no read.
// With blockSize == page size (Decision 3) a single fault is wholly present or wholly gap, but the loop
// also handles a buffer that spans a run boundary (e.g. a hugepage fault crossing present <-> zero).
func (m *mappedSource) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= m.size {
		return 0, io.EOF
	}
	end := off + int64(len(p))
	short := false
	if end > m.size { // a page reaching past the memfile end -- fill what exists, then signal EOF
		end = m.size
		short = true
	}
	filled := 0
	for cur := off; cur < end; {
		phys, segLimit, present := m.locate(cur)
		if segLimit > end {
			segLimit = end
		}
		seg := p[filled : filled+int(segLimit-cur)]
		if present {
			if err := readPhysFull(m.phys, seg, phys); err != nil {
				return filled, fmt.Errorf("read compacted memfile at phys %d (logical %d): %w", phys, cur, err)
			}
		} else {
			clear(seg) // gap -> zeros, no fetch
		}
		filled += len(seg)
		cur = segLimit
	}
	if short {
		return filled, io.EOF // readPage turns a short page into an error, as the mmap/chunked sources do
	}
	return filled, nil
}

// locate classifies the logical offset cur. If an extent covers it: present, returning the physical
// offset for cur and the logical end of that run. Otherwise a gap, returning the logical offset where
// the gap ends (the next extent's start, or the memfile size). Binary search over the sorted extents.
func (m *mappedSource) locate(cur int64) (phys, segLimit int64, present bool) {
	i := sort.Search(len(m.extents), func(i int) bool { return m.extents[i].Logical > cur }) - 1
	if i >= 0 {
		e := m.extents[i]
		if cur < e.Logical+e.Length {
			return e.Physical + (cur - e.Logical), e.Logical + e.Length, true
		}
	}
	next := m.size
	if j := i + 1; j < len(m.extents) {
		next = m.extents[j].Logical
	}
	return 0, next, false
}

// Close releases the underlying chunked physical source (the compacted object reader + its cache).
func (m *mappedSource) Close() error { return m.phys.Close() }

// readPhysFull fills seg from the chunked physical source starting at off, looping because the chunked
// source returns at most one chunk per call (so a segment spanning a physical chunk boundary -- only a
// hugepage with a large run -- takes several reads). A real end-of-object before seg is full is an
// error; a no-progress read is too (a guard against an infinite loop).
func readPhysFull(phys PageSource, seg []byte, off int64) error {
	for o := 0; o < len(seg); {
		n, err := phys.ReadAt(seg[o:], off+int64(o))
		o += n
		if o >= len(seg) {
			return nil
		}
		if n == 0 { // no progress: a genuine short object (EOF) or a stuck source
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			return err
		}
		if err != nil && err != io.EOF { // io.EOF with progress just means a chunk boundary -- keep going
			return err
		}
	}
	return nil
}
