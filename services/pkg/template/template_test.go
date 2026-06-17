package template

import (
	"path/filepath"
	"testing"
)

// Resolve is pure path + name logic, so these run with no VM/KVM/filesystem,
// like proxy_test.go / pool_test.go. They pin the two invariants Stage 6 leans on:
// (1) default maps to the legacy vendor/ paths (so existing artifacts/tests are
// untouched), and (2) a name can never escape vendor/templates/<name>/.

func TestResolveTemplateDefault(t *testing.T) {
	for _, name := range []string{"", "default"} {
		tmpl, err := Resolve("/v", name)
		if err != nil {
			t.Fatalf("Resolve(%q) errored: %v", name, err)
		}
		if tmpl.Name != DefaultTemplate {
			t.Errorf("name = %q, want %q", tmpl.Name, DefaultTemplate)
		}
		if want := filepath.Join("/v", "rootfs.ext4"); tmpl.Rootfs != want {
			t.Errorf("rootfs = %q, want %q", tmpl.Rootfs, want)
		}
		if want := filepath.Join("/v", "snapshot"); tmpl.SnapshotDir != want {
			t.Errorf("snapshotDir = %q, want %q", tmpl.SnapshotDir, want)
		}
	}
}

func TestResolveTemplateNamed(t *testing.T) {
	tmpl, err := Resolve("/v", "ml-env")
	if err != nil {
		t.Fatalf("Resolve errored: %v", err)
	}
	if tmpl.Name != "ml-env" {
		t.Errorf("name = %q, want ml-env", tmpl.Name)
	}
	if want := filepath.Join("/v", "templates", "ml-env", "rootfs.ext4"); tmpl.Rootfs != want {
		t.Errorf("rootfs = %q, want %q", tmpl.Rootfs, want)
	}
	if want := filepath.Join("/v", "templates", "ml-env", "snapshot"); tmpl.SnapshotDir != want {
		t.Errorf("snapshotDir = %q, want %q", tmpl.SnapshotDir, want)
	}
}

func TestResolveTemplateRejectsUnsafeNames(t *testing.T) {
	// Anything that could escape vendor/templates/<name>/ or isn't a clean lowercase
	// component must be rejected before it ever becomes a path.
	for _, name := range []string{
		"../../etc", "a/b", "foo/", "/abs", ".", "..", "-lead", "_lead",
		"UPPER", "with space", "name.dot", "x/../y",
	} {
		if _, err := Resolve("/v", name); err == nil {
			t.Errorf("Resolve(%q) = nil error, want rejection", name)
		}
	}
}

func TestResolveTemplateAcceptsValidNames(t *testing.T) {
	for _, name := range []string{"a", "ml-env", "py3_12", "web2", "a-b_c-1"} {
		if _, err := Resolve("/v", name); err != nil {
			t.Errorf("Resolve(%q) errored: %v", name, err)
		}
	}
}
