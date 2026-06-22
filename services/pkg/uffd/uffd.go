//go:build linux

// Package uffd lazily supplies a restored Firecracker microVM's guest RAM from its
// snapshot memory file, using Linux userfaultfd (the "become the memory supplier" trick).
//
// When a VM is restored with mem_backend.backend_type = "Uffd" (Stage 13), firecracker
// creates a userfaultfd, registers the guest RAM as MISSING, and hands us -- over a Unix
// domain socket, during PUT /snapshot/load -- the uffd file descriptor (via SCM_RIGHTS)
// plus the guest memory layout (a JSON array). From then on the first touch of any guest
// page faults out to this handler, which copies that page in from the memfile with
// UFFDIO_COPY. We become the guest's memory supplier. (Contrast the File backend, where
// the kernel demand-pages the mmap'd memfile with us on the outside.)
//
// On one machine this is not a latency win -- the point is to learn userfaultfd (the
// page-fault-interception primitive behind Firecracker, gVisor, CRIU and QEMU post-copy
// migration) and to make the page source pluggable: once *we* supply the pages, they need
// not come from a local file (object storage, a peer node, a shared cache). That is the
// precondition for the roadmap's "storage swaps go live" work. See docs/STAGE13_DESIGN.md.
//
// This is the ONLY package in the tree with raw ioctl / unsafe / mmap code (Decision 2):
// the Go stdlib (and x/sys/unix) ship the userfaultfd *syscall* but none of the UFFDIO_*
// ioctl request numbers, argument structs, or event tags, so we define them here from the
// kernel ABI (x86_64), deriving the ioctl numbers via the kernel's _IOWR macro rather than
// hand-typing magic. It is Linux-only (userfaultfd + epoll); the whole project is, but this
// is the one package that would not even compile elsewhere -- hence the build tag.
package uffd

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"
	"unsafe"
)

// ---- kernel ABI: userfaultfd ioctls, structs, event tags (x86_64) ----
//
// The kernel encodes an ioctl request number from a (direction, type, nr, size) tuple via
// the _IOC macro in <uapi/asm-generic/ioctl.h>. We reproduce just enough of it to derive
// UFFDIO_COPY / UFFDIO_ZEROPAGE, so the constants read as their kernel definition (and a
// unit test pins them to the known values) rather than as opaque hex.
const (
	iocNRBits   = 8
	iocTypeBits = 8
	iocSizeBits = 14

	iocNRShift   = 0
	iocTypeShift = iocNRShift + iocNRBits     // 8
	iocSizeShift = iocTypeShift + iocTypeBits // 16
	iocDirShift  = iocSizeShift + iocSizeBits // 30

	iocWrite = 1
	iocRead  = 2

	uffdioType = 0xAA // the userfaultfd ioctl "magic" (the kernel's UFFDIO group)
)

// iowr computes _IOWR(type, nr, size): a read+write ioctl request number. uintptr because
// it is passed straight to ioctl(2) as the request argument.
func iowr(typ, nr, size uintptr) uintptr {
	return (iocRead|iocWrite)<<iocDirShift |
		typ<<iocTypeShift |
		nr<<iocNRShift |
		size<<iocSizeShift
}

// The two ioctls this handler issues. The size in each request number is sizeof the kernel
// argument struct below (40 / 32), which on x86_64 yields 0xC028AA03 / 0xC020AA04.
var (
	uffdioCopyOp     = iowr(uffdioType, 3, unsafe.Sizeof(uffdioCopy{}))
	uffdioZeropageOp = iowr(uffdioType, 4, unsafe.Sizeof(uffdioZeropage{}))
)

// uffdioCopy mirrors the kernel's struct uffdio_copy -- the UFFDIO_COPY argument. The
// handler fills dst (the faulting page, page-aligned), src (where the bytes live in our
// memfile mmap) and len (one page); the kernel writes copy (bytes copied, or -errno).
type uffdioCopy struct {
	dst  uint64
	src  uint64
	len  uint64
	mode uint64
	copy int64
}

// uffdioRange is struct uffdio_range: a [start, start+len) guest range.
type uffdioRange struct {
	start uint64
	len   uint64
}

