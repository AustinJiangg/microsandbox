package storage

// KVM-/network-free unit tests for the blob store seam. The Local (dir-as-bucket) impl exercises the
// interface + the alias/key/materialize helpers hermetically (a tempdir, no MinIO). The S3 impl is
// covered by TestS3RoundTrip, which self-skips unless MSB_TEST_S3_ENDPOINT points at a live MinIO.

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestArtifactKey(t *testing.T) {
	if got := ArtifactKey("bld123", RootfsName); got != "bld123/rootfs.ext4" {
		t.Errorf("ArtifactKey = %q, want bld123/rootfs.ext4", got)
	}
}

func TestValidateBuildableRejectsDefaultAndInvalid(t *testing.T) {
	// "default"/"" are the stock image; the rest would escape or aren't canonical single components.
	for _, name := range []string{"", "default", "../etc", "Bad", "a/b", "with space", "."} {
		if err := ValidateBuildable(name); err == nil {
			t.Errorf("ValidateBuildable(%q) = nil, want an error", name)
		}
	}
	if err := ValidateBuildable("demo"); err != nil {
		t.Errorf("ValidateBuildable(demo) = %v, want nil", err)
	}
}

func TestLocalTemplateDir(t *testing.T) {
	dir, err := LocalTemplateDir("/vendor", "demo")
	if err != nil {
		t.Fatalf("LocalTemplateDir: %v", err)
	}
	if want := filepath.Join("/vendor", "templates", "demo"); dir != want {
		t.Fatalf("LocalTemplateDir = %q, want %q", dir, want)
	}
	if _, err := LocalTemplateDir("/vendor", "default"); err == nil {
		t.Error("LocalTemplateDir(default) = nil, want an error")
	}
}

// Local round-trips an object through Upload/Open/OpenReaderAt/Exists -- the StorageProvider contract.
func TestLocalRoundTrip(t *testing.T) {
	sp := NewLocal(t.TempDir())
	ctx := context.Background()
	key := ArtifactKey("bld1", MemfileName)
	want := []byte("hello firecracker memory")

	if ok, _ := sp.Exists(ctx, key); ok {
		t.Fatal("Exists before Upload = true, want false")
	}
	if err := sp.Upload(ctx, key, bytes.NewReader(want), int64(len(want))); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if ok, err := sp.Exists(ctx, key); err != nil || !ok {
		t.Fatalf("Exists after Upload = %v, %v; want true, nil", ok, err)
	}

	rc, err := sp.Open(ctx, key)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, want) {
		t.Errorf("Open read %q, want %q", got, want)
	}

	// OpenReaderAt is the UFFD page source path: a range read at an offset.
	rr, err := sp.OpenReaderAt(ctx, key)
	if err != nil {
		t.Fatalf("OpenReaderAt: %v", err)
	}
	defer rr.Close()
	buf := make([]byte, 5)
	if _, err := rr.ReadAt(buf, 6); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if string(buf) != "firec" {
		t.Errorf("ReadAt(6,5) = %q, want %q", buf, "firec")
	}
}

// SetAlias/ResolveAlias round-trip the mutable name->buildID pointer through the store.
func TestAliasRoundTrip(t *testing.T) {
	sp := NewLocal(t.TempDir())
	ctx := context.Background()
	if err := SetAlias(ctx, sp, "demo", "bld-xyz"); err != nil {
		t.Fatalf("SetAlias: %v", err)
	}
	got, err := ResolveAlias(ctx, sp, "demo")
	if err != nil {
		t.Fatalf("ResolveAlias: %v", err)
	}
	if got != "bld-xyz" {
		t.Errorf("ResolveAlias = %q, want bld-xyz", got)
	}
	if _, err := ResolveAlias(ctx, sp, "missing"); err == nil {
		t.Error("ResolveAlias(missing) = nil, want an error")
	}
}

// Materialize downloads on a cache miss and is a no-op (does not re-download) on a cache hit.
func TestMaterialize(t *testing.T) {
	root := t.TempDir()
	sp := NewLocal(root)
	ctx := context.Background()
	key := ArtifactKey("bld1", RootfsName)
	want := []byte("rootfs bytes")
	if err := sp.Upload(ctx, key, bytes.NewReader(want), int64(len(want))); err != nil {
		t.Fatalf("Upload: %v", err)
	}

	dst := filepath.Join(t.TempDir(), "nested", "rootfs.ext4")
	if err := Materialize(ctx, sp, key, dst); err != nil {
		t.Fatalf("Materialize (miss): %v", err)
	}
	if got, _ := os.ReadFile(dst); !bytes.Equal(got, want) {
		t.Errorf("materialized %q, want %q", got, want)
	}

	// Cache hit: overwrite dst with a sentinel, Materialize again, and confirm it was NOT re-downloaded.
	sentinel := []byte("local cache wins")
	if err := os.WriteFile(dst, sentinel, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Materialize(ctx, sp, key, dst); err != nil {
		t.Fatalf("Materialize (hit): %v", err)
	}
	if got, _ := os.ReadFile(dst); !bytes.Equal(got, sentinel) {
		t.Errorf("cache hit re-downloaded: dst = %q, want the sentinel %q", got, sentinel)
	}
}

// TestS3RoundTrip exercises the real S3 impl against a live MinIO; it self-skips when one isn't
// configured, keeping `go test ./services/...` hermetic (the same gate the Redis/Postgres tests use).
func TestS3RoundTrip(t *testing.T) {
	endpoint := os.Getenv("MSB_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("set MSB_TEST_S3_ENDPOINT (host:port of a live MinIO) to run the S3 storage test")
	}
	ctx := context.Background()
	sp, err := NewS3(ctx, endpoint, envOr("MSB_TEST_S3_ACCESS_KEY", "minioadmin"),
		envOr("MSB_TEST_S3_SECRET_KEY", "minioadmin"), envOr("MSB_TEST_S3_BUCKET", "msb-test"), false)
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}
	key := ArtifactKey("bldtest", MemfileName)
	want := []byte("object storage page bytes")
	if err := sp.Upload(ctx, key, bytes.NewReader(want), int64(len(want))); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	rr, err := sp.OpenReaderAt(ctx, key)
	if err != nil {
		t.Fatalf("OpenReaderAt: %v", err)
	}
	defer rr.Close()
	buf := make([]byte, 4)
	if _, err := rr.ReadAt(buf, 7); err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if string(buf) != "stor" { // "object storage page bytes"[7:11]
		t.Errorf("ReadAt(7,4) = %q, want %q", buf, "stor")
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
