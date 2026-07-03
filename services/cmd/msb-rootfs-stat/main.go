// Command msb-rootfs-stat reports how many bytes a template's rootfs actually occupies in the object
// store versus its full logical size -- i.e. the copy-on-write layering win (Stage 18/19). For a
// layered build the stored object ({buildID}/rootfs.ext4) holds only the child's changed blocks while
// the header's Metadata.Size is the full assembled size; for a non-layered build the two are equal.
// It prints "<stored> <full>" (two integers, bytes) to stdout so the e2e harness can assert the diff
// is a tiny fraction of the base (docs/STAGE19_DESIGN.md Decision 4 -- "a Go probe in the e2e
// harness"), and a human summary to stderr. Dev/test glue, like msb-seed; NOT for production.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"

	"microsandbox/services/pkg/storage"
)

func main() {
	endpoint := flag.String("s3-endpoint", "127.0.0.1:9000", "S3/MinIO endpoint host:port (no scheme)")
	bucket := flag.String("s3-bucket", "msb", "S3 bucket holding template artifacts")
	accessKey := flag.String("s3-access-key", "minioadmin", "S3 access key")
	secretKey := flag.String("s3-secret-key", "minioadmin", "S3 secret key")
	ssl := flag.Bool("s3-ssl", false, "use https for the S3 endpoint")
	name := flag.String("name", "", "template name to stat (e.g. derived)")
	flag.Parse()

	if *name == "" {
		log.Fatal("--name is required")
	}
	ctx := context.Background()
	sp, err := storage.NewS3(ctx, *endpoint, *accessKey, *secretKey, *bucket, *ssl)
	if err != nil {
		log.Fatalf("connect object storage: %v", err)
	}
	bid, err := storage.ResolveAlias(ctx, sp, *name)
	if err != nil {
		log.Fatalf("resolve template %q: %v", *name, err)
	}

	stored, err := objectSize(ctx, sp, storage.ArtifactKey(bid, storage.RootfsName))
	if err != nil {
		log.Fatalf("size stored rootfs: %v", err)
	}
	// A layered build's full size is its header's Metadata.Size; a non-layered build has no header, so
	// the stored object already is the full rootfs.
	full := stored
	if h, err := storage.OpenRootfsHeader(ctx, sp, bid); err != nil {
		log.Fatalf("open rootfs header: %v", err)
	} else if h != nil {
		full = int64(h.Metadata.Size)
	}

	pct := 100.0
	if full > 0 {
		pct = 100 * float64(stored) / float64(full)
	}
	log.Printf("template %q (build %s): stored %d B over full %d B (%.4f%%)", *name, bid, stored, full, pct)
	fmt.Printf("%d %d\n", stored, full) // machine-readable: "<stored> <full>" for the e2e assertion
}

// objectSize returns key's byte length. os.File / *minio.Object both implement Seeker, so this stats
// the object rather than reading it; a reader without a Seeker falls back to counting the bytes.
func objectSize(ctx context.Context, sp storage.StorageProvider, key string) (int64, error) {
	rc, err := sp.Open(ctx, key)
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	if s, ok := rc.(io.Seeker); ok {
		return s.Seek(0, io.SeekEnd)
	}
	return io.Copy(io.Discard, rc)
}
