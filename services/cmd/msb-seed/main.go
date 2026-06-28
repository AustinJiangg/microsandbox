// Command msb-seed publishes a locally-built template's artifacts into the object store (Stage 15),
// for the templates that are NOT built through the api: the baked "default" image and any
// script-built template (e.g. "example"). It uploads {buildID}/rootfs.ext4 (plus snapfile + memfile
// when a snapshot exists) and flips aliases/<name>, using the same pkg/storage code the orchestrator
// and builder use -- so the bucket layout has a single source of truth. The e2e fixture
// (tests/conftest.py) and scripts/dev-up.sh invoke it; in a real deployment the build pipeline is the
// only writer. Dev/test glue, NOT for production.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"path/filepath"

	"microsandbox/services/pkg/storage"
	"microsandbox/services/pkg/template"
)

func main() {
	endpoint := flag.String("s3-endpoint", "127.0.0.1:9000", "S3/MinIO endpoint host:port (no scheme)")
	bucket := flag.String("s3-bucket", "msb", "S3 bucket to publish into")
	accessKey := flag.String("s3-access-key", "minioadmin", "S3 access key")
	secretKey := flag.String("s3-secret-key", "minioadmin", "S3 secret key")
	ssl := flag.Bool("s3-ssl", false, "use https for the S3 endpoint")
	vendorDir := flag.String("vendor-dir", "vendor", "dir holding the local artifacts to publish")
	name := flag.String("name", "", "template name to publish (e.g. default, example)")
	buildID := flag.String("build-id", "", "build id to key the artifacts under (default: the name)")
	flag.Parse()

	if *name == "" {
		log.Fatal("--name is required")
	}
	bid := *buildID
	if bid == "" {
		bid = *name // script-built templates have no api build id; the name is a stable sentinel
	}
	tmpl, err := template.Resolve(*vendorDir, *name)
	if err != nil {
		log.Fatal(err)
	}

	ctx := context.Background()
	sp, err := storage.NewS3(ctx, *endpoint, *accessKey, *secretKey, *bucket, *ssl)
	if err != nil {
		log.Fatalf("connect object storage: %v", err)
	}

	// rootfs is always present; snapfile (local "vmstate") + memfile only when the template has a snapshot.
	if err := upload(ctx, sp, tmpl.Rootfs, storage.ArtifactKey(bid, storage.RootfsName)); err != nil {
		log.Fatalf("upload rootfs: %v", err)
	}
	vmstate := filepath.Join(tmpl.SnapshotDir, "vmstate")
	memfile := filepath.Join(tmpl.SnapshotDir, "memfile")
	if exists(vmstate) && exists(memfile) {
		if err := upload(ctx, sp, vmstate, storage.ArtifactKey(bid, storage.SnapfileName)); err != nil {
			log.Fatalf("upload snapfile: %v", err)
		}
		if err := upload(ctx, sp, memfile, storage.ArtifactKey(bid, storage.MemfileName)); err != nil {
			log.Fatalf("upload memfile: %v", err)
		}
	}
	// Flip the alias last, so a resolver never sees a half-published build.
	if err := storage.SetAlias(ctx, sp, *name, bid); err != nil {
		log.Fatalf("set alias: %v", err)
	}
	log.Printf("seeded template %q as build %q into bucket %q at %s", *name, bid, *bucket, *endpoint)
}

func upload(ctx context.Context, sp storage.StorageProvider, localPath, key string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	return sp.Upload(ctx, key, f, fi.Size())
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
