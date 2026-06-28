// Package storage is the artifact object store: where a template's built files (rootfs + snapshot)
// live so the orchestrator can boot a sandbox from them. Stage 15 reshaped it from a path resolver
// into E2B's real StorageProvider seam -- a blob store addressed by key, with whole-object reads
// (materialize) and range reads (the UFFD memfile page source). Two impls: S3 (minio-go, the default;
// s3.go) and Local (a directory treated as a bucket, the hermetic unit-test double; local.go).
//
// Why this seam is *not* isomorphic (unlike Stage 14's catalog/store): a Firecracker snapshot bakes
// in its rootfs's *absolute path* (see fc.Restore + scripts/build-snapshot.sh), so artifacts can't
// merely be opened from a bucket -- rootfs + snapfile must be **materialized to the baked local path
// before boot**, while the memfile is **streamed page-by-page** from the bucket via the Stage-13 UFFD
// handler (the payoff Stage 13 unlocked). See docs/STAGE15_DESIGN.md.
//
// Object layout mirrors E2B's: immutable per-build artifacts at "{buildID}/{file}", and a mutable
// "aliases/<name>" pointer at the template's current buildID (Decision 8 -- the single-machine
// stand-in for E2B's DB-side name->build resolution). E2B keys the VM-state file "snapfile"; our
// local copy is named "vmstate" (fc.Restore's snapshot_path), so the two names map across this seam.
package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"microsandbox/services/pkg/template"
)

// Artifact file names within a build's prefix, mirroring E2B's storage constants.
const (
	RootfsName   = "rootfs.ext4"
	MemfileName  = "memfile"
	SnapfileName = "snapfile" // E2B's name for the Firecracker VM state (our local file is "vmstate")
)

// aliasPrefix namespaces the mutable name->buildID pointers away from the immutable {buildID}/ prefixes.
const aliasPrefix = "aliases/"

// RangeReader is a random-access, closable object reader: ReadAt fetches a byte range on demand
// (an HTTP Range GET for S3, a pread for Local). Its method set is exactly uffd.PageSource's, so the
// memfile reader can be handed straight to the UFFD handler as the page source (no adapter).
type RangeReader interface {
	io.ReaderAt
	io.Closer
}

// StorageProvider is the artifact blob store (E2B's StorageProvider seam: OpenBlob/OpenSeekable).
// The orchestrator materializes rootfs/snapfile whole (Open) and streams the memfile page-by-page
// (OpenReaderAt). Impls: S3 (the running default) and Local (the unit-test double).
type StorageProvider interface {
	// Upload writes size bytes from r to key, overwriting any existing object.
	Upload(ctx context.Context, key string, r io.Reader, size int64) error
	// Open opens the whole object at key for sequential reading (used to materialize rootfs/snapfile).
	Open(ctx context.Context, key string) (io.ReadCloser, error)
	// OpenReaderAt opens key for random-access range reads -- the memfile page source for UFFD. The
	// returned reader satisfies uffd.PageSource (io.ReaderAt + Close); the caller closes it.
	OpenReaderAt(ctx context.Context, key string) (RangeReader, error)
	// Exists reports whether key is present.
	Exists(ctx context.Context, key string) (bool, error)
}

// ArtifactKey is the immutable object key for one of a build's files: "{buildID}/{file}".
func ArtifactKey(buildID, file string) string { return buildID + "/" + file }

// ValidateBuildable rejects names that cannot be built/published via the API: "" and "default" are
// the baked stock image, and a name must be a single safe path component (the rule shared with
// pkg/template, so "a name that builds is a name that resolves").
func ValidateBuildable(name string) error {
	if name == "" || name == template.DefaultTemplate {
		return fmt.Errorf("the default template is the baked stock image; it cannot be built via the API")
	}
	if !template.ValidName(name) {
		return fmt.Errorf("invalid template name %q: must match [a-z0-9][a-z0-9_-]* (max 64 chars)", name)
	}
	return nil
}

// LocalTemplateDir is the local published/cache dir for a buildable template (root/templates/<name>),
// used by the builder for its output before upload. Rejects "default"/invalid via ValidateBuildable.
func LocalTemplateDir(root, name string) (string, error) {
	if err := ValidateBuildable(name); err != nil {
		return "", err
	}
	return filepath.Join(root, "templates", name), nil
}

// SetAlias points template name at buildID (its current build) -- a small mutable object that the
// orchestrator resolves on boot. Publishing flips this after the immutable artifacts are uploaded.
func SetAlias(ctx context.Context, sp StorageProvider, name, buildID string) error {
	b := []byte(buildID)
	return sp.Upload(ctx, aliasPrefix+name, bytes.NewReader(b), int64(len(b)))
}

// ResolveAlias returns the buildID that template name currently points at (the inverse of SetAlias).
func ResolveAlias(ctx context.Context, sp StorageProvider, name string) (string, error) {
	rc, err := sp.Open(ctx, aliasPrefix+name)
	if err != nil {
		return "", fmt.Errorf("resolve template %q: %w", name, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return "", fmt.Errorf("read alias for %q: %w", name, err)
	}
	buildID := strings.TrimSpace(string(b))
	if buildID == "" {
		return "", fmt.Errorf("template %q resolves to an empty build id", name)
	}
	return buildID, nil
}

// Materialize downloads key to the local path dst if dst is absent (the baked path is the cache, so a
// present file is a cache hit and we skip the download). The write is atomic (temp + rename) so a
// concurrent boot -- e.g. several warm-pool restores of one template -- never sees a partial file.
func Materialize(ctx context.Context, sp StorageProvider, key, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return nil // cache hit: the baked local path already holds this artifact
	}
	rc, err := sp.Open(ctx, key)
	if err != nil {
		return fmt.Errorf("open %s: %w", key, err)
	}
	defer rc.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp := dst + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, rc); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("download %s -> %s: %w", key, dst, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst) // atomic publish at the baked path
}
