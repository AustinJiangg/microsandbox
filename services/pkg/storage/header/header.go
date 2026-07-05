// Package header is the snapshot artifact index: a per-block map of which logical ranges of a memory
// file (or rootfs) actually hold data and -- with copy-on-write layering -- which build owns each
// range, so a build can store only its own blocks and the boot path can fetch each range from the
// build that owns it (and serve gaps as zeros without a fetch). It mirrors E2B's
// packages/shared/pkg/storage/header (Metadata + an ordered Mapping of BuildMap entries; a logical
// offset resolves to a (build, storage offset), and unmapped gaps are zeros that are never read).
// See docs/STAGE17_DESIGN.md (the compacted single-build memfile) and docs/STAGE18_DESIGN.md (COW).
//
// Two on-disk formats coexist, dispatched by Metadata.Version:
//
//	v1 (Version, Stage 17): single flat build -- Version/BlockSize/Size + an ordered mapping of present
//	  runs, no owner. Every present block belongs to the one build. Used by the streamed memfile.
//	v2 (VersionLayered, Stage 18): COW layers -- Version/BlockSize/Size/Generation + BuildId/BaseBuildId,
//	  and each BuildMap carries the owning build. Used by a layered (base-derived) rootfs. The owner is
//	  serialized as an index into a header-local build table (see Decision 5 in STAGE18_DESIGN.md): this
//	  keeps fixed-width entries and string build IDs with zero new dependencies (no uuid type).
//
// The package is pure (encoding/binary + io + os + sort): no network, no storage/minio import, no KVM.
// That keeps it a leaf both the producers (pkg/build, cmd/msb-seed) and the boot path (cmd/orchestrator,
// pkg/storage) can depend on, and fully unit-testable.
package header

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// Version is the v1 (Stage 17) on-disk format: a single flat build, no per-run owner. The streamed
// memfile uses it.
const Version uint64 = 1

// VersionLayered is the v2 (Stage 18) format: copy-on-write layers, each run owned by a build. A
// layered (base-derived) rootfs uses it. The reader dispatches on the version word, so v1 and v2
// objects coexist and an old (v1) bucket still boots.
const VersionLayered uint64 = 2

// DefaultBlockSize is the index granularity in bytes: the guest base page size, so each faulting page
// maps to exactly one block (wholly zero or wholly present -- see docs/STAGE17_DESIGN.md, Decision 3).
const DefaultBlockSize uint64 = 4096

// Metadata is the fixed-size head of a serialized header. Generation/BuildId/BaseBuildId are v2-only
// (zero/"" in v1) -- they link a layer to its base and order the chain (E2B's metadata.go).
type Metadata struct {
	Version     uint64
	BlockSize   uint64 // index granularity in bytes
	Size        uint64 // total LOGICAL size of the indexed image (gaps up to Size read as zero)
	Generation  uint64 // v2: layer generation in the chain (0 for a base/full build)
	BuildId     string // v2: the build this header describes ("" in v1)
	BaseBuildId string // v2: the base build at the root of the chain ("" in v1 / a base build)
}

// BuildMap maps one logical run of the image to its bytes in a build's stored object. Mirrors E2B's
// BuildMap; Owner is the build that owns the run ("" = the zero/gap owner, E2B's uuid.Nil -- served as
// zeros, never fetched). Owner is unused in v1 (single build), set in v2 (layers).
type BuildMap struct {
	Offset             uint64 // logical byte offset of the run (block-aligned)
	Length             uint64 // run length in bytes (block-multiple, except possibly a short final block)
	BuildStorageOffset uint64 // byte offset of this run inside Owner's stored object
	Owner              string // v2: the build that owns this run ("" = zeros)
}

// Mapping is the ordered (ascending Offset), non-overlapping list of runs.
type Mapping = []BuildMap

// Header is a parsed index: its metadata plus the run mapping.
type Header struct {
	Metadata Metadata
	Mapping  Mapping
}

