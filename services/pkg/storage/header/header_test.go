package header

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// blocks builds a memfile of blockSize-sized blocks from a pattern: 'z' = a zero block, 'd' = a
// data block filled with a distinct non-zero byte (so the compacted bytes are checkable). The byte
// for the i-th data block is byte(i+1), so successive data blocks differ.
func blocks(blockSize uint64, pattern string) (mem []byte, dataBlocks [][]byte) {
	var d int
	for _, c := range pattern {
		blk := make([]byte, blockSize)
		if c == 'd' {
			d++
			for i := range blk {
				blk[i] = byte(d)
			}
			dataBlocks = append(dataBlocks, blk)
		}
		mem = append(mem, blk...)
	}
	return mem, dataBlocks
}

func TestSerializeRoundTrip(t *testing.T) {
	h := Header{
		Metadata: Metadata{Version: Version, BlockSize: 4096, Size: 1 << 20},
		Mapping: Mapping{
			{Offset: 0, Length: 8192, BuildStorageOffset: 0},
			{Offset: 40960, Length: 4096, BuildStorageOffset: 8192},
		},
	}
	got, err := Deserialize(h.Serialize())
	if err != nil {
		t.Fatalf("Deserialize: %v", err)
	}
	if !reflect.DeepEqual(got, h) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", got, h)
	}
}

func TestBuildCompaction(t *testing.T) {
	const bs = 16
	// data, zero, data, data, zero(tail): two present runs -- [0,16) and [32,64).
	mem, data := blocks(bs, "dzddz")
	var out bytes.Buffer
	h, err := Build(bytes.NewReader(mem), bs, &out)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	wantMap := Mapping{
		{Offset: 0, Length: bs, BuildStorageOffset: 0},           // the first 'd'
		{Offset: 2 * bs, Length: 2 * bs, BuildStorageOffset: bs}, // the coalesced 'dd'
	}
	if !reflect.DeepEqual(h.Mapping, wantMap) {
		t.Fatalf("mapping:\n got %+v\nwant %+v", h.Mapping, wantMap)
	}
	if h.Metadata.Size != uint64(len(mem)) {
		t.Fatalf("Size = %d, want %d", h.Metadata.Size, len(mem))
	}
	// The compacted object is exactly the present blocks concatenated, in order -- zeros omitted.
	wantBytes := append(append([]byte{}, data[0]...), append(data[1], data[2]...)...)
	if !bytes.Equal(out.Bytes(), wantBytes) {
		t.Fatalf("compacted bytes = %d, want %d (present blocks only)", out.Len(), len(wantBytes))
	}
}

func TestBuildAllZero(t *testing.T) {
	const bs = 16
	mem, _ := blocks(bs, "zzz")
	var out bytes.Buffer
	h, err := Build(bytes.NewReader(mem), bs, &out)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(h.Mapping) != 0 {
		t.Fatalf("all-zero memfile: mapping = %+v, want empty", h.Mapping)
	}
	if out.Len() != 0 {
		t.Fatalf("all-zero memfile: compacted %d bytes, want 0", out.Len())
	}
	if h.Metadata.Size != uint64(len(mem)) {
		t.Fatalf("Size = %d, want %d", h.Metadata.Size, len(mem))
	}
}

func TestBuildAllPresent(t *testing.T) {
	const bs = 16
	mem, _ := blocks(bs, "ddd")
	var out bytes.Buffer
	h, err := Build(bytes.NewReader(mem), bs, &out)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// One coalesced run covering everything; the compacted object equals the input.
	want := Mapping{{Offset: 0, Length: uint64(len(mem)), BuildStorageOffset: 0}}
	if !reflect.DeepEqual(h.Mapping, want) {
		t.Fatalf("mapping:\n got %+v\nwant %+v", h.Mapping, want)
	}
	if !bytes.Equal(out.Bytes(), mem) {
		t.Fatalf("compacted bytes differ from input")
	}
}

// TestBuildShortFinalBlock covers a memfile whose size is not a block multiple: the final partial
// block must still be indexed at its true (short) length, and Size must be the exact byte count.
func TestBuildShortFinalBlock(t *testing.T) {
	const bs = 16
	mem, _ := blocks(bs, "d")
	mem = append(mem, 7, 7, 7) // a 3-byte non-zero tail (a short final block)
	var out bytes.Buffer
	h, err := Build(bytes.NewReader(mem), bs, &out)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	want := Mapping{{Offset: 0, Length: uint64(len(mem)), BuildStorageOffset: 0}}
	if !reflect.DeepEqual(h.Mapping, want) {
		t.Fatalf("mapping:\n got %+v\nwant %+v", h.Mapping, want)
	}
	if h.Metadata.Size != uint64(len(mem)) {
		t.Fatalf("Size = %d, want %d", h.Metadata.Size, len(mem))
	}
}

func TestBuildDefaultBlockSize(t *testing.T) {
	h, err := Build(bytes.NewReader(make([]byte, 100)), 0, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if h.Metadata.BlockSize != DefaultBlockSize {
		t.Fatalf("BlockSize = %d, want default %d", h.Metadata.BlockSize, DefaultBlockSize)
	}
}

func TestDeserializeErrors(t *testing.T) {
	t.Run("too short", func(t *testing.T) {
		if _, err := Deserialize([]byte{1, 2, 3}); err == nil {
			t.Fatal("want error for a short buffer")
		}
	})
	t.Run("bad version", func(t *testing.T) {
		b := Header{Metadata: Metadata{Version: 99}}.Serialize()
		if _, err := Deserialize(b); err == nil {
			t.Fatal("want error for an unknown version")
		}
	})
	t.Run("mapping length mismatch", func(t *testing.T) {
		b := Header{Metadata: Metadata{Version: Version}, Mapping: Mapping{{Length: 1}}}.Serialize()
		if _, err := Deserialize(b[:len(b)-1]); err == nil {
			t.Fatal("want error when trailing bytes do not match the declared count")
		}
	})
}

// TestBuildFile exercises the on-disk convenience: it compacts to a sibling temp file whose contents
// match the in-memory Build, and the returned header matches too.
func TestBuildFile(t *testing.T) {
	const bs = 16
	mem, _ := blocks(bs, "dzd")
	dir := t.TempDir()
	memPath := filepath.Join(dir, "memfile")
	if err := os.WriteFile(memPath, mem, 0o644); err != nil {
		t.Fatal(err)
	}

	h, compactPath, err := BuildFile(memPath, bs)
	if err != nil {
		t.Fatalf("BuildFile: %v", err)
	}
	defer os.Remove(compactPath)

	var want bytes.Buffer
	wantH, _ := Build(bytes.NewReader(mem), bs, &want)
	if !reflect.DeepEqual(h, wantH) {
		t.Fatalf("header:\n got %+v\nwant %+v", h, wantH)
	}
	got, err := os.ReadFile(compactPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want.Bytes()) {
		t.Fatalf("compacted file (%d bytes) != in-memory Build (%d bytes)", len(got), want.Len())
	}
}
