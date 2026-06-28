//go:build linux

package uffd

import (
	"bytes"
	"io"
	"testing"
)

// countingReaderAt (source_bucket_test.go) counts ReadAt calls, so these tests can assert that a gap
// (zero) page is served WITHOUT touching the compacted object -- the central Stage 17 win.

func fill(b []byte, v byte) []byte {
	for i := range b {
		b[i] = v
	}
	return b
}

// newTestMapped builds a 64-byte logical memfile over a 48-byte compacted object:
//
//	logical [0,16)  = 0x01   (present, phys 0)
//	logical [16,32) = 0x00   (gap, not stored)
//	logical [32,48) = 0x02   (present, phys 16)  } one coalesced run [32,64), phys 16
//	logical [48,64) = 0x03   (present, phys 32)  }
func newTestMapped(chunkSize int64) (PageSource, *countingReaderAt) {
	compacted := make([]byte, 48)
	fill(compacted[0:16], 1)
	fill(compacted[16:32], 2)
	fill(compacted[32:48], 3)
	cr := &countingReaderAt{ra: bytes.NewReader(compacted)}
	extents := []Extent{{Logical: 0, Length: 16, Physical: 0}, {Logical: 32, Length: 32, Physical: 16}}
	return NewMappedSource(cr, func() error { return nil }, extents, 64, chunkSize), cr
}

func TestMappedSourcePresentAndGap(t *testing.T) {
	cases := []struct {
		name string
		off  int64
		want byte
	}{
		{"first present run", 0, 1},
		{"second run, first block", 32, 2},
		{"second run, second block", 48, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src, _ := newTestMapped(0)
			p := make([]byte, 16)
			n, err := src.ReadAt(p, c.off)
			if err != nil || n != 16 {
				t.Fatalf("ReadAt(%d) = %d, %v; want 16, nil", c.off, n, err)
			}
			if !bytes.Equal(p, fill(make([]byte, 16), c.want)) {
				t.Fatalf("ReadAt(%d) = %v; want all %d", c.off, p, c.want)
			}
		})
	}
}

// TestMappedSourceGapNoFetch is the key assertion: a fault in the zero gap returns a zero page and
// never reads the compacted object.
func TestMappedSourceGapNoFetch(t *testing.T) {
	src, cr := newTestMapped(0)
	p := make([]byte, 16)
	n, err := src.ReadAt(p, 16) // the gap [16,32)
	if err != nil || n != 16 {
		t.Fatalf("gap ReadAt = %d, %v; want 16, nil", n, err)
	}
	if !bytes.Equal(p, make([]byte, 16)) {
		t.Fatalf("gap page = %v; want all zero", p)
	}
	if cr.reads != 0 {
		t.Fatalf("gap page triggered %d physical reads; want 0", cr.reads)
	}
}

// TestMappedSourceSpanning covers a single buffer crossing present -> gap -> present (the hugepage
// case the loop must handle), with a small chunk size so the second run also spans a physical chunk.
func TestMappedSourceSpanning(t *testing.T) {
	src, _ := newTestMapped(16) // chunk size 16: the [32,64) run is two physical chunks
	p := make([]byte, 64)
	n, err := src.ReadAt(p, 0)
	if err != nil || n != 64 {
		t.Fatalf("spanning ReadAt = %d, %v; want 64, nil", n, err)
	}
	want := bytes.Join([][]byte{
		fill(make([]byte, 16), 1), // [0,16)  present
		make([]byte, 16),          // [16,32) gap -> zeros
		fill(make([]byte, 16), 2), // [32,48) present
		fill(make([]byte, 16), 3), // [48,64) present
	}, nil)
	if !bytes.Equal(p, want) {
		t.Fatalf("spanning read = %v\nwant %v", p, want)
	}
}

// TestMappedSourceAllZero: an empty mapping (a fully-zero memfile) serves every page as zeros with no
// reads, and an offset past the logical size is EOF.
func TestMappedSourceAllZero(t *testing.T) {
	cr := &countingReaderAt{ra: bytes.NewReader(nil)}
	src := NewMappedSource(cr, func() error { return nil }, nil, 32, 0)
	p := make([]byte, 16)
	if n, err := src.ReadAt(p, 0); err != nil || n != 16 || !bytes.Equal(p, make([]byte, 16)) {
		t.Fatalf("all-zero ReadAt(0) = %d, %v, %v; want 16, nil, zeros", n, err, p)
	}
	if cr.reads != 0 {
		t.Fatalf("all-zero memfile triggered %d reads; want 0", cr.reads)
	}
	if _, err := src.ReadAt(p, 32); err != io.EOF {
		t.Fatalf("ReadAt past size = %v; want io.EOF", err)
	}
}