// Serialization is fixed-width little-endian. v1 and v2 share the word size; their head/entry layouts
// differ (v2 adds Generation + the build-table indices + a per-entry owner index).
const (
	wordSize = 8

	// v1: {Version,BlockSize,Size} ‖ count ‖ entries{Offset,Length,BuildStorageOffset}.
	v1MetaWords = 3
	v1HeadWords = v1MetaWords + 1 // + the mapping count
	v1MapWords  = 3
	v1HeadSize  = v1HeadWords * wordSize
	v1EntrySize = v1MapWords * wordSize

	// v2: {Version,BlockSize,Size,Generation,buildIdIdx,baseBuildIdIdx,tableCount} ‖ table ‖ count ‖
	//     entries{Offset,Length,BuildStorageOffset,ownerIdx}. The table is `tableCount` length-prefixed
	//     UTF-8 build IDs (index 0 is "" by convention -- the zero/gap owner).
	v2HeadWords  = 7
	v2HeadSize   = v2HeadWords * wordSize
	v2EntryWords = 4
	v2EntrySize  = v2EntryWords * wordSize
)

// Serialize encodes the header, choosing the format by Metadata.Version (v1 for the single-build
// memfile, v2 for a layered rootfs). Self-describing and fixed-width -- the same hand-rolled binary
// discipline pkg/uffd uses for the kernel ABI structs (no external schema, no new dependency).
func (h Header) Serialize() []byte {
	if h.Metadata.Version >= VersionLayered {
		return h.serializeV2()
	}
	return h.serializeV1()
}

func (h Header) serializeV1() []byte {
	b := make([]byte, 0, v1HeadSize+len(h.Mapping)*v1EntrySize)
	b = binary.LittleEndian.AppendUint64(b, h.Metadata.Version)
	b = binary.LittleEndian.AppendUint64(b, h.Metadata.BlockSize)
	b = binary.LittleEndian.AppendUint64(b, h.Metadata.Size)
	b = binary.LittleEndian.AppendUint64(b, uint64(len(h.Mapping)))
	for _, m := range h.Mapping {
		b = binary.LittleEndian.AppendUint64(b, m.Offset)
		b = binary.LittleEndian.AppendUint64(b, m.Length)
		b = binary.LittleEndian.AppendUint64(b, m.BuildStorageOffset)
	}
	return b
}

func (h Header) serializeV2() []byte {
	table, idx := h.buildTable()
	b := make([]byte, 0, v2HeadSize+len(h.Mapping)*v2EntrySize)
	b = binary.LittleEndian.AppendUint64(b, h.Metadata.Version)
	b = binary.LittleEndian.AppendUint64(b, h.Metadata.BlockSize)
	b = binary.LittleEndian.AppendUint64(b, h.Metadata.Size)
	b = binary.LittleEndian.AppendUint64(b, h.Metadata.Generation)
	b = binary.LittleEndian.AppendUint64(b, uint64(idx[h.Metadata.BuildId]))
	b = binary.LittleEndian.AppendUint64(b, uint64(idx[h.Metadata.BaseBuildId]))
	b = binary.LittleEndian.AppendUint64(b, uint64(len(table)))
	for _, s := range table {
		b = binary.LittleEndian.AppendUint64(b, uint64(len(s)))
		b = append(b, s...)
	}
	b = binary.LittleEndian.AppendUint64(b, uint64(len(h.Mapping)))
	for _, m := range h.Mapping {
		b = binary.LittleEndian.AppendUint64(b, m.Offset)
		b = binary.LittleEndian.AppendUint64(b, m.Length)
		b = binary.LittleEndian.AppendUint64(b, m.BuildStorageOffset)
		b = binary.LittleEndian.AppendUint64(b, uint64(idx[m.Owner]))
	}
	return b
}

// buildTable returns the ordered, de-duplicated list of build-ID strings this header references (index
// 0 is always "" -- the zero/gap owner) and the reverse index used to serialize owners as table indices.
func (h Header) buildTable() (table []string, idx map[string]int) {
	idx = map[string]int{"": 0}
	table = []string{""}
	add := func(s string) {
		if _, ok := idx[s]; !ok {
			idx[s] = len(table)
			table = append(table, s)
		}
	}
	add(h.Metadata.BuildId)
	add(h.Metadata.BaseBuildId)
	for _, m := range h.Mapping {
		add(m.Owner)
	}
	return table, idx
}