// uffdioZeropage mirrors struct uffdio_zeropage -- the UFFDIO_ZEROPAGE argument, used to
// hand a REMOVE'd range back as zero pages (Decision 4). The kernel writes zeropage.
type uffdioZeropage struct {
	rng      uffdioRange
	mode     uint64
	zeropage int64
}

// userfaultfd event tags -- the first byte of struct uffd_msg. We handle PAGEFAULT (lazy
// populate) and REMOVE (a range was madvise'd away -> serve it as zeros, Decision 4).
const (
	uffdEventPagefault = 0x12
	uffdEventRemove    = 0x9
)

// struct uffd_msg is 32 bytes, __packed: a 1-byte event + 7 reserved, then a 24-byte union.
// We read it as raw bytes and pull fields by offset (x86_64 is little-endian). Getting these
// offsets right is the #1 ABI failure mode, so they are named and unit-tested.
const (
	sizeofUffdMsg    = 32
	msgEventOffset   = 0  // event tag
	msgAddressOffset = 16 // pagefault.address (after the 8-byte header + u64 flags)
	msgRemoveStart   = 8  // remove.start
	msgRemoveEnd     = 16 // remove.end
)

// GuestRegion is one entry of the guest memory layout firecracker sends (a JSON array) right
// after the uffd fd. Field names are pinned to firecracker v1.16.0
// (src/firecracker/examples/uffd/uffd_utils.rs); note page_size is in BYTES in 1.16.0.
type GuestRegion struct {
	BaseHostVirtAddr uint64 `json:"base_host_virt_addr"`
	Size             uint64 `json:"size"`
	Offset           uint64 `json:"offset"`
	PageSize         uint64 `json:"page_size"`
}

// parseRegions unmarshals firecracker's mappings handshake body. An empty array means we got
// no layout to serve from -- a protocol error, not a VM with no memory.
func parseRegions(body []byte) ([]GuestRegion, error) {
	var regions []GuestRegion
	if err := json.Unmarshal(body, &regions); err != nil {
		return nil, fmt.Errorf("parse uffd mappings %q: %w", body, err)
	}
	if len(regions) == 0 {
		return nil, fmt.Errorf("uffd mappings empty")
	}
	return regions, nil
}

// resolveFault maps a faulting guest address to the page to copy in: the page-aligned
// destination (the UFFDIO_COPY dst) and the byte offset of that page in the memfile (the
// src), plus the region's page size. ok is false if no region contains the address -- a
// fault we cannot serve (the guest vCPU would hang on it; it signals a mappings/math bug).
//
//	memfile offset = region.Offset + (alignedAddr - region.BaseHostVirtAddr)
func resolveFault(regions []GuestRegion, addr uint64) (alignedAddr, memOffset, pageSize uint64, ok bool) {
	for _, r := range regions {
		if addr >= r.BaseHostVirtAddr && addr < r.BaseHostVirtAddr+r.Size {
			aligned := addr &^ (r.PageSize - 1) // round down to the page boundary
			return aligned, r.Offset + (aligned - r.BaseHostVirtAddr), r.PageSize, true
		}
	}
	return 0, 0, 0, false
}

// faultAddr / removeRange read the union fields of a uffd_msg at their kernel offsets.
func faultAddr(msg []byte) uint64 { return binary.LittleEndian.Uint64(msg[msgAddressOffset:]) }
func removeRange(msg []byte) (start, end uint64) {
	return binary.LittleEndian.Uint64(msg[msgRemoveStart:]), binary.LittleEndian.Uint64(msg[msgRemoveEnd:])
}

// Handler owns the resources of one VM's page-fault service: the listening socket, the
// read-only mmap of the memfile, the stop pipe, and the serve goroutine. Its lifetime is
// bound to the MicroVM -- pkg/fc holds it and calls Stop in Destroy (Stage 13b).
type Handler struct {
	udsPath  string
	listener net.Listener

	stopOnce sync.Once
	stopW    int           // write end of the stop pipe; a byte here wakes the serve loop's epoll
	done     chan struct{} // closed once the serve goroutine has fully exited (mem unmapped, fds closed)

	mu  sync.Mutex
	err error // first fatal error from the serve goroutine
}

