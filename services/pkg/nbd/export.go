//go:build linux

package nbd

import (
	"fmt"
	"os"
	"sync"

	"github.com/Merovius/nbd/nbdnl"
	"golang.org/x/sys/unix"
)

// nbdConns is the number of parallel socket connections bound to one device (E2B's multiconn). The
// kernel spreads block requests across them, so several Dispatch goroutines serve one Provider
// concurrently -- which is why a Provider must be concurrency-safe.
const nbdConns = 4

// nbdBlockSize is the logical block size announced to the kernel (4 KiB), matching our page/mapping
// granularity so a block request never straddles a header run boundary awkwardly.
const nbdBlockSize = 4096

// Export is a live binding of a Provider to /dev/nbd{idx}: the kernel client on one end, our Dispatch
// goroutines on the other. It owns every socket fd for the device's life and tears the binding down on
// Close (netlink Disconnect + close the sockets + wait for the goroutines).
type Export struct {
	idx   int
	files []*os.File     // all socket ends we hold (kernel + server); closed on Close
	wg    sync.WaitGroup // the per-server-socket Dispatch goroutines
}

// Bind attaches provider to /dev/nbd{idx} over netlink multiconn (Merovius/nbd/nbdnl), the E2B-faithful
// path (Decision D1). It creates nbdConns socketpairs, hands the kernel one end of each via
// nbdnl.Connect (which carries the device size and server flags out-of-band, putting the kernel
// straight into the transmission phase), and starts a Dispatch goroutine on each server end. It needs
// the nbd module and root, so it runs only on a real box; the Dispatch loop it starts is what the unit
// tests exercise over an in-memory socket.
func Bind(idx int, provider Provider) (_ *Export, err error) {
	var kernelFiles, serverFiles []*os.File
	// On any failure before the goroutines start, close every socket we opened.
	defer func() {
		if err != nil {
			for _, f := range append(kernelFiles, serverFiles...) {
				_ = f.Close()
			}
		}
	}()

	for i := 0; i < nbdConns; i++ {
		fds, perr := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
		if perr != nil {
			return nil, fmt.Errorf("nbd: socketpair for nbd%d conn %d: %w", idx, i, perr)
		}
		// fds[0] -> the kernel client (given to nbdnl.Connect); fds[1] -> our Dispatch server.
		kernelFiles = append(kernelFiles, os.NewFile(uintptr(fds[0]), fmt.Sprintf("nbd%d-kernel%d", idx, i)))
		serverFiles = append(serverFiles, os.NewFile(uintptr(fds[1]), fmt.Sprintf("nbd%d-server%d", idx, i)))
	}

	// Server flags: we support flags, flush, trim, and multiple connections. Client flags left 0 (we
	// disconnect explicitly via Close, not on last-opener-close).
	sf := nbdnl.FlagHasFlags | nbdnl.FlagSendFlush | nbdnl.FlagSendTrim | nbdnl.FlagCanMulticonn
	if _, err = nbdnl.Connect(uint32(idx), kernelFiles, uint64(provider.Size()), 0, sf, nbdnl.WithBlockSize(nbdBlockSize)); err != nil {
		return nil, fmt.Errorf("nbd: netlink connect nbd%d: %w", idx, err)
	}

	e := &Export{idx: idx}
	e.files = append(e.files, kernelFiles...)
	e.files = append(e.files, serverFiles...)
	for _, sfile := range serverFiles {
		e.wg.Add(1)
		go func(conn *os.File) {
			defer e.wg.Done()
			// Dispatch returns on EOF when Close disconnects the kernel / closes the socket. A protocol
			// error is swallowed here: the device is being torn down regardless, and there is no caller
			// waiting on a per-conn result.
			_ = Dispatch(conn, provider)
		}(sfile)
	}
	return e, nil
}

// Close tears the binding down: netlink Disconnect tells the kernel to drop the device (closing its
// socket ends, which unblocks the Dispatch reads), then we close our sockets and wait for the Dispatch
// goroutines to exit. Closing the server sockets is what guarantees a goroutine blocked mid-write also
// unblocks, so Wait cannot hang.
func (e *Export) Close() error {
	derr := nbdnl.Disconnect(uint32(e.idx))
	for _, f := range e.files {
		_ = f.Close()
	}
	e.wg.Wait()
	if derr != nil {
		return fmt.Errorf("nbd: disconnect nbd%d: %w", e.idx, derr)
	}
	return nil
}

// Index is the device number this export is bound to (for the caller to symlink / return to the pool).
func (e *Export) Index() int { return e.idx }
