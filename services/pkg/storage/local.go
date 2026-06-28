package storage

import (
	"context"
	"io"
	"os"
	"path/filepath"
)

// Local is a StorageProvider backed by a directory treated as a bucket: a key maps to a file at
// root/<key>, and range reads are file preads. Since Stage 15 flipped the running default to S3,
// Local survives only as the **hermetic unit-test double** for the interface + the alias/key/
// materialize helpers (no MinIO needed) -- the same demotion catalog.InMemory took in Stage 14.
// (Note: the orchestrator's `--storage local-fs` escape hatch is the *nil-provider* direct-path mode,
// not this dir-as-bucket impl; they are different things, see docs/STAGE15_DESIGN.md.)
type Local struct{ root string }

// NewLocal returns a Local provider rooted at root (each key becomes root/<key>).
func NewLocal(root string) *Local { return &Local{root: root} }

var _ StorageProvider = (*Local)(nil)

// path turns a slash-separated object key into a local filesystem path under root.
func (l *Local) path(key string) string { return filepath.Join(l.root, filepath.FromSlash(key)) }

func (l *Local) Upload(_ context.Context, key string, r io.Reader, _ int64) error {
	p := l.path(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.Create(p)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, r)
	return err
}

func (l *Local) Open(_ context.Context, key string) (io.ReadCloser, error) {
	return os.Open(l.path(key))
}

// OpenReaderAt returns the key's *os.File, which is a RangeReader (ReadAt + Close) directly.
func (l *Local) OpenReaderAt(_ context.Context, key string) (RangeReader, error) {
	return os.Open(l.path(key))
}

func (l *Local) Exists(_ context.Context, key string) (bool, error) {
	_, err := os.Stat(l.path(key))
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}
