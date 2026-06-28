//go:build linux

package uffd

// KVM-/network-free test for the chunked bucket page source: it serves pages over an in-memory
// io.ReaderAt, proving correct bytes across chunk boundaries, that each chunk is fetched once
// (cached), and that a read past the end is a short read (which readPage turns into an error).

import (
	"bytes"
	"io"
	"testing"
)

// countingReaderAt counts ReadAt calls so the test can assert chunk caching (one fetch per chunk).
type countingReaderAt struct {
	ra    io.ReaderAt
	reads int
}

func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	c.reads++
	return c.ra.ReadAt(p, off)
}

func TestChunkedSource(t *testing.T) {
	const chunk = 16
	data := make([]byte, 40) // 2.5 chunks: [0,16) [16,32) [32,40)
	for i := range data {
		data[i] = byte(i)
	}
	counting := &countingReaderAt{ra: bytes.NewReader(data)}
	src := NewChunkedSource(counting, nil, chunk)
	defer src.Close()

	// A page (8 bytes) at offset 4, within chunk 0.
	p := make([]byte, 8)
	if err := readPage(src, p, 4); err != nil {
		t.Fatalf("readPage @4: %v", err)
	}
	if !bytes.Equal(p, data[4:12]) {
		t.Errorf("@4 = %v, want %v", p, data[4:12])
	}
	// Another page within chunk 0 is served from cache -- no second fetch.
	if err := readPage(src, p, 0); err != nil {
		t.Fatalf("readPage @0: %v", err)
	}
	if counting.reads != 1 {
		t.Errorf("chunk 0 fetched %d times, want 1 (cached)", counting.reads)
	}

	// A page in the short tail chunk (offset 34, chunk [32,40)) -> a second fetch, correct bytes.
	p2 := make([]byte, 4)
	if err := readPage(src, p2, 34); err != nil {
		t.Fatalf("readPage @34: %v", err)
	}
	if !bytes.Equal(p2, data[34:38]) {
		t.Errorf("@34 = %v, want %v", p2, data[34:38])
	}
	if counting.reads != 2 {
		t.Errorf("after touching chunk 2, fetched %d chunks, want 2", counting.reads)
	}

	// A page straddling the object end is a short read -> readPage errors (the memfile-bounds check).
	if err := readPage(src, make([]byte, 8), 38); err == nil {
		t.Error("readPage past end = nil, want an error")
	}
}
