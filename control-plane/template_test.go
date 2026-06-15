package main

import (
	"path/filepath"
	"testing"
)

// resolveTemplate is pure path + name logic, so these run with no VM/KVM/filesystem,
// like proxy_test.go / pool_test.go. They pin the two invariants Stage 6 leans on:
// (1) default maps to the legacy vendor/ paths (so existing artifacts/tests are
// untouched), and (2) a name can never escape vendor/templates/<name>/.

func TestResolveTemplateDefault(t *testing.T) {
	for _, name := range []string{"", "default"} {
		tmpl, err := resolveTemplate("/v", name)
		if err != nil {
			t.Fatalf("resolveTemplate(%q) errored: %v", name, err)
		}
		if tmpl.name != defaultTemplate {
			t.Errorf("name = %q, want %q", tmpl.name, defaultTemplate)
		}
		if want := filepath.Join("/v", "rootfs.ext4"); tmpl.rootfs != want {
			t.Errorf("rootfs = %q, want %q", tmpl.rootfs, want)
		}
		if want := filepath.Join("/v", "snapshot"); tmpl.snapshotDir != want {
			t.Errorf("snapshotDir = %q, want %q", tmpl.snapshotDir, want)
		}
	}
}

func TestResolveTemplateNamed(t *testing.T) {
	tmpl, err := resolveTemplate("/v", "ml-env")
	if err != nil {
		t.Fatalf("resolveTemplate errored: %v", err)
	}
	if tmpl.name != "ml-env" {
		t.Errorf("name = %q, want ml-env", tmpl.name)
	}
	if want := filepath.Join("/v", "templates", "ml-env", "rootfs.ext4"); tmpl.rootfs != want {
		t.Errorf("rootfs = %q, want %q", tmpl.rootfs, want)
	}
	if want := filepath.Join("/v", "templates", "ml-env", "snapshot"); tmpl.snapshotDir != want {
		t.Errorf("snapshotDir = %q, want %q", tmpl.snapshotDir, want)
	}
}

func TestResolveTemplateRejectsUnsafeNames(t *testing.T) {
	// Anything that could escape vendor/templates/<name>/ or isn't a clean lowercase
	// component must be rejected before it ever becomes a path.
	for _, name := range []string{
		"../../etc", "a/b", "foo/", "/abs", ".", "..", "-lead", "_lead",
		"UPPER", "with space", "name.dot", "x/../y",
	} {
		if _, err := resolveTemplate("/v", name); err == nil {
			t.Errorf("resolveTemplate(%q) = nil error, want rejection", name)
		}
	}
}

func TestResolveTemplateAcceptsValidNames(t *testing.T) {
	for _, name := range []string{"a", "ml-env", "py3_12", "web2", "a-b_c-1"} {
		if _, err := resolveTemplate("/v", name); err != nil {
			t.Errorf("resolveTemplate(%q) errored: %v", name, err)
		}
	}
}
