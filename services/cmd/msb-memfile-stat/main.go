// Command msb-memfile-stat reports how many bytes a LAYERED template's memfile actually occupies in the
// object store (its copy-on-write diff over the base) versus the base's full compacted memfile -- i.e.
// the Stage-20 memfile COW win. For a layered child the stored object ({childBuildID}/memfile) holds only
// the RAM blocks that differ from the base, while the child's header names its base build, whose
// {baseBuildID}/memfile is the full compacted memfile a non-layered child would otherwise store (Stage 17).
// It prints "<stored> <full>" (two integers, bytes) to stdout so the e2e harness can assert the diff is a
// small fraction of the base (mirroring msb-rootfs-stat), and a human summary to stderr. Dev/test glue
// like msb-seed / msb-rootfs-stat; NOT for production.
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
	name := flag.String("name", "", "layered template name to stat (e.g. derived_snap)")
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

	stored, err := objectSize(ctx, sp, storage.ArtifactKey(bid, storage.MemfileName))
	if err != nil {
		log.Fatalf("size stored memfile diff: %v", err)
	}
	h, err := storage.OpenMemfileHeader(ctx, sp, bid)
	if err != nil {
		log.Fatalf("open memfile header: %v", err)
	}
	if h == nil {
		log.Fatalf("template %q (build %s) has no memfile header -- not a layered/compacted memfile", *name, bid)
	}
	// "full" is the base build's compacted memfile: what a non-layered child would store (Stage 17). The
	// child's header names its base at the chain root; for a default-based layer that is the default's memfile.
	baseID := h.Metadata.BaseBuildId
	if baseID == "" {
		log.Fatalf("template %q (build %s) is not layered (no base build) -- nothing to compare against", *name, bid)
	}
	full, err := objectSize(ctx, sp, storage.ArtifactKey(baseID, storage.MemfileName))
	if err != nil {
		log.Fatalf("size base compacted memfile %q: %v", baseID, err)
	}

	pct := 100.0
	if full > 0 {
		pct = 100 * float64(stored) / float64(full)
	}
	log.Printf("template %q (build %s over base %s): memfile diff %d B over base compacted %d B (%.4f%%)",
		*name, bid, baseID, stored, full, pct)
	fmt.Printf("%d %d\n", stored, full) // machine-readable: "<stored> <full>" for the e2e assertion
}

// objectSize returns key's byte length. *minio.Object / os.File both implement Seeker, so this stats the
// object rather than reading it; a reader without a Seeker falls back to counting the bytes.
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
