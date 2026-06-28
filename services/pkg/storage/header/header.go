// Package header is the memfile index: a per-block map of which logical ranges of a snapshot's
// memory file actually hold data, so the builder can store only those (compaction) and the boot path
// can serve the rest as zeros without a fetch. It mirrors E2B's packages/shared/pkg/storage/header
// (Metadata + an ordered Mapping of BuildMap entries; a logical offset resolves to a storage offset,
// and unmapped gaps are zeros that are never read). See docs/STAGE17_DESIGN.md.
//
// Scope of this stage (single flat build): we keep Version/BlockSize/Size and an ordered mapping of
// present runs, but DROP E2B's per-entry BuildId / BaseBuildId (which build owns a range) -- with one
// build per memfile every present block belongs to that build. The Version field lets copy-on-write
// layered builds add the owner later without a flag day (Stage 17 §10 item 3). No compression yet.
//
// The package is pure (encoding/binary + io + os): no network, no storage/minio import, no KVM. That
// keeps it a leaf both the producers (pkg/build, cmd/msb-seed) and the boot path (cmd/orchestrator)
// can depend on, and fully unit-testable.
package header

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Version is the on-disk format version. Compression / COW layers bump it (the reader rejects
// versions it does not understand, so an old orchestrator never misreads a newer header).
const Version uint64 = 1

// DefaultBlockSize is the index granularity in bytes: the guest base page size, so each faulting page
// maps to exactly one block (wholly zero or wholly present -- see docs/STAGE17_DESIGN.md, Decision 3).
const DefaultBlockSize uint64 = 4096

// Metadata is the fixed-size head of a serialized header. E2B also carries Generation/BuildId/
// BaseBuildId (uuid); dropped until COW (package doc).
type Metadata struct {
	Version   uint64
	BlockSize uint64 // index granularity in bytes
	Size      uint64 // total LOGICAL size of the indexed memfile (gaps up to Size read as zero)
}

// BuildMap maps one present (non-zero) logical run of the memfile to its bytes in the compacted
// object. Mirrors E2B's BuildMap minus BuildId (the owning build, added with COW).
type BuildMap struct {
	Offset             uint64 // logical byte offset of the run (block-aligned)
	Length             uint64 // run length in bytes (block-multiple, except possibly a short final block)
	BuildStorageOffset uint64 // byte offset of this run inside the compacted {buildID}/memfile object
}

// Mapping is the ordered (ascending Offset), non-overlapping list of present runs.
type Mapping = []BuildMap

// Header is a parsed memfile index: its metadata plus the present-run mapping.
type Header struct {
	Metadata Metadata
	Mapping  Mapping
}

// metadataWords is the count of uint64 fields in the serialized Metadata, then the mapping length
// counter; mapWords is the count per BuildMap entry. Serialization is fixed-width little-endian.
const (
	metadataWords = 3 // Version, BlockSize, Size
	headWords     = metadataWords + 1
	mapWords      = 3 // Offset, Length, BuildStorageOffset
	wordSize      = 8
	headSize      = headWords * wordSize
	mapEntrySize  = mapWords * wordSize
)

// Serialize encodes the header as Metadata ‖ count ‖ mapping[] (every field a little-endian uint64),
// self-describing and fixed-width -- the same hand-rolled binary discipline pkg/uffd uses for the
// kernel ABI structs (no external schema, no new dependency).
func (h Header) Serialize() []byte {
	b := make([]byte, 0, headSize+len(h.Mapping)*mapEntrySize)
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

// Deserialize parses bytes produced by Serialize. It rejects a short buffer, an unknown version, and
// a mapping-length that does not match the trailing bytes -- so a truncated or wrong-format header
// fails loudly at boot, not as a silent mis-read mid-fault.
func Deserialize(b []byte) (Header, error) {
	if len(b) < headSize {
		return Header{}, fmt.Errorf("header too short: %d bytes (want >= %d)", len(b), headSize)
	}
	md := Metadata{
		Version:   binary.LittleEndian.Uint64(b[0:]),
		BlockSize: binary.LittleEndian.Uint64(b[8:]),
		Size:      binary.LittleEndian.Uint64(b[16:]),
	}
	if md.Version != Version {
		return Header{}, fmt.Errorf("unsupported header version %d (want %d)", md.Version, Version)
	}
	count := binary.LittleEndian.Uint64(b[24:])
	rest := b[headSize:]
	if uint64(len(rest)) != count*mapEntrySize {
		return Header{}, fmt.Errorf("header mapping length mismatch: %d entries declared, %d trailing bytes",
			count, len(rest))
	}
	mapping := make(Mapping, count)
	for i := range mapping {
		e := rest[i*mapEntrySize:]
		mapping[i] = BuildMap{
			Offset:             binary.LittleEndian.Uint64(e[0:]),
			Length:             binary.LittleEndian.Uint64(e[8:]),
			BuildStorageOffset: binary.LittleEndian.Uint64(e[16:]),
		}
	}
	return Header{Metadata: md, Mapping: mapping}, nil
}

// Build scans r (a memfile) in blockSize blocks, writing only the non-zero blocks to out (the
// compacted object) and recording each maximal run of present blocks as one BuildMap. It returns the
// header describing that compaction. A non-positive blockSize uses DefaultBlockSize.
//
// This is the single-build analogue of E2B's CreateMapping over a dirty bitmap: our "is this block
// non-zero" stands in for E2B's "did this block change vs the parent" -- same mapping shape, simpler
// source. Runs are coalesced, so a mostly-zero memfile yields a tiny mapping (one entry per island of
// data), and out holds only the present bytes.
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
