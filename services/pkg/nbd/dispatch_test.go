//go:build linux

package nbd

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
)

// bytesProvider is a Provider backed by an in-memory slice: the KVM-free stand-in for the real COW
// block stack, so the Dispatch protocol loop can be tested without the nbd module or a device. It is
// concurrency-safe (Dispatch may serve several sockets at once), though the single-conn dispatch tests
// do not rely on that. The reads/writes counters let the real-device test (realdevice_test.go) assert
// that a block round-trip actually reached the Provider through Dispatch, not just the page cache.
type bytesProvider struct {
	mu     sync.Mutex
	data   []byte
	reads  atomic.Int64
	writes atomic.Int64
}

func (p *bytesProvider) Size() int64 { return int64(len(p.data)) }

func (p *bytesProvider) ReadAt(b []byte, off int64) (int, error) {
	p.reads.Add(1)
	p.mu.Lock()
	defer p.mu.Unlock()
	if off < 0 || off >= int64(len(p.data)) {
		return 0, io.EOF
	}
	n := copy(b, p.data[off:])
	if n < len(b) {
		return n, io.EOF
	}
	return n, nil
}

func (p *bytesProvider) WriteAt(b []byte, off int64) (int, error) {
	p.writes.Add(1)
	p.mu.Lock()
	defer p.mu.Unlock()
	if off < 0 || off+int64(len(b)) > int64(len(p.data)) {
		return 0, io.ErrShortWrite
	}
	return copy(p.data[off:], b), nil
}

// nbdRequest frames a 28-byte NBD transmission request as the kernel would send it.
func nbdRequest(typ uint16, handle, offset uint64, length uint32) []byte {
	b := make([]byte, requestLen)
	binary.BigEndian.PutUint32(b[0:4], requestMagic)
	binary.BigEndian.PutUint16(b[4:6], 0) // command flags
	binary.BigEndian.PutUint16(b[6:8], typ)
	binary.BigEndian.PutUint64(b[8:16], handle)
	binary.BigEndian.PutUint64(b[16:24], offset)
	binary.BigEndian.PutUint32(b[24:28], length)
	return b
}

// readReplyHeader reads and validates a 16-byte simple reply, returning its error code and handle.
func readReplyHeader(t *testing.T, conn io.Reader) (uint32, uint64) {
	t.Helper()
	var h [simpleReplyLen]byte
	if _, err := io.ReadFull(conn, h[:]); err != nil {
		t.Fatalf("read reply header: %v", err)
	}
	if magic := binary.BigEndian.Uint32(h[0:4]); magic != simpleReplyMagic {
		t.Fatalf("bad reply magic %#x", magic)
	}
	return binary.BigEndian.Uint32(h[4:8]), binary.BigEndian.Uint64(h[8:16])
}

// startDispatch runs Dispatch over an in-memory pipe against provider, returning the kernel-side conn
// and a finish func that closes the pipe and yields Dispatch's return value.
func startDispatch(provider Provider) (kernel net.Conn, finish func() error) {
	server, kernel := net.Pipe()
	errc := make(chan error, 1)
	go func() { errc <- Dispatch(server, provider) }()
	return kernel, func() error {
		// Close only the peer: on net.Pipe, that makes Dispatch's blocked read on `server` return
		// io.EOF (a clean disconnect). Closing `server` here too would race that read and surface
		// ErrClosedPipe instead, so we wait for Dispatch to exit first, then close our end.
		_ = kernel.Close()
		err := <-errc
		_ = server.Close()
		return err
	}
}

func TestDispatchRead(t *testing.T) {
	data := []byte("0123456789abcdef")
	kernel, finish := startDispatch(&bytesProvider{data: append([]byte(nil), data...)})

	if _, err := kernel.Write(nbdRequest(cmdRead, 0xdeadbeef, 4, 8)); err != nil {
		t.Fatalf("write request: %v", err)
	}
	errCode, handle := readReplyHeader(t, kernel)
	if errCode != errNone {
		t.Fatalf("read errCode = %d, want 0", errCode)
	}
	if handle != 0xdeadbeef {
		t.Fatalf("handle = %#x, want 0xdeadbeef (must be echoed)", handle)
	}
	got := make([]byte, 8)
	if _, err := io.ReadFull(kernel, got); err != nil {
		t.Fatalf("read reply data: %v", err)
	}
	if !bytes.Equal(got, data[4:12]) {
		t.Fatalf("read data = %q, want %q", got, data[4:12])
	}
	if err := finish(); err != nil {
		t.Fatalf("dispatch returned %v, want nil", err)
	}
}

