package header

import (
	"bytes"
	"reflect"
	"testing"
)

// TestSerializeRoundTripV2 round-trips a layered (v2) header: metadata generation/build ids and the
// per-run owners must survive the build-table encoding unchanged.
func TestSerializeRoundTripV2(t *testing.T) {
	h := Header{
		Metadata: Metadata{
			Version: VersionLayered, BlockSize: 4096, Size: 1 << 20,
			Generation: 2, BuildId: "build-b", BaseBuildId: "build-a",
		},
		Mapping: Mapping{
			{Offset: 0, Length: 4096, BuildStorageOffset: 0, Owner: "build-a"},
			{Offset: 4096, Length: 8192, BuildStorageOffset: 0, Owner: "build-b"},
			{Offset: 12288, Length: 4096, BuildStorageOffset: 0, Owner: ""}, // a zero run
			{Offset: 16384, Length: 4096, BuildStorageOffset: 4096, Owner: "build-a"},
		},
	}
	got, err := Deserialize(h.Serialize())
	if err != nil {
		t.Fatalf("Deserialize: %v", err)
	}
	if !reflect.DeepEqual(got, h) {
		t.Fatalf("v2 round trip mismatch:\n got %+v\nwant %+v", got, h)
	}
}

// TestV1AndV2Coexist proves the version dispatch: a v1 (memfile) header and a v2 (layered) header
// serialize to different layouts but both parse back, so an old v1 bucket still boots after Stage 18.
func TestV1AndV2Coexist(t *testing.T) {
	v1 := Header{Metadata: Metadata{Version: Version, BlockSize: 4096, Size: 8192},
		Mapping: Mapping{{Offset: 0, Length: 4096, BuildStorageOffset: 0}}}
	v2 := Header{Metadata: Metadata{Version: VersionLayered, BlockSize: 4096, Size: 8192, BuildId: "b"},
		Mapping: Mapping{{Offset: 0, Length: 4096, BuildStorageOffset: 0, Owner: "b"}}}
	for _, h := range []Header{v1, v2} {
		got, err := Deserialize(h.Serialize())
		if err != nil {
			t.Fatalf("Deserialize v%d: %v", h.Metadata.Version, err)
		}
		if !reflect.DeepEqual(got, h) {
			t.Fatalf("v%d round trip:\n got %+v\nwant %+v", h.Metadata.Version, got, h)
		}
	}
}

// img builds `count` blocks of blockSize bytes, block i filled with vals[i] (so a block is checkable
// and a 0 value yields a zero block).
func img(blockSize int, vals ...byte) []byte {
	out := make([]byte, 0, blockSize*len(vals))
	for _, v := range vals {
		blk := make([]byte, blockSize)
		for i := range blk {
			blk[i] = v
		}
		out = append(out, blk...)
	}
	return out
}

// reconstruct rebuilds the full image from a flattened mapping by copying each run from its owner's
// storage object (src(owner)); a zero-owner run is left as zeros. It is the in-test stand-in for the
// Stage-18b assemble path, validating the algebra end to end without any storage.
func reconstruct(m Mapping, size int, src func(owner string) []byte) []byte {
	out := make([]byte, size)
	for _, run := range m {
		if run.Owner == "" {
			continue
		}
		obj := src(run.Owner)
		copy(out[run.Offset:run.Offset+run.Length], obj[run.BuildStorageOffset:run.BuildStorageOffset+run.Length])
	}
	return out
}

// TestBuildDiffAndReconstruct is the heart of 18a: a child that changes some blocks (one to non-zero,
// one to zero) diffs to only those blocks; merging onto the base yields a whole-covering mapping that
// reconstructs the child exactly, reading unchanged ranges from the base and changed ones from the diff.
func TestBuildDiffAndReconstruct(t *testing.T) {
	const bs = 4
	base := img(bs, 1, 2, 3, 4, 5, 6)
	child := img(bs, 1, 9, 3, 0, 5, 8) // block1 1->9 (data), block3 4->0 (zeroed), block5 6->8 (data)
	size := int64(len(base))

	var diff bytes.Buffer
	diffMap, err := BuildDiff(bytes.NewReader(base), bytes.NewReader(child), size, bs, "B", &diff)
	if err != nil {
		t.Fatalf("BuildDiff: %v", err)
	}
	wantDiffMap := Mapping{
		{Offset: 4, Length: 4, BuildStorageOffset: 0, Owner: "B"},
		{Offset: 12, Length: 4, BuildStorageOffset: 0, Owner: ""}, // zeroed block -> zero owner, no bytes stored
		{Offset: 20, Length: 4, BuildStorageOffset: 4, Owner: "B"},
	}
	if !reflect.DeepEqual(diffMap, wantDiffMap) {
		t.Fatalf("diff mapping:\n got %+v\nwant %+v", diffMap, wantDiffMap)
	}
	// Only the two changed non-zero blocks are stored (9999 then 8888): the zeroed block costs nothing.
	if want := img(bs, 9, 8); !bytes.Equal(diff.Bytes(), want) {
		t.Fatalf("diff bytes = %v, want %v", diff.Bytes(), want)
	}

	flat := MergeMappings(SingleBuildMapping(uint64(size), "A"), diffMap)
	got := reconstruct(flat, int(size), func(owner string) []byte {
		switch owner {
		case "A":
			return base // base owner has identity storage offsets
		case "B":
			return diff.Bytes()
		}
		return nil
	})
	if !bytes.Equal(got, child) {
		t.Fatalf("reconstructed image != child:\n got %v\nwant %v", got, child)
	}
}

