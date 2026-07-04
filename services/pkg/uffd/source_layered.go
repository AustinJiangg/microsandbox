//go:build linux

package uffd

import (
	"fmt"
	"io"
	"sort"
	"sync"
)

// OpenFunc opens the compacted memfile object owned by build `owner`, returning a random-access reader
// and a closer. The layered source calls it lazily -- once per distinct owner it actually faults into --
// so pkg/uffd stays free of any storage/minio import: the orchestrator supplies the opener, wiring each
// owner to {owner}/memfile in object storage. A "" (zero) owner is never opened; it is served as zeros.
type OpenFunc func(owner string) (io.ReaderAt, func() error, error)

// layeredSource is the Stage 20 PageSource for a COW-layered memfile: each extent carries an Owner (the
// build whose compacted object holds that run), so a faulting page is resolved to its owning build and
// range-read from THAT build's object -- E2B's build.File multi-build read (fault -> owning build -> that
// build's diff), served lazily over UFFD. It generalizes mappedSource: a single-build memfile is just the
// case where every present run shares one owner. A zero-owner ("") run OR an uncovered gap is served as
// zeros with NO fetch -- the COW zero-override and the Stage 17 gap elision, unified.
//
// Each owner's object is opened once (lazily, on the first fault into it) and wrapped in a chunkedSource,
// so the many faults within one owner's chunk are served from that owner's cache -- E2B's DiffStore keyed
// by build. pkg/uffd stays storage-free: the owner->object mapping arrives as an injected OpenFunc.
type layeredSource struct {
	extents   []Extent // sorted ascending by Logical, non-overlapping; Owner=="" means zeros
	size      int64    // total LOGICAL memfile size; [last run end, size) and any gap read as zeros
	chunkSize int64    // physical range-read granularity for each owner's chunked source (0 = default)
	open      OpenFunc // owner -> its compacted object (called once per owner, lazily)

	mu      sync.Mutex            // guards sources (defensive: the serve loop is single-threaded today)
	sources map[string]PageSource // owner -> its lazily-opened chunked source (excludes the "" zero-owner)
}

// NewLayeredSource builds a PageSource over a COW-layered memfile. extents (the flattened v2 header's
// mapping, converted) must be sorted by Logical and non-overlapping, with Owner set per run ("" = zeros);
// size is the logical memfile size; chunkSize is each owner's range-read granularity (0 = default); open
// resolves an owner to its object. The source owns every reader open hands it (all closed on Close).
func NewLayeredSource(extents []Extent, size, chunkSize int64, open OpenFunc) PageSource {
	return &layeredSource{extents: extents, size: size, chunkSize: chunkSize, open: open, sources: map[string]PageSource{}}
}

// ReadAt fills p at logical offset off, walking the runs/gaps it overlaps: a run owned by a build is read
// from that owner's object at the remapped physical offset; a zero-owner run or a gap is zero-filled with
// no fetch. The loop handles a buffer spanning several runs -- a hugepage fault, or a page straddling an
// owner boundary -- mirroring mappedSource.ReadAt.
func (l *layeredSource) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= l.size {
		return 0, io.EOF
	}
	end := off + int64(len(p))
	short := false
	if end > l.size { // a page reaching past the memfile end -- fill what exists, then signal EOF
		end = l.size
		short = true
	}
	filled := 0
	for cur := off; cur < end; {
		owner, phys, segLimit, present := l.locate(cur)
		if segLimit > end {
			segLimit = end
		}
		seg := p[filled : filled+int(segLimit-cur)]
		if present {
			src, err := l.sourceFor(owner)
			if err != nil {
				return filled, err
			}
			if err := readPhysFull(src, seg, phys); err != nil {
				return filled, fmt.Errorf("read owner %q memfile at phys %d (logical %d): %w", owner, phys, cur, err)
			}
		} else {
			clear(seg) // zero-owner run or gap -> zeros, no fetch
		}
		filled += len(seg)
		cur = segLimit
	}
	if short {
		return filled, io.EOF
	}
	return filled, nil
}

// locate classifies the logical offset cur against the sorted extents. A run covering cur that is owned
// by a build is present (returning its owner, the physical offset for cur, and the run's logical end); a
// zero-owner run or an uncovered gap is not present (zeros), returning where that zero region ends.
func (l *layeredSource) locate(cur int64) (owner string, phys, segLimit int64, present bool) {
	i := sort.Search(len(l.extents), func(i int) bool { return l.extents[i].Logical > cur }) - 1
	if i >= 0 {
		e := l.extents[i]
		if cur < e.Logical+e.Length { // cur is inside run i
			if e.Owner == "" {
				return "", 0, e.Logical + e.Length, false // zero-owner run -> zeros
			}
			return e.Owner, e.Physical + (cur - e.Logical), e.Logical + e.Length, true
		}
	}
	next := l.size
	if j := i + 1; j < len(l.extents) {
		next = l.extents[j].Logical
	}
	return "", 0, next, false // gap between runs -> zeros
}

// sourceFor returns owner's chunked source, opening + caching it on first use. Never called for "".
func (l *layeredSource) sourceFor(owner string) (PageSource, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if s, ok := l.sources[owner]; ok {
		return s, nil
	}
	ra, closer, err := l.open(owner)
	if err != nil {
		return nil, fmt.Errorf("open owner %q memfile: %w", owner, err)
	}
	s := NewChunkedSource(ra, closer, l.chunkSize)
	l.sources[owner] = s
	return s, nil
}

// Close releases every owner source opened over this VM's life.
func (l *layeredSource) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	var firstErr error
	for _, s := range l.sources {
		if err := s.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
