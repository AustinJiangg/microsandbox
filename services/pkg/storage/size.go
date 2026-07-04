package storage

import (
	"context"
	"fmt"
	"os"

	"github.com/minio/minio-go/v7"
)

// ObjectSize returns the byte size of the whole object at key without downloading it. The NBD rootfs
// base (Stage 21c) needs it to announce the device size to the kernel for a non-layered build -- one
// with no rootfs header to read the logical size from (the default template and any built without a
// base). It reads the size the concrete providers already expose (minio's StatObject, os.Stat) rather
// than widening StorageProvider, which keeps this a self-contained addition. A provider whose reader is
// neither shape is a programming error, reported rather than guessed.
func ObjectSize(ctx context.Context, sp StorageProvider, key string) (int64, error) {
	rr, err := sp.OpenReaderAt(ctx, key)
	if err != nil {
		return 0, err
	}
	defer rr.Close()
	switch o := rr.(type) {
	case *os.File: // Local: a plain file
		fi, err := o.Stat()
		if err != nil {
			return 0, err
		}
		return fi.Size(), nil
	case *minio.Object: // S3: OpenReaderAt already Stat'd it, so this is cached
		oi, err := o.Stat()
		if err != nil {
			return 0, err
		}
		return oi.Size, nil
	default:
		return 0, fmt.Errorf("storage: cannot size %s: unknown reader type %T", key, rr)
	}
}