// TestBuildDiffIdentical: a child equal to the base diffs to nothing (empty mapping, no stored bytes).
func TestBuildDiffIdentical(t *testing.T) {
	const bs = 4
	base := img(bs, 1, 2, 3)
	var diff bytes.Buffer
	m, err := BuildDiff(bytes.NewReader(base), bytes.NewReader(base), int64(len(base)), bs, "B", &diff)
	if err != nil {
		t.Fatalf("BuildDiff: %v", err)
	}
	if len(m) != 0 || diff.Len() != 0 {
		t.Fatalf("identical child: mapping=%+v diffBytes=%d, want empty/0", m, diff.Len())
	}
}

func TestMergeMappings(t *testing.T) {
	tests := []struct {
		name string
		base Mapping
		diff Mapping
		want Mapping
	}{
		{
			name: "empty diff returns base",
			base: Mapping{{Offset: 0, Length: 100, Owner: "A"}},
			diff: nil,
			want: Mapping{{Offset: 0, Length: 100, Owner: "A"}},
		},
		{
			name: "diff inside base splits into left+diff+right",
			base: Mapping{{Offset: 0, Length: 100, BuildStorageOffset: 0, Owner: "A"}},
			diff: Mapping{{Offset: 40, Length: 20, BuildStorageOffset: 0, Owner: "B"}},
			want: Mapping{
				{Offset: 0, Length: 40, BuildStorageOffset: 0, Owner: "A"},
				{Offset: 40, Length: 20, BuildStorageOffset: 0, Owner: "B"},
				{Offset: 60, Length: 40, BuildStorageOffset: 60, Owner: "A"},
			},
		},
		{
			name: "base inside diff is dropped",
			base: Mapping{{Offset: 40, Length: 20, BuildStorageOffset: 0, Owner: "A"}},
			diff: Mapping{{Offset: 0, Length: 100, BuildStorageOffset: 0, Owner: "B"}},
			want: Mapping{{Offset: 0, Length: 100, BuildStorageOffset: 0, Owner: "B"}},
		},
		{
			name: "two disjoint diffs inside one base",
			base: Mapping{{Offset: 0, Length: 100, BuildStorageOffset: 0, Owner: "A"}},
			diff: Mapping{
				{Offset: 10, Length: 10, BuildStorageOffset: 0, Owner: "B"},
				{Offset: 50, Length: 10, BuildStorageOffset: 10, Owner: "C"},
			},
			want: Mapping{
				{Offset: 0, Length: 10, BuildStorageOffset: 0, Owner: "A"},
				{Offset: 10, Length: 10, BuildStorageOffset: 0, Owner: "B"},
				{Offset: 20, Length: 30, BuildStorageOffset: 20, Owner: "A"},
				{Offset: 50, Length: 10, BuildStorageOffset: 10, Owner: "C"},
				{Offset: 60, Length: 40, BuildStorageOffset: 60, Owner: "A"},
			},
		},
		{
			name: "diff overlaps base start, base right remainder shifts",
			base: Mapping{{Offset: 20, Length: 30, BuildStorageOffset: 0, Owner: "A"}},
			diff: Mapping{{Offset: 0, Length: 30, BuildStorageOffset: 0, Owner: "B"}},
			want: Mapping{
				{Offset: 0, Length: 30, BuildStorageOffset: 0, Owner: "B"},
				{Offset: 30, Length: 20, BuildStorageOffset: 10, Owner: "A"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := MergeMappings(tc.base, tc.diff)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("MergeMappings:\n got %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

func TestNormalizeMappings(t *testing.T) {
	tests := []struct {
		name string
		in   Mapping
		want Mapping
	}{
		{
			name: "same owner, logically + storage contiguous -> coalesced",
			in:   Mapping{{Offset: 0, Length: 4, BuildStorageOffset: 0, Owner: "A"}, {Offset: 4, Length: 4, BuildStorageOffset: 4, Owner: "A"}, {Offset: 8, Length: 4, BuildStorageOffset: 0, Owner: "B"}},
			want: Mapping{{Offset: 0, Length: 8, BuildStorageOffset: 0, Owner: "A"}, {Offset: 8, Length: 4, BuildStorageOffset: 0, Owner: "B"}},
		},
		{
			name: "zero runs coalesce on logical contiguity alone",
			in:   Mapping{{Offset: 0, Length: 4, BuildStorageOffset: 0, Owner: ""}, {Offset: 4, Length: 4, BuildStorageOffset: 99, Owner: ""}},
			want: Mapping{{Offset: 0, Length: 8, BuildStorageOffset: 0, Owner: ""}},
		},
		{
			name: "same owner but storage discontiguous -> kept separate",
			in:   Mapping{{Offset: 0, Length: 4, BuildStorageOffset: 0, Owner: "A"}, {Offset: 4, Length: 4, BuildStorageOffset: 99, Owner: "A"}},
			want: Mapping{{Offset: 0, Length: 4, BuildStorageOffset: 0, Owner: "A"}, {Offset: 4, Length: 4, BuildStorageOffset: 99, Owner: "A"}},
		},
		{
			name: "logically non-contiguous -> kept separate",
			in:   Mapping{{Offset: 0, Length: 4, BuildStorageOffset: 0, Owner: "A"}, {Offset: 8, Length: 4, BuildStorageOffset: 4, Owner: "A"}},
			want: Mapping{{Offset: 0, Length: 4, BuildStorageOffset: 0, Owner: "A"}, {Offset: 8, Length: 4, BuildStorageOffset: 4, Owner: "A"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := NormalizeMappings(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("NormalizeMappings:\n got %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

func TestLocate(t *testing.T) {
	h := Header{
		Metadata: Metadata{Version: VersionLayered, BlockSize: 4, Size: 24},
		Mapping: Mapping{
			{Offset: 0, Length: 4, BuildStorageOffset: 0, Owner: "A"},
			{Offset: 4, Length: 4, BuildStorageOffset: 0, Owner: "B"},
			{Offset: 8, Length: 4, BuildStorageOffset: 8, Owner: "A"},
			{Offset: 12, Length: 4, BuildStorageOffset: 0, Owner: ""},
			{Offset: 16, Length: 4, BuildStorageOffset: 16, Owner: "A"},
			{Offset: 20, Length: 4, BuildStorageOffset: 4, Owner: "B"},
		},
	}
	cases := []struct {
		off                int64
		owner              string
		storageOff, runEnd int64
		ok                 bool
	}{
		{0, "A", 0, 4, true},
		{5, "B", 1, 8, true},    // 1 byte into the B run at logical 4 (storage 0)
		{10, "A", 10, 12, true}, // 2 bytes into the A run at logical 8 (storage 8)
		{13, "", 0, 16, true},   // inside the zero run
		{23, "B", 7, 24, true},
		{24, "", 0, 0, false}, // out of range
		{-1, "", 0, 0, false},
	}
	for _, c := range cases {
		owner, so, re, ok := h.Locate(c.off)
		if owner != c.owner || so != c.storageOff || re != c.runEnd || ok != c.ok {
			t.Fatalf("Locate(%d) = (%q,%d,%d,%v), want (%q,%d,%d,%v)",
				c.off, owner, so, re, ok, c.owner, c.storageOff, c.runEnd, c.ok)
		}
	}
}

func TestDeserializeV2Errors(t *testing.T) {
	valid := Header{
		Metadata: Metadata{Version: VersionLayered, BlockSize: 4, Size: 8, BuildId: "b"},
		Mapping:  Mapping{{Offset: 0, Length: 4, BuildStorageOffset: 0, Owner: "b"}},
	}.Serialize()

	t.Run("truncated mapping", func(t *testing.T) {
		if _, err := Deserialize(valid[:len(valid)-1]); err == nil {
			t.Fatal("want error when trailing bytes do not match the declared count")
		}
	})
	t.Run("truncated head", func(t *testing.T) {
		if _, err := Deserialize(valid[:16]); err == nil {
			t.Fatal("want error for a head shorter than v2HeadSize")
		}
	})
}
