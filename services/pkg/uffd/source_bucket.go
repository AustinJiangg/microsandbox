//go:build linux

package uffd

import (
	"fmt"
	"io"
	"sync"
)

// chunkedSource is a PageSource that serves guest pages from a remote object (e.g. a *minio.Object
// range-reading from object storage) through a local chunk cache. It is the Stage 15 memfile page
// source -- the second impl of the interface Stage 15a extracted (MmapSource is the first).
//
// Why chunked, not per-page: UFFD faults each guest page at most once per VM, so a naive per-page
// range GET would issue one HTTP request per faulted page -- tens of thousands during a single boot,
// which would blow the orchestrator's ~10s health-check timeout. Instead a fault pulls the whole
// CHUNK containing it in one range read and caches it, so the many later faults within that chunk
// are served from memory. See docs/STAGE15_DESIGN.md, Decision 6 (chosen chunked from the start
// because per-page would not boot in time; a shared/evicting cache + tuned prefetch are deferred, §11).
//
// It takes a plain io.ReaderAt (the object), so pkg/uffd stays free of any storage / minio import.
type chunkedSource struct {
	ra        io.ReaderAt
	closer    func() error
	chunkSize int64

	mu     sync.Mutex       // the serve loop is single-threaded today; this guards the cache defensively
	chunks map[int64][]byte // chunkStart -> bytes (grows with the guest's touched working set)
}

// DefaultChunkSize is the range-read granularity (1 MiB = 256 base pages). Larger cuts the request
// count but over-fetches pages the guest may never touch; this is a middle ground for a learning impl.
const DefaultChunkSize = 1 << 20

// NewChunkedSource wraps ra (an object opened for range reads) as a PageSource, fetching and caching
// chunkSize-aligned windows. closer is called on Close (e.g. the underlying object's Close). A
// non-positive chunkSize uses DefaultChunkSize.
func NewChunkedSource(ra io.ReaderAt, closer func() error, chunkSize int64) PageSource {
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}
	return &chunkedSource{ra: ra, closer: closer, chunkSize: chunkSize, chunks: map[int64][]byte{}}
}

// ReadAt fills p (one page) from the chunk covering off, fetching + caching that chunk on a miss.
func (c *chunkedSource) ReadAt(p []byte, off int64) (int, error) {
	chunkStart := off - off%c.chunkSize
	chunk, err := c.chunk(chunkStart)
	if err != nil {
		return 0, err
	}
	inner := off - chunkStart
	if inner >= int64(len(chunk)) {
		return 0, io.EOF // off is past the object's end (the chunk was short)
	}
	n := copy(p, chunk[inner:])
	if n < len(p) {
		return n, io.EOF // p straddles the object's end -> readPage turns this into an error
	}
	return n, nil
}

// chunk returns the cached chunk at chunkStart, fetching it with one range read on a miss. A short
// read at the object's end is cached as a short chunk, so the end-of-object boundary is remembered.
func (c *chunkedSource) chunk(chunkStart int64) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if b, ok := c.chunks[chunkStart]; ok {
		return b, nil
	}
	buf := make([]byte, c.chunkSize)
	n, err := c.ra.ReadAt(buf, chunkStart)
	if err != nil && err != io.EOF { // io.EOF just means this is the last (short) chunk
		return nil, fmt.Errorf("fetch chunk @ %d: %w", chunkStart, err)
	}
	buf = buf[:n]
	c.chunks[chunkStart] = buf
	return buf, nil
}

func (c *chunkedSource) Close() error {
	if c.closer != nil {
		return c.closer()
	}
	return nil
}
