//go:build linux

// Package nbd serves a block device to a Firecracker guest over the kernel's Network Block Device
// client (/dev/nbdX), so the rootfs is streamed lazily from object storage instead of materialized
// whole to a local file before boot (Stages 15/18/19 assembled it via MaterializeLayered). It is the
// disk-side analogue of pkg/uffd: uffd serves guest RAM page-by-page on a userfaultfd fault, this
// serves guest disk block-by-block on an NBD read -- both resolving each offset through the same
// pkg/storage/header COW mapping and chunked bucket reader. See docs/STAGE21_DESIGN.md.
//
// The shape mirrors E2B's pkg/nbd (verified against e2b-dev/infra):
//
//   - a userspace Dispatch server (this file's Provider + dispatch.go) speaks the NBD transmission
//     protocol on a socket the kernel drives -- it parses each 28-byte request and replies, delegating
//     the actual bytes to a Provider (an io.ReaderAt/io.WriterAt over the layered rootfs).
//   - a device Pool (pool.go) hands out free /dev/nbdX slots, sized to the warm pool.
//   - Export (export.go) binds a Provider to a device via netlink multiconn (Merovius/nbd/nbdnl) --
//     the one new runtime dependency this stage accepts for E2B fidelity (Decision D1).
//
// Firecracker itself is stock: the drive's path_on_host is a constant that a per-VM symlink points at
// the freshly-allocated /dev/nbdX, so the snapshot never bakes a device node or a rootfs file (the
// portable-snapshot payoff). That symlink/mount-ns wiring lands in fc (Stage 21c); this package is the
// transport underneath it and stays KVM-free-testable (Dispatch over an in-memory socket, sysfs parsing
// over a fake tree); the real device bind needs the nbd module and is exercised by the Python e2e.
package nbd

import "io"

// Provider is the block device backing an NBD export: random-access reads and writes plus a fixed
// size. It is exactly E2B's Provider{io.ReaderAt, io.WriterAt, Size()}. Dispatch turns each kernel NBD
// request into a ReadAt/WriteAt call on the Provider, so the Provider is where the COW block stack
// plugs in: the read-only base resolves an offset through header.Locate to the owning build's chunked
// bucket object (Stage 21b), and the writable Overlay layers a per-VM cache on top. ReadAt/WriteAt may
// be called concurrently -- the kernel binds several sockets (multiconn) each with its own Dispatch
// goroutine -- so a Provider must be safe for concurrent access.
type Provider interface {
	io.ReaderAt
	io.WriterAt
	// Size is the logical size of the exported device in bytes. It is reported to the kernel at bind
	// time (nbdnl.Connect) and bounds every offset the kernel will ever request.
	Size() int64
}

// The NBD transmission-phase wire constants (big-endian on the wire). We speak this phase directly:
// the kernel is put into transmission mode by the netlink bind (Export), which carries the size and
// flags out-of-band, so there is no in-band handshake to implement -- Dispatch only ever sees requests.
const (
	// requestMagic (NBD_REQUEST_MAGIC) prefixes every 28-byte request the kernel sends.
	requestMagic = 0x25609513
	// simpleReplyMagic (NBD_SIMPLE_REPLY_MAGIC) prefixes every 16-byte reply we send. We never
	// negotiate structured replies (that is a handshake option we skip), so simple replies are correct.
	simpleReplyMagic = 0x67446698

	// requestLen is the fixed size of a classic (non-extended) NBD request header.
	requestLen = 28
	// simpleReplyLen is the fixed size of a simple reply header (magic + error + handle).
	simpleReplyLen = 16
)

// NBD command types (the request's 2-byte type field).
const (
	cmdRead  = 0 // NBD_CMD_READ:  reply header then `length` bytes of data
	cmdWrite = 1 // NBD_CMD_WRITE: request header then `length` bytes of data, reply header only
	cmdDisc  = 2 // NBD_CMD_DISC:  orderly disconnect, no reply
	cmdFlush = 3 // NBD_CMD_FLUSH: durability barrier; our cache is a plain file, so it is a no-op ack
	cmdTrim  = 4 // NBD_CMD_TRIM:  discard a range; we ack without punching a hole (a learning impl)
)

// NBD reply error codes (the reply's 4-byte error field). Zero is success; the kernel surfaces a
// nonzero value to the guest as a block I/O error, so we map our failures onto the errno the guest
// would expect. Only the small set we actually emit is named here.
const (
	errNone  = 0  // success
	errIO    = 5  // EIO:    the Provider read/write failed
	errInval = 22 // EINVAL: an unsupported command or a malformed request
)