// Serve binds a Unix domain socket at udsPath and mmaps memfilePath read-only, then spawns a
// goroutine that accepts firecracker's one connection, receives the uffd fd + guest layout,
// and serves page faults from the memfile until Stop (or the VM dies). It returns as soon as
// the socket is listening: the caller MUST call Serve *before* firecracker's PUT
// /snapshot/load, which connects to this socket during the load (a hard ordering requirement).
func Serve(udsPath, memfilePath string) (*Handler, error) {
	// Map the memfile read-only -- the source bytes for every page we copy in. Do it up front
	// so a missing/empty memfile fails here, before firecracker's snapshot load.
	mem, err := mmapFile(memfilePath)
	if err != nil {
		return nil, err
	}

	// The stop pipe: Stop() writes a byte to stopW to wake the serve loop's epoll (Decision 5).
	var p [2]int
	if err := syscall.Pipe(p[:]); err != nil {
		unmap(mem)
		return nil, fmt.Errorf("uffd stop pipe: %w", err)
	}
	stopR, stopW := p[0], p[1]

	// Clear any stale socket from a crashed run, then listen before returning.
	if err := os.Remove(udsPath); err != nil && !os.IsNotExist(err) {
		closeStop(stopR, stopW)
		unmap(mem)
		return nil, fmt.Errorf("clear stale uffd socket %s: %w", udsPath, err)
	}
	l, err := net.Listen("unix", udsPath)
	if err != nil {
		closeStop(stopR, stopW)
		unmap(mem)
		return nil, fmt.Errorf("listen uffd uds %s: %w", udsPath, err)
	}

	h := &Handler{udsPath: udsPath, listener: l, stopW: stopW, done: make(chan struct{})}
	go h.serve(mem, stopR)
	return h, nil
}

// Stop tears the handler down deterministically: it unblocks the goroutine whichever phase it
// is in, waits for it to exit (so the memfile is munmap'd and fds closed -- no leaks across the
// warm pool's churn), then closes the stop pipe and removes the socket. Called once, from Destroy.
func (h *Handler) Stop() {
	h.stopOnce.Do(func() {
		// Closing the listener unblocks a pending Accept (firecracker never connected); a byte
		// on the stop pipe wakes the epoll loop (already past Accept). Do both -- we don't know
		// which phase the goroutine is in.
		_ = h.listener.Close()
		var b [1]byte
		_, _ = syscall.Write(h.stopW, b[:])
		<-h.done
		_ = syscall.Close(h.stopW)
		_ = os.Remove(h.udsPath) // best-effort; listener.Close usually already removed it
	})
}

// Err returns the first fatal error from the serve goroutine, or nil if it is running /
// stopped cleanly. pkg/fc surfaces a non-nil Err as a restore failure (Decision 3).
func (h *Handler) Err() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.err
}

func (h *Handler) fail(err error) {
	h.mu.Lock()
	if h.err == nil {
		h.err = err
	}
	h.mu.Unlock()
}

// serve is the goroutine: receive the handshake, then serve faults until stopped or the VM
// dies. Cleanup runs LIFO on exit, with done closed last so a waiter in Stop sees a fully
// torn-down handler.
func (h *Handler) serve(mem []byte, stopR int) {
	defer close(h.done)
	defer unmap(mem)
	defer func() { _ = syscall.Close(stopR) }()

	uffdFD, regions, err := h.recvHandshake()
	if err != nil {
		h.fail(err)
		return
	}
	defer func() { _ = syscall.Close(uffdFD) }()

	if err := faultLoop(uffdFD, stopR, mem, regions); err != nil {
		h.fail(err)
	}
}