// Deserialize parses bytes produced by Serialize, dispatching on the version word. It rejects a short
// buffer, an unknown version, and a length that does not match -- so a truncated or wrong-format header
// fails loudly at boot, not as a silent mis-read mid-fault.
func Deserialize(b []byte) (Header, error) {
	if len(b) < wordSize {
		return Header{}, fmt.Errorf("header too short: %d bytes (want >= %d)", len(b), wordSize)
	}
	switch ver := binary.LittleEndian.Uint64(b[0:]); ver {
	case Version:
		return deserializeV1(b)
	case VersionLayered:
		return deserializeV2(b)
	default:
		return Header{}, fmt.Errorf("unsupported header version %d (want %d or %d)", ver, Version, VersionLayered)
	}
}

func deserializeV1(b []byte) (Header, error) {
	if len(b) < v1HeadSize {
		return Header{}, fmt.Errorf("header too short: %d bytes (want >= %d)", len(b), v1HeadSize)
	}
	md := Metadata{
		Version:   binary.LittleEndian.Uint64(b[0:]),
		BlockSize: binary.LittleEndian.Uint64(b[8:]),
		Size:      binary.LittleEndian.Uint64(b[16:]),
	}
	count := binary.LittleEndian.Uint64(b[24:])
	rest := b[v1HeadSize:]
	if uint64(len(rest)) != count*v1EntrySize {
		return Header{}, fmt.Errorf("header mapping length mismatch: %d entries declared, %d trailing bytes",
			count, len(rest))
	}
	mapping := make(Mapping, count)
	for i := range mapping {
		e := rest[i*v1EntrySize:]
		mapping[i] = BuildMap{
			Offset:             binary.LittleEndian.Uint64(e[0:]),
			Length:             binary.LittleEndian.Uint64(e[8:]),
			BuildStorageOffset: binary.LittleEndian.Uint64(e[16:]),
		}
	}
	return Header{Metadata: md, Mapping: mapping}, nil
}

func deserializeV2(b []byte) (Header, error) {
	if len(b) < v2HeadSize {
		return Header{}, fmt.Errorf("layered header too short: %d bytes (want >= %d)", len(b), v2HeadSize)
	}
	md := Metadata{
		Version:    binary.LittleEndian.Uint64(b[0:]),
		BlockSize:  binary.LittleEndian.Uint64(b[8:]),
		Size:       binary.LittleEndian.Uint64(b[16:]),
		Generation: binary.LittleEndian.Uint64(b[24:]),
	}
	buildIdIdx := binary.LittleEndian.Uint64(b[32:])
	baseBuildIdIdx := binary.LittleEndian.Uint64(b[40:])
	tableCount := binary.LittleEndian.Uint64(b[48:])
	rest := b[v2HeadSize:]
	table := make([]string, tableCount)
	for i := range table {
		s, r, err := readString(rest)
		if err != nil {
			return Header{}, fmt.Errorf("build table entry %d: %w", i, err)
		}
		table[i], rest = s, r
	}
	resolve := func(i uint64) (string, error) {
		if i >= tableCount {
			return "", fmt.Errorf("owner index %d out of range (table has %d entries)", i, tableCount)
		}
		return table[i], nil
	}
	var err error
	if md.BuildId, err = resolve(buildIdIdx); err != nil {
		return Header{}, err
	}
	if md.BaseBuildId, err = resolve(baseBuildIdIdx); err != nil {
		return Header{}, err
	}
	if len(rest) < wordSize {
		return Header{}, fmt.Errorf("layered header truncated before mapping count")
	}
	count := binary.LittleEndian.Uint64(rest)
	rest = rest[wordSize:]
	if uint64(len(rest)) != count*v2EntrySize {
		return Header{}, fmt.Errorf("layered header mapping length mismatch: %d entries declared, %d trailing bytes",
			count, len(rest))
	}
	mapping := make(Mapping, count)
	for i := range mapping {
		e := rest[i*v2EntrySize:]
		owner, err := resolve(binary.LittleEndian.Uint64(e[24:]))
		if err != nil {
			return Header{}, err
		}
		mapping[i] = BuildMap{
			Offset:             binary.LittleEndian.Uint64(e[0:]),
			Length:             binary.LittleEndian.Uint64(e[8:]),
			BuildStorageOffset: binary.LittleEndian.Uint64(e[16:]),
			Owner:              owner,
		}
	}
	return Header{Metadata: md, Mapping: mapping}, nil
}

