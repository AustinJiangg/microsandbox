//go:build linux

package block

import (
	"errors"
	"io"
)

// ReadOnly presents a ReadSource as a read-only nbd.Provider: reads pass through to the base, writes are
// rejected. Stage 21c serves the rootfs read-only (the guest mounts root ro, unchanged from before NBD),
// so the drive is bound is_read_only and the kernel never issues a write -- WriteAt returning an error is
// a belt-and-braces guard, not a path the kernel takes. The writable Overlay (this package) is wired the
// same way and swaps in when Stage 20's producer captures the overlay diff with the snapshot.
type ReadOnly struct{ base ReadSource }

// NewReadOnly wraps base as a read-only provider. It takes ownership of base (Close closes it).
func NewReadOnly(base ReadSource) *ReadOnly { return &ReadOnly{base: base} }

func (r *ReadOnly) ReadAt(p []byte, off int64) (int, error) { return r.base.ReadAt(p, off) }
func (r *ReadOnly) Size() int64                             { return r.base.Size() }
func (r *ReadOnly) Close() error                            { return r.base.Close() }

var errReadOnly = errors.New("block: rootfs is read-only (Stage 21c); the writable overlay lands in Stage 20")

// WriteAt always fails: the NBD drive is read-only, so the kernel never calls this.
func (r *ReadOnly) WriteAt([]byte, int64) (int, error) { return 0, errReadOnly }

// compile-time assurance that ReadOnly is a valid random-access reader/writer (the nbd.Provider shape).
var _ interface {
	io.ReaderAt
	io.WriterAt
} = (*ReadOnly)(nil)