// recvHandshake accepts firecracker's one connection and reads, in a single message, the
// guest layout (JSON body) and the uffd fd (SCM_RIGHTS ancillary data). After this the socket
// is done -- all further interaction is on the uffd fd -- so the connection is closed.
func (h *Handler) recvHandshake() (int, []GuestRegion, error) {
	conn, err := h.listener.Accept()
	if err != nil {
		return -1, nil, fmt.Errorf("accept uffd uds: %w", err)
	}
	defer conn.Close()
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return -1, nil, fmt.Errorf("uffd uds: not a unix connection")
	}

	// firecracker sends the layout in one write; a few-KB buffer covers any realistic region
	// count (a VM has a handful of regions). oob holds exactly the one fd it passes.
	body := make([]byte, 8192)
	oob := make([]byte, syscall.CmsgSpace(4))
	n, oobn, _, _, err := uc.ReadMsgUnix(body, oob)
	if err != nil {
		return -1, nil, fmt.Errorf("recv uffd handshake: %w", err)
	}
	fd, err := parseOneFD(oob[:oobn])
	if err != nil {
		return -1, nil, err
	}
	regions, err := parseRegions(body[:n])
	if err != nil {
		_ = syscall.Close(fd)
		return -1, nil, err
	}
	return fd, regions, nil
}

// parseOneFD extracts the single file descriptor firecracker passes as SCM_RIGHTS ancillary
// data (the uffd). Exactly one fd is expected; none or more is a protocol error.
func parseOneFD(oob []byte) (int, error) {
	scms, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return -1, fmt.Errorf("parse uffd control message: %w", err)
	}
	if len(scms) != 1 {
		return -1, fmt.Errorf("uffd handshake: %d control messages, want 1", len(scms))
	}
	fds, err := syscall.ParseUnixRights(&scms[0])
	if err != nil {
		return -1, fmt.Errorf("parse uffd rights: %w", err)
	}
	if len(fds) != 1 {
		return -1, fmt.Errorf("uffd handshake: %d fds, want 1", len(fds))
	}
	return fds[0], nil
}

// faultLoop waits on the uffd fd and the stop pipe together (epoll), serving each page fault
// off the uffd fd. It returns nil on a clean stop (Stop fired, or the uffd hung up because
// firecracker exited) and an error on an unexpected failure.
func faultLoop(uffdFD, stopR int, mem []byte, regions []GuestRegion) error {
	epfd, err := syscall.EpollCreate1(0)
	if err != nil {
		return fmt.Errorf("epoll_create1: %w", err)
	}
	defer func() { _ = syscall.Close(epfd) }()

	for _, fd := range []int{uffdFD, stopR} {
		ev := syscall.EpollEvent{Events: syscall.EPOLLIN, Fd: int32(fd)}
		if err := syscall.EpollCtl(epfd, syscall.EPOLL_CTL_ADD, fd, &ev); err != nil {
			return fmt.Errorf("epoll_ctl add fd %d: %w", fd, err)
		}
	}

	events := make([]syscall.EpollEvent, 2)
	msg := make([]byte, sizeofUffdMsg)
	for {
		n, err := syscall.EpollWait(epfd, events, -1)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			return fmt.Errorf("epoll_wait: %w", err)
		}
		for i := 0; i < n; i++ {
			switch events[i].Fd {
			case int32(stopR):
				return nil // Stop() asked us to exit
			case int32(uffdFD):
				// A hangup on the uffd means firecracker exited -> the VM is gone (Decision 5).
				if events[i].Events&(syscall.EPOLLHUP|syscall.EPOLLERR) != 0 {
					return nil
				}
				stop, err := readAndServe(uffdFD, msg, mem, regions)
				if err != nil {
					return err
				}
				if stop {
					return nil
				}
			}
		}
	}
}

// readAndServe reads one fault message off the uffd fd and serves it. stop is true on EOF
// (firecracker gone). Level-triggered epoll re-wakes us if more messages are queued, so we
// read exactly one per wake (simple over batched -- a learning implementation).
func readAndServe(uffdFD int, msg, mem []byte, regions []GuestRegion) (stop bool, err error) {
	n, err := syscall.Read(uffdFD, msg)
	if err != nil {
		if err == syscall.EINTR || err == syscall.EAGAIN {
			return false, nil
		}
		return false, fmt.Errorf("read uffd: %w", err)
	}
	if n == 0 {
		return true, nil // EOF: the VM is gone
	}
	if n != sizeofUffdMsg {
		return false, fmt.Errorf("short uffd message: %d bytes (want %d)", n, sizeofUffdMsg)
	}
	return false, handleEvent(uffdFD, msg, mem, regions)
}