// readString reads a length-prefixed UTF-8 string (a uint64 length then that many bytes) from the front
// of b, returning it and the remaining bytes. It guards a short buffer so a truncated table fails loudly.
func readString(b []byte) (string, []byte, error) {
	if len(b) < wordSize {
		return "", nil, fmt.Errorf("truncated string length")
	}
	n := binary.LittleEndian.Uint64(b)
	b = b[wordSize:]
	if uint64(len(b)) < n {
		return "", nil, fmt.Errorf("truncated string: want %d bytes, have %d", n, len(b))
	}
	return string(b[:n]), b[n:], nil
}

// Build scans r (a memfile) in blockSize blocks, writing only the non-zero blocks to out (the
// compacted object) and recording each maximal run of present blocks as one BuildMap. It returns the
// v1 header describing that compaction. A non-positive blockSize uses DefaultBlockSize.
//
// This is the single-build analogue of E2B's CreateMapping over a dirty bitmap: our "is this block
// non-zero" stands in for E2B's "did this block change vs the parent" -- same mapping shape, simpler
// source. Runs are coalesced, so a mostly-zero memfile yields a tiny mapping (one entry per island of
// data), and out holds only the present bytes. (BuildDiff is the layered, base-relative analogue.)
func Build(r io.Reader, blockSize uint64, out io.Writer) (Header, error) {
	if blockSize == 0 {
		blockSize = DefaultBlockSize
	}
	buf := make([]byte, blockSize)
	var (
		mapping    Mapping
		logicalOff uint64    // bytes scanned so far (the next block's logical offset)
		storageOff uint64    // bytes written to out so far (the next present run's storage offset)
		cur        *BuildMap // the open present run, or nil between runs
	)
	flush := func() {
		if cur != nil {
			mapping = append(mapping, *cur)
			cur = nil
		}
	}
	for {
		// ReadFull gives a full block, or the short final block (ErrUnexpectedEOF), or EOF at the end.
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			block := buf[:n]
			if isZero(block) {
				flush() // a zero block ends any open present run
			} else {
				if _, werr := out.Write(block); werr != nil {
					return Header{}, fmt.Errorf("write compacted block at %d: %w", logicalOff, werr)
				}
				if cur == nil {
					cur = &BuildMap{Offset: logicalOff, Length: uint64(n), BuildStorageOffset: storageOff}
				} else {
					cur.Length += uint64(n)
				}
				storageOff += uint64(n)
			}
			logicalOff += uint64(n)
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return Header{}, fmt.Errorf("read memfile at %d: %w", logicalOff, err)
		}
	}
	flush()
	return Header{
		Metadata: Metadata{Version: Version, BlockSize: blockSize, Size: logicalOff},
		Mapping:  mapping,
	}, nil
}

// BuildFile is the on-disk convenience the producers use: it reads memfilePath and writes the compacted
// bytes to a sibling temp file, returning the header and that temp file's path. The caller uploads both
// (the header via Serialize, the compacted file as {buildID}/memfile) and removes the temp file. The
// temp file sits beside memfilePath so the rename/cleanup stays on one filesystem.
func BuildFile(memfilePath string, blockSize uint64) (Header, string, error) {
	in, err := os.Open(memfilePath)
	if err != nil {
		return Header{}, "", fmt.Errorf("open memfile %s: %w", memfilePath, err)
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(memfilePath), "memfile-compact-*")
	if err != nil {
		return Header{}, "", fmt.Errorf("create compacted temp: %w", err)
	}
	h, err := Build(in, blockSize, tmp)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp.Name())
		return Header{}, "", err
	}
	return h, tmp.Name(), nil
}