func TestDispatchWrite(t *testing.T) {
	p := &bytesProvider{data: make([]byte, 16)}
	kernel, finish := startDispatch(p)

	payload := []byte("WXYZ")
	req := append(nbdRequest(cmdWrite, 1, 2, uint32(len(payload))), payload...)
	if _, err := kernel.Write(req); err != nil {
		t.Fatalf("write request+payload: %v", err)
	}
	errCode, handle := readReplyHeader(t, kernel)
	if errCode != errNone || handle != 1 {
		t.Fatalf("write reply errCode=%d handle=%d, want 0/1", errCode, handle)
	}
	if err := finish(); err != nil {
		t.Fatalf("dispatch returned %v, want nil", err)
	}
	if !bytes.Equal(p.data[2:6], payload) {
		t.Fatalf("provider bytes [2:6] = %q, want %q", p.data[2:6], payload)
	}
}

func TestDispatchFlushAndTrimAck(t *testing.T) {
	kernel, finish := startDispatch(&bytesProvider{data: make([]byte, 8)})

	for _, typ := range []uint16{cmdFlush, cmdTrim} {
		if _, err := kernel.Write(nbdRequest(typ, 7, 0, 8)); err != nil {
			t.Fatalf("write request type %d: %v", typ, err)
		}
		if errCode, handle := readReplyHeader(t, kernel); errCode != errNone || handle != 7 {
			t.Fatalf("type %d reply errCode=%d handle=%d, want 0/7", typ, errCode, handle)
		}
	}
	if err := finish(); err != nil {
		t.Fatalf("dispatch returned %v, want nil", err)
	}
}

func TestDispatchUnknownCommandRejected(t *testing.T) {
	kernel, finish := startDispatch(&bytesProvider{data: make([]byte, 8)})

	if _, err := kernel.Write(nbdRequest(99, 3, 0, 0)); err != nil {
		t.Fatalf("write request: %v", err)
	}
	if errCode, _ := readReplyHeader(t, kernel); errCode != errInval {
		t.Fatalf("unknown command errCode = %d, want %d (EINVAL)", errCode, errInval)
	}
	if err := finish(); err != nil {
		t.Fatalf("dispatch returned %v, want nil", err)
	}
}

func TestDispatchDisconnectReturnsCleanly(t *testing.T) {
	server, kernel := net.Pipe()
	errc := make(chan error, 1)
	go func() { errc <- Dispatch(server, &bytesProvider{data: make([]byte, 8)}) }()

	// A disconnect command yields no reply; Dispatch must return nil on its own.
	if _, err := kernel.Write(nbdRequest(cmdDisc, 0, 0, 0)); err != nil {
		t.Fatalf("write disconnect: %v", err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("dispatch after disconnect returned %v, want nil", err)
	}
	_ = kernel.Close()
	_ = server.Close()
}

func TestDispatchBadMagicIsError(t *testing.T) {
	server, kernel := net.Pipe()
	errc := make(chan error, 1)
	go func() { errc <- Dispatch(server, &bytesProvider{data: make([]byte, 8)}) }()

	bad := nbdRequest(cmdRead, 0, 0, 8)
	binary.BigEndian.PutUint32(bad[0:4], 0x00000000) // corrupt the request magic
	if _, err := kernel.Write(bad); err != nil {
		t.Fatalf("write bad request: %v", err)
	}
	if err := <-errc; err == nil {
		t.Fatalf("dispatch accepted a bad magic, want a protocol error")
	}
	_ = kernel.Close()
	_ = server.Close()
}
