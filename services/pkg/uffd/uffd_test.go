//go:build linux

package uffd

// KVM-free unit tests for the pure parts of the userfaultfd handler: the ioctl-number
// derivation, the argument-struct sizes those numbers encode, the mappings-JSON parse, the
// fault-address -> memfile-offset math, and the uffd_msg field offsets. The syscall-heavy
// serve loop (mmap, recvmsg, epoll, the ioctls) is exercised only on a real VM in Stage 13b.

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"unsafe"
)

// The ioctl request numbers must equal the known x86_64 kernel values -- a wrong number means
// firecracker's uffd would reject our copy. Derived from _IOWR(0xAA, nr, sizeof(arg)).
func TestIoctlNumbersMatchKernelABI(t *testing.T) {
	if got := uffdioCopyOp; got != 0xC028AA03 {
		t.Errorf("UFFDIO_COPY = %#x, want 0xC028AA03", got)
	}
	if got := uffdioZeropageOp; got != 0xC020AA04 {
		t.Errorf("UFFDIO_ZEROPAGE = %#x, want 0xC020AA04", got)
	}
}

// The size baked into each ioctl number is sizeof the argument struct; the Go structs must
// match the kernel's byte layout exactly (40 / 32) or the kernel reads the wrong fields.
func TestArgStructSizes(t *testing.T) {
	if got := unsafe.Sizeof(uffdioCopy{}); got != 40 {
		t.Errorf("sizeof uffdio_copy = %d, want 40", got)
	}
	if got := unsafe.Sizeof(uffdioZeropage{}); got != 32 {
		t.Errorf("sizeof uffdio_zeropage = %d, want 32", got)
	}
}

// parseRegions decodes the exact handshake body firecracker v1.16.0 sends (page_size in BYTES).
func TestParseRegions(t *testing.T) {
	body := []byte(`[{"base_host_virt_addr":140737488355328,"size":134217728,"offset":0,"page_size":4096}]`)
	regions, err := parseRegions(body)
	if err != nil {
		t.Fatalf("parseRegions: %v", err)
	}
	if len(regions) != 1 {
		t.Fatalf("got %d regions, want 1", len(regions))
	}
	if r := regions[0]; r.BaseHostVirtAddr != 140737488355328 || r.Size != 134217728 || r.Offset != 0 || r.PageSize != 4096 {
		t.Errorf("region = %+v", r)
	}
}

func TestParseRegionsRejectsEmptyAndGarbage(t *testing.T) {
	if _, err := parseRegions([]byte(`[]`)); err == nil {
		t.Error("parseRegions([]) = nil error, want error (no layout to serve)")
	}
	if _, err := parseRegions([]byte(`not json`)); err == nil {
		t.Error("parseRegions(garbage) = nil error, want error")
	}
}

// resolveFault: a single 128 MiB region at a high base, page size 4096.
func TestResolveFault(t *testing.T) {
	const base, size = 0x7f0000000000, 0x8000000
	regions := []GuestRegion{{BaseHostVirtAddr: base, Size: size, Offset: 0, PageSize: 4096}}
	cases := []struct {
		name                    string
		addr                    uint64
		wantAligned, wantOffset uint64
		wantOK                  bool
	}{
		{"region base", base, base, 0, true},
		{"second page", base + 0x1000, base + 0x1000, 0x1000, true},
		{"unaligned rounds down", base + 0x1abc, base + 0x1000, 0x1000, true},
		{"last byte of region", base + size - 1, base + size - 0x1000, size - 0x1000, true},
		{"just past region", base + size, 0, 0, false},
		{"below region", base - 1, 0, 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			aligned, off, ps, ok := resolveFault(regions, c.addr)
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if !ok {
				return
			}
			if aligned != c.wantAligned {
				t.Errorf("aligned = %#x, want %#x", aligned, c.wantAligned)
			}
			if off != c.wantOffset {
				t.Errorf("memfile offset = %#x, want %#x", off, c.wantOffset)
			}
			if ps != 4096 {
				t.Errorf("pageSize = %d, want 4096", ps)
			}
		})
	}
}

// A second region whose bytes live further into the memfile (nonzero Offset): the computed
// memfile offset must use that region's own Offset, not a flat address delta from region 0.
func TestResolveFaultMultiRegionUsesRegionOffset(t *testing.T) {
	regions := []GuestRegion{
		{BaseHostVirtAddr: 0x10000, Size: 0x2000, Offset: 0, PageSize: 4096},
		{BaseHostVirtAddr: 0x20000, Size: 0x2000, Offset: 0x2000, PageSize: 4096},
	}
	aligned, off, _, ok := resolveFault(regions, 0x21000)
	if !ok {
		t.Fatal("ok = false, want true (address is in region 1)")
	}
	if aligned != 0x21000 {
		t.Errorf("aligned = %#x, want 0x21000", aligned)
	}
	if off != 0x3000 { // region.Offset(0x2000) + (0x21000 - 0x20000)
		t.Errorf("memfile offset = %#x, want 0x3000", off)
	}
}

// The union fields must be read at the right byte offsets -- the #1 ABI failure mode. Build a
// uffd_msg by hand and confirm faultAddr / removeRange pull the values we wrote.
func TestMessageFieldOffsets(t *testing.T) {
	pf := make([]byte, sizeofUffdMsg)
	binary.LittleEndian.PutUint64(pf[msgAddressOffset:], 0xdeadbeef000)
	if got := faultAddr(pf); got != 0xdeadbeef000 {
		t.Errorf("faultAddr = %#x, want 0xdeadbeef000", got)
	}

	rm := make([]byte, sizeofUffdMsg)
	binary.LittleEndian.PutUint64(rm[msgRemoveStart:], 0x1000)
	binary.LittleEndian.PutUint64(rm[msgRemoveEnd:], 0x3000)
	if start, end := removeRange(rm); start != 0x1000 || end != 0x3000 {
		t.Errorf("removeRange = (%#x, %#x), want (0x1000, 0x3000)", start, end)
	}
}

// MmapSource is the page supply the handler used directly before Stage 15a's PageSource refactor
// (now one impl behind the interface). This exercises it without a VM: a plain file mmap needs no
// KVM/privilege. We check ReadAt returns the right page bytes and that readPage turns a page past
// the file end into an error -- the bounds check that used to live inline in serveFault.
func TestMmapSourceReadPage(t *testing.T) {
	const pageSize = 4096
	// A 3-page file where page i is filled with the byte value i, so a read is recognizable.
	data := make([]byte, 3*pageSize)
	for i := 0; i < 3; i++ {
		for j := 0; j < pageSize; j++ {
			data[i*pageSize+j] = byte(i)
		}
	}
	path := filepath.Join(t.TempDir(), "memfile")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write memfile: %v", err)
	}
	src, err := MmapSource(path)
	if err != nil {
		t.Fatalf("MmapSource: %v", err)
	}
	defer src.Close()

	// Middle page (offset 1*pageSize): every byte must be 1.
	page := make([]byte, pageSize)
	if err := readPage(src, page, pageSize); err != nil {
		t.Fatalf("readPage(page 1): %v", err)
	}
	for i, b := range page {
		if b != 1 {
			t.Fatalf("page 1 byte %d = %d, want 1", i, b)
		}
	}
	// A page starting at the file end is a short read -> an error (the old serveFault bounds check).
	if err := readPage(src, page, 3*pageSize); err == nil {
		t.Error("readPage past end = nil, want an error")
	}
}
