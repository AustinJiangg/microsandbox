//go:build linux

package nbd

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Dispatch serves the NBD transmission-phase protocol on conn until the kernel closes it (EOF) or
// sends a disconnect. conn is one end of a socket whose other end the kernel drives as /dev/nbdX
// (Export gives the kernel the peer fd via netlink). Each iteration reads one 28-byte request, turns
// it into a ReadAt/WriteAt on provider, and writes the reply -- E2B's dispatch.go, hand-rolled.
//
// One Dispatch runs per socket. Export binds several sockets to one device (multiconn), so several
// Dispatch goroutines share one provider and the kernel load-balances requests across them; that is
// why Provider must be concurrency-safe. Within a single conn, requests are handled strictly in order
// (the kernel tolerates out-of-order replies via the handle, but in-order is simplest and correct).
//
// It returns nil on an orderly close (EOF or NBD_CMD_DISC) and an error only on a protocol violation
// or an unrecoverable conn write failure -- a per-request Provider error is reported to the guest in
// the reply (errIO), not returned, so one bad block does not tear down the whole device.
func Dispatch(conn io.ReadWriter, provider Provider) error {
	var hdr [requestLen]byte
	for {
		if _, err := io.ReadFull(conn, hdr[:]); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil // the kernel disconnected (netlink Disconnect closes the socket)
			}
			return fmt.Errorf("nbd: read request: %w", err)
		}
		if magic := binary.BigEndian.Uint32(hdr[0:4]); magic != requestMagic {
			return fmt.Errorf("nbd: bad request magic %#x", magic)
		}
		// hdr[4:6] is the command-flag field (FUA, etc.); our backing store has no ordering guarantees
		// to honor, so we ignore it. handle is an opaque cookie we must echo back verbatim.
		typ := binary.BigEndian.Uint16(hdr[6:8])
		handle := binary.BigEndian.Uint64(hdr[8:16])
		offset := int64(binary.BigEndian.Uint64(hdr[16:24]))
		length := binary.BigEndian.Uint32(hdr[24:28])

		switch typ {
		case cmdRead:
			if err := serveRead(conn, provider, handle, offset, int(length)); err != nil {
				return err
			}
		case cmdWrite:
			if err := serveWrite(conn, provider, handle, offset, int(length)); err != nil {
				return err
			}
		case cmdDisc:
			return nil // orderly disconnect: no reply expected
		case cmdFlush, cmdTrim:
			// Nothing to persist/discard on our side (the cache is a plain sparse file). Ack success so
			// the guest's fsync/discard completes; correctness does not depend on either doing work here.
			if err := writeReply(conn, handle, errNone); err != nil {
				return err
			}
		default:
			if err := writeReply(conn, handle, errInval); err != nil {
				return err
			}
		}
	}
}

// serveRead fills length bytes from provider at offset and writes them after a success reply header. A
// Provider read failure is reported to the guest as errIO (no data follows); io.EOF at the exact device
// end is not an error (ReadAt filled the buffer). The reply header and data go out in one write -- one
// syscall, and the frame can never be split.
func serveRead(conn io.Writer, provider Provider, handle uint64, offset int64, length int) error {
	buf := make([]byte, simpleReplyLen+length)
	data := buf[simpleReplyLen:]
	if _, err := provider.ReadAt(data, offset); err != nil && !errors.Is(err, io.EOF) {
		return writeReply(conn, handle, errIO)
	}
	putReplyHeader(buf[:simpleReplyLen], handle, errNone)
	if _, err := conn.Write(buf); err != nil {
		return fmt.Errorf("nbd: write read reply: %w", err)
	}
	return nil
}

// serveWrite reads length payload bytes off conn (which MUST be consumed to keep the stream framed,
// even on a Provider error) and writes them through provider at offset, then replies. A short read of
// the payload is a fatal protocol error; a Provider write failure is a per-request errIO.
func serveWrite(conn io.ReadWriter, provider Provider, handle uint64, offset int64, length int) error {
	data := make([]byte, length)
	if _, err := io.ReadFull(conn, data); err != nil {
		return fmt.Errorf("nbd: read write payload: %w", err)
	}
	if _, err := provider.WriteAt(data, offset); err != nil {
		return writeReply(conn, handle, errIO)
	}
	return writeReply(conn, handle, errNone)
}

// writeReply sends a bare 16-byte simple reply (used for writes, flush/trim acks, and errors).
func writeReply(conn io.Writer, handle uint64, errCode uint32) error {
	var hdr [simpleReplyLen]byte
	putReplyHeader(hdr[:], handle, errCode)
	if _, err := conn.Write(hdr[:]); err != nil {
		return fmt.Errorf("nbd: write reply: %w", err)
	}
	return nil
}

// putReplyHeader lays the 16-byte simple reply header into b: magic, error, handle (big-endian).
func putReplyHeader(b []byte, handle uint64, errCode uint32) {
	binary.BigEndian.PutUint32(b[0:4], simpleReplyMagic)
	binary.BigEndian.PutUint32(b[4:8], errCode)
	binary.BigEndian.PutUint64(b[8:16], handle)
}