// SingleBuildMapping is the whole-image mapping of a non-layered build: one run [0,size) owned by owner
// with an identity storage offset. It is the base a first diff merges onto (a build with no header
// resolves to this), mirroring E2B's NewHeader synthesizing a single full-size mapping when none exists.
func SingleBuildMapping(size uint64, owner string) Mapping {
	if size == 0 {
		return nil
	}
	return Mapping{{Offset: 0, Length: size, BuildStorageOffset: 0, Owner: owner}}
}

// BuildDiff compares child against base block by block over [0,size) and writes the COW diff of child:
// a changed, non-empty block is appended to out (the child's diff object) and recorded as a run owned by
// childOwner; a changed but all-zero block is recorded as a zero-owner ("") run (no bytes written --
// it overrides the base with zeros, E2B's empty-block rule); an unchanged block is omitted (it resolves
// to the base in MergeMappings). It returns the child's diff mapping (changed runs only, ascending,
// non-overlapping). A non-positive blockSize uses DefaultBlockSize.
//
// The "child != base" test is our build-time analogue of E2B's runtime dirty bit (STAGE18_DESIGN.md
// Decision 2); combining the compare with run-coalescing here is the single-machine equivalent of E2B's
// writeDiff + CreateMapping. The caller flattens onto the base via MergeMappings(baseMapping, diff).
// diffAccumulator coalesces a block-by-block COW classification into a diff Mapping: child-owned runs
// (their bytes concatenated in ascending order in the caller's diff object, tracked by storageOff),
// zero-owner runs (no bytes stored), and gaps where a block is unchanged. It is the single place the
// run/coalescing/storage-offset contract lives, shared by BuildDiff (a base-vs-child content compare)
// and MappingFromDirty (a per-block dirty set), so the two paths always produce identical mappings.
type diffAccumulator struct {
	mapping    Mapping
	storageOff uint64    // child-owned bytes emitted so far = the next child-owned run's storage offset
	cur        *BuildMap // the open run, or nil
}

// unchanged ends any open run: an unchanged block resolves to the base, so it is omitted from the diff.
func (a *diffAccumulator) unchanged() {
	if a.cur != nil {
		a.mapping = append(a.mapping, *a.cur)
		a.cur = nil
	}
}

// add records a changed block [off,off+n) owned by owner ("" = zero-owner, no bytes stored): it extends
// the open run when the owner matches, else flushes and starts a new one. A child-owned block advances
// storageOff, because the caller writes its bytes to the diff object in this same ascending order.
func (a *diffAccumulator) add(off, n int64, owner string) {
	if a.cur == nil || a.cur.Owner != owner {
		a.unchanged() // flush the open run
		start := uint64(0)
		if owner != "" {
			start = a.storageOff
		}
		a.cur = &BuildMap{Offset: uint64(off), Length: uint64(n), BuildStorageOffset: start, Owner: owner}
	} else {
		a.cur.Length += uint64(n)
	}
	if owner != "" {
		a.storageOff += uint64(n)
	}
}

// finish flushes the last open run and returns the accumulated mapping.
func (a *diffAccumulator) finish() Mapping {
	a.unchanged()
	return a.mapping
}

func BuildDiff(base, child io.ReaderAt, size, blockSize int64, childOwner string, out io.Writer) (Mapping, error) {
	bs := blockSize
	if bs <= 0 {
		bs = int64(DefaultBlockSize)
	}
	childBuf := make([]byte, bs)
	baseBuf := make([]byte, bs)
	var acc diffAccumulator
	for off := int64(0); off < size; off += bs {
		n := bs
		if off+n > size {
			n = size - off
		}
		cb := childBuf[:n]
		if err := readBlock(child, cb, off); err != nil {
			return nil, fmt.Errorf("read child at %d: %w", off, err)
		}
		bb := baseBuf[:n]
		if err := readBlock(base, bb, off); err != nil {
			return nil, fmt.Errorf("read base at %d: %w", off, err)
		}
		switch {
		case equalBytes(cb, bb):
			acc.unchanged() // resolves to the base
		case isZero(cb):
			acc.add(off, n, "") // changed to zero: a zero-owner override, no bytes stored
		default:
			acc.add(off, n, childOwner)
			if _, err := out.Write(cb); err != nil {
				return nil, fmt.Errorf("write diff block at %d: %w", off, err)
			}
		}
	}
	return acc.finish(), nil
}