// handleEvent dispatches one uffd_msg by its event tag.
func handleEvent(uffdFD int, msg, mem []byte, regions []GuestRegion) error {
	switch msg[msgEventOffset] {
	case uffdEventPagefault:
		return serveFault(uffdFD, mem, regions, faultAddr(msg))
	case uffdEventRemove:
		start, end := removeRange(msg)
		return serveRemove(uffdFD, start, end)
	default:
		return nil // events we didn't register for (FORK/REMAP/UNMAP); ignore
	}
}

// serveFault copies the faulting page in from the memfile via UFFDIO_COPY, waking the blocked
// vCPU. Benign races are absorbed: EEXIST means another fault already populated the page;
// EAGAIN means the mapping changed under us (e.g. a concurrent REMOVE) -- hand back zeros.
func serveFault(uffdFD int, mem []byte, regions []GuestRegion, addr uint64) error {
	alignedAddr, memOffset, pageSize, ok := resolveFault(regions, addr)
	if !ok {
		return fmt.Errorf("page fault at %#x outside all guest regions", addr)
	}
	if memOffset+pageSize > uint64(len(mem)) {
		return fmt.Errorf("copy src [%d,%d) past memfile (%d bytes)", memOffset, memOffset+pageSize, len(mem))
	}

	// src points into the mmap'd memfile, which is off-heap and never moved/collected by the
	// GC while mem stays reachable -- so holding its address as an integer here is safe.
	arg := uffdioCopy{
		dst: alignedAddr,
		src: uint64(uintptr(unsafe.Pointer(&mem[memOffset]))),
		len: pageSize,
	}
	err := ioctl(uffdFD, uffdioCopyOp, unsafe.Pointer(&arg))
	// The kernel reports a mid-copy failure either as the ioctl errno or in arg.copy (-errno).
	if err == nil && arg.copy < 0 {
		err = syscall.Errno(-arg.copy)
	}
	switch err {
	case nil:
		return nil
	case syscall.EEXIST:
		return nil // already populated by a racing fault -- fine
	case syscall.EAGAIN:
		return serveRemove(uffdFD, alignedAddr, alignedAddr+pageSize) // mapping changed -> zero it
	default:
		return fmt.Errorf("UFFDIO_COPY @ %#x: %w", alignedAddr, err)
	}
}

// serveRemove zeroes a guest range via UFFDIO_ZEROPAGE, so a later fault on a removed page
// gets a zero page, not stale memfile bytes (Decision 4).
func serveRemove(uffdFD int, start, end uint64) error {
	arg := uffdioZeropage{rng: uffdioRange{start: start, len: end - start}}
	if err := ioctl(uffdFD, uffdioZeropageOp, unsafe.Pointer(&arg)); err != nil {
		return fmt.Errorf("UFFDIO_ZEROPAGE [%#x,%#x): %w", start, end, err)
	}
	return nil
}

// ioctl issues one ioctl(fd, op, arg); arg points at the kernel argument struct. The uintptr
// conversion is inline in the Syscall call so the pointer stays live across it (the Go unsafe rule).
func ioctl(fd int, op uintptr, arg unsafe.Pointer) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), op, uintptr(arg)); errno != 0 {
		return errno
	}
	return nil
}

// mmapFile maps a file read-only (MAP_PRIVATE): we only read its bytes as UFFDIO_COPY sources.
func mmapFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open memfile: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat memfile: %w", err)
	}
	size := int(fi.Size())
	if size == 0 {
		return nil, fmt.Errorf("memfile %s is empty", path)
	}
	mem, err := syscall.Mmap(int(f.Fd()), 0, size, syscall.PROT_READ, syscall.MAP_PRIVATE)
	if err != nil {
		return nil, fmt.Errorf("mmap memfile %s: %w", path, err)
	}
	return mem, nil
}

func unmap(mem []byte) {
	if len(mem) > 0 {
		_ = syscall.Munmap(mem)
	}
}

func closeStop(stopR, stopW int) {
	_ = syscall.Close(stopR)
	_ = syscall.Close(stopW)
}
