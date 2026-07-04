//go:build linux

package uffd

import (
	"bytes"
	"fmt"
	"io"
	"testing"
)

// newTestLayered assembles a 64-byte logical memfile from TWO owner objects, a zero-owner run, and a
// trailing gap -- the multi-build read Stage 20 adds:
//
//	logical [0,16)  = owner "A" @ phys 0   (0x0A)
//	logical [16,32) = owner "B" @ phys 0   (0x0B)
//	logical [32,48) = owner ""             (zero-owner run: overrides with zeros, no fetch)
//	logical [48,64) = gap (no extent)      (zeros, no fetch)
//
// The opener counts how many times each owner is opened so the tests can assert lazy, once-per-owner
// opening and that the zero regions open/read nothing.
func newTestLayered(chunkSize int64) (src PageSource, objs map[string]*countingReaderAt, opens map[string]int) {
	objs = map[string]*countingReaderAt{
		"A": {ra: bytes.NewReader(fill(make([]byte, 16), 0x0A))},
		"B": {ra: bytes.NewReader(fill(make([]byte, 16), 0x0B))},
	}
	opens = map[string]int{}
	open := func(owner string) (io.ReaderAt, func() error, error) {
		opens[owner]++
		cr, ok := objs[owner]
		if !ok {
			return nil, nil, fmt.Errorf("unknown owner %q", owner)
		}
		return cr, func() error { return nil }, nil
	}
	extents := []Extent{
		{Logical: 0, Length: 16, Physical: 0, Owner: "A"},
		{Logical: 16, Length: 16, Physical: 0, Owner: "B"},
		{Logical: 32, Length: 16, Physical: 0, Owner: ""}, // zero-owner override
	}
	return NewLayeredSource(extents, 64, chunkSize, open), objs, opens
}

// TestLayeredMultiOwnerStitch reads the whole memfile and asserts each run comes from the right owner (or
// zeros), each owner object is opened exactly once (lazy + cached), and a re-read opens nothing new.
func TestLayeredMultiOwnerStitch(t *testing.T) {
	src, objs, opens := newTestLayered(0)
	p := make([]byte, 64)
	n, err := src.ReadAt(p, 0)
	if err != nil || n != 64 {
		t.Fatalf("ReadAt(0,64) = %d, %v; want 64, nil", n, err)
	}
	want := bytes.Join([][]byte{
		fill(make([]byte, 16), 0x0A), // owner A
		fill(make([]byte, 16), 0x0B), // owner B
		make([]byte, 16),             // zero-owner -> zeros
		make([]byte, 16),             // gap -> zeros
	}, nil)
	if !bytes.Equal(p, want) {
		t.Fatalf("stitched read =\n%v\nwant\n%v", p, want)
	}
	if opens["A"] != 1 || opens["B"] != 1 {
		t.Fatalf("owner opens = %v; want A:1 B:1 (lazy, once each)", opens)
	}
	if objs["A"].reads == 0 || objs["B"].reads == 0 {
		t.Fatalf("owner reads = A:%d B:%d; want both > 0", objs["A"].reads, objs["B"].reads)
	}

	// A second full read is served from the per-owner caches: no new opens.
	if _, err := src.ReadAt(p, 0); err != nil {
		t.Fatalf("second ReadAt = %v", err)
	}
	if opens["A"] != 1 || opens["B"] != 1 {
		t.Fatalf("owner opens after re-read = %v; want unchanged A:1 B:1", opens)
	}
}

// TestLayeredCrossOwnerBoundary covers a single buffer straddling the A->B owner boundary (a page that
// spans two builds), which the ReadAt loop must serve from both owners.
func TestLayeredCrossOwnerBoundary(t *testing.T) {
	src, _, _ := newTestLayered(0)
	p := make([]byte, 16)
	n, err := src.ReadAt(p, 8) // [8,16) owner A, [16,24) owner B
	if err != nil || n != 16 {
		t.Fatalf("ReadAt(8,16) = %d, %v; want 16, nil", n, err)
	}
	want := append(fill(make([]byte, 8), 0x0A), fill(make([]byte, 8), 0x0B)...)
	if !bytes.Equal(p, want) {
		t.Fatalf("cross-owner read = %v; want %v", p, want)
	}
}

// TestLayeredZeroAndGapNoFetch asserts a zero-owner run and an uncovered gap both serve zeros WITHOUT
// opening or reading any owner object -- the COW zero-override + the Stage 17 gap elision, unified.
func TestLayeredZeroAndGapNoFetch(t *testing.T) {
	for _, c := range []struct {
		name string
		off  int64
	}{
		{"zero-owner run", 32},
		{"trailing gap", 48},
	} {
		t.Run(c.name, func(t *testing.T) {
			src, objs, opens := newTestLayered(0)
			p := make([]byte, 16)
			n, err := src.ReadAt(p, c.off)
			if err != nil || n != 16 || !bytes.Equal(p, make([]byte, 16)) {
				t.Fatalf("ReadAt(%d) = %d, %v, %v; want 16, nil, zeros", c.off, n, err, p)
			}
			if len(opens) != 0 || objs["A"].reads != 0 || objs["B"].reads != 0 {
				t.Fatalf("zero region touched storage: opens=%v A.reads=%d B.reads=%d; want none",
					opens, objs["A"].reads, objs["B"].reads)
			}
		})
	}
}

// TestLayeredEOF: an offset at/after the logical size is io.EOF.
func TestLayeredEOF(t *testing.T) {
	src, _, _ := newTestLayered(0)
	if _, err := src.ReadAt(make([]byte, 16), 64); err != io.EOF {
		t.Fatalf("ReadAt at size = %v; want io.EOF", err)
	}
}