// MappingFromDirty builds a COW diff mapping identical in form to BuildDiff's, but from a per-block
// dirty/empty classification instead of a base-vs-child content compare -- the input the Stage-22 layer
// producer gets from the writable rootfs overlay's ExportToDiff (block.Cache): a dirty non-empty block
// is a run owned by childOwner (its bytes are the caller's diff object, the dirty non-empty blocks
// concatenated in ascending block order), a dirty all-zero block is a zero-owner ("") run (no bytes
// stored), and an unwritten block is omitted (it resolves to the base in MergeMappings). dirty and empty
// are per-block flags; size is the logical image size; a non-positive blockSize uses DefaultBlockSize.
// This is the disk-diff twin of BuildDiff for the one-run-two-diffs producer -- see docs/STAGE22_DESIGN.md.
func MappingFromDirty(dirty, empty []bool, blockSize, size int64, childOwner string) Mapping {
	bs := blockSize
	if bs <= 0 {
		bs = int64(DefaultBlockSize)
	}
	var acc diffAccumulator
	for b := 0; b < len(dirty); b++ {
		off := int64(b) * bs
		if off >= size {
			break
		}
		n := bs
		if off+n > size {
			n = size - off
		}
		switch {
		case !dirty[b]:
			acc.unchanged()
		case b < len(empty) && empty[b]:
			acc.add(off, n, "")
		default:
			acc.add(off, n, childOwner)
		}
	}
	return acc.finish()
}

// readBlock fills p from ra at off, tolerating a short read at the object's end (the tail is left as the
// caller pre-sized it). It zeroes p first so a base shorter than the child reads as zeros past its end.
func readBlock(ra io.ReaderAt, p []byte, off int64) error {
	clear(p)
	if _, err := ra.ReadAt(p, off); err != nil && err != io.EOF {
		return err // a short read at end (io.EOF, e.g. a base shorter than the child) leaves the tail zero
	}
	return nil
}

// MergeMappings flattens diffMapping onto baseMapping: where a diff run overlaps a base run, the diff
// wins (its owner/offset) and the base run is split into the non-overlapped remainders (which keep the
// base owner, with BuildStorageOffset shifted by the trimmed prefix). baseMapping must cover the whole
// size; the result also covers it. Ported from E2B's mapping.go MergeMappings (owner as a string, not a
// uuid). Done at build time so a layer chain collapses into one mapping -- the boot path never recurses.
func MergeMappings(baseMapping, diffMapping Mapping) Mapping {
	if len(diffMapping) == 0 {
		return baseMapping
	}
	// Work on a copy: the algorithm rewrites a base entry in place when a diff splits it (the right
	// remainder is re-examined against the next diff).
	base := make(Mapping, len(baseMapping))
	copy(base, baseMapping)

	out := make(Mapping, 0, len(base)+len(diffMapping))
	var bi, di int
	for bi < len(base) && di < len(diffMapping) {
		b := base[bi]
		d := diffMapping[di]
		switch {
		case b.Length == 0:
			bi++
		case d.Length == 0:
			di++
		case b.Offset+b.Length <= d.Offset: // base entirely before diff
			out = append(out, b)
			bi++
		case d.Offset+d.Length <= b.Offset: // diff entirely before base
			out = append(out, d)
			di++
		case b.Offset >= d.Offset && b.Offset+b.Length <= d.Offset+d.Length: // base inside diff -> drop base
			bi++
		case d.Offset >= b.Offset && d.Offset+d.Length <= b.Offset+b.Length: // diff inside base -> split base
			if left := int64(d.Offset) - int64(b.Offset); left > 0 {
				out = append(out, BuildMap{Offset: b.Offset, Length: uint64(left), BuildStorageOffset: b.BuildStorageOffset, Owner: b.Owner})
			}
			out = append(out, d)
			di++
			shift := int64(d.Offset) + int64(d.Length) - int64(b.Offset)
			if right := int64(b.Length) - shift; right > 0 {
				base[bi] = BuildMap{Offset: b.Offset + uint64(shift), Length: uint64(right), BuildStorageOffset: b.BuildStorageOffset + uint64(shift), Owner: b.Owner}
			} else {
				bi++
			}
		case b.Offset > d.Offset: // base starts after diff, overlapping -> diff wins, keep base's right remainder
			out = append(out, d)
			di++
			shift := int64(d.Offset) + int64(d.Length) - int64(b.Offset)
			if right := int64(b.Length) - shift; right > 0 {
				base[bi] = BuildMap{Offset: b.Offset + uint64(shift), Length: uint64(right), BuildStorageOffset: b.BuildStorageOffset + uint64(shift), Owner: b.Owner}
			} else {
				bi++
			}
		case d.Offset > b.Offset: // diff starts after base, overlapping -> keep base's left remainder, then re-examine diff
			if left := int64(d.Offset) - int64(b.Offset); left > 0 {
				out = append(out, BuildMap{Offset: b.Offset, Length: uint64(left), BuildStorageOffset: b.BuildStorageOffset, Owner: b.Owner})
			}
			bi++
		default:
			// Unreachable for sorted, non-overlapping inputs; drop the base entry to make progress.
			bi++
		}
	}
	out = append(out, base[bi:]...)
	out = append(out, diffMapping[di:]...)
	return out
}

