package storage

import (
	"path/filepath"
	"testing"
)

// Local must satisfy the StorageProvider interface (the seam swapped for object storage later).
var _ StorageProvider = (*Local)(nil)

func TestTemplateDir(t *testing.T) {
	l := NewLocal("/vendor")
	dir, err := l.TemplateDir("demo")
	if err != nil {
		t.Fatalf("TemplateDir: %v", err)
	}
	if want := filepath.Join("/vendor", "templates", "demo"); dir != want {
		t.Fatalf("TemplateDir = %q, want %q", dir, want)
	}
}

func TestTemplateDirRejectsDefaultAndInvalid(t *testing.T) {
	l := NewLocal("/vendor")
	// "default"/"" are the stock image (not API-buildable); the rest would escape or are
	// not canonical single path components -- the same rule pkg/template enforces.
	for _, name := range []string{"", "default", "../etc", "Bad", "a/b", "with space", "."} {
		if dir, err := l.TemplateDir(name); err == nil {
			t.Errorf("TemplateDir(%q) = %q, want an error", name, dir)
		}
	}
}