// NormalizeMappings coalesces adjacent runs that have the same owner AND are contiguous both logically
// (this run starts where the previous ends) and in storage (so a single range read of the owner's
// object still serves them). Zero-owner ("") runs coalesce on logical contiguity alone (no storage).
// This shrinks a mapping after a merge without ever fusing runs whose stored bytes are discontiguous --
// a stricter (correctness-safe) variant of E2B's NormalizeMappings.
func NormalizeMappings(mappings Mapping) Mapping {
	if len(mappings) == 0 {
		return nil
	}
	out := make(Mapping, 0, len(mappings))
	cur := mappings[0]
	for _, m := range mappings[1:] {
		logicalAdjacent := cur.Offset+cur.Length == m.Offset
		sameOwner := cur.Owner == m.Owner
		storageAdjacent := m.Owner == "" || cur.BuildStorageOffset+cur.Length == m.BuildStorageOffset
		if sameOwner && logicalAdjacent && storageAdjacent {
			cur.Length += m.Length
			continue
		}
		out = append(out, cur)
		cur = m
	}
	return append(out, cur)
}

// Locate resolves a logical offset to the run covering it: its owner ("" = zeros, served without a
// fetch), the storage offset within that owner's object, and the logical end of the run (so a reader can
// consume up to that boundary in one step). ok is false when off is outside [0,Size). An uncovered range
// (a gap, if the mapping is not full coverage) is reported as a zero run up to the next run or Size.
// Binary search over the ascending mapping -- the per-fault lookup Stage 19's layered memfile will use.
func (h Header) Locate(off int64) (owner string, storageOff, runEnd int64, ok bool) {
	if off < 0 || off >= int64(h.Metadata.Size) {
		return "", 0, 0, false
	}
	m := h.Mapping
	i := sort.Search(len(m), func(i int) bool { return int64(m[i].Offset) > off }) - 1
	if i >= 0 {
		e := m[i]
		if off < int64(e.Offset)+int64(e.Length) {
			runEnd := int64(e.Offset) + int64(e.Length)
			if e.Owner == "" { // an explicit zero run: zeros, no storage
				return "", 0, runEnd, true
			}
			return e.Owner, int64(e.BuildStorageOffset) + (off - int64(e.Offset)), runEnd, true
		}
	}
	next := int64(h.Metadata.Size)
	if j := i + 1; j < len(m) {
		next = int64(m[j].Offset)
	}
	return "", 0, next, true
}

// isZero reports whether block is all zero bytes (an absent/zero block, omitted from the compacted
// object). A simple early-exit scan -- clear over clever for a learning implementation.
func isZero(block []byte) bool {
	for _, c := range block {
		if c != 0 {
			return false
		}
	}
	return true
}

// equalBytes reports whether two equal-length blocks are byte-identical (the "unchanged vs base" test).
func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
