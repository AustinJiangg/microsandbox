package main

import (
	"bytes"
	"io"
	"path/filepath"
	"testing"

	"microsandbox/services/pkg/block"
	"microsandbox/services/pkg/fc"
	"microsandbox/services/pkg/storage"
	"microsandbox/services/pkg/template"
	"microsandbox/services/pkg/uffd"
)

// Stage 26R-a: the registry retains, per running sandbox, the handles a later per-sandbox
// Pause needs -- the writable rootfs overlay and the base build id. KVM-free coverage here:
// the legacy (local-fs / non-NBD) paths yield no overlay and no build id (which is what makes
// Pause's FailedPrecondition guard honest), and the registry verbs expose/tear down what
// restoreHealthy threads in. The "--nbd mode retains a real overlay" half needs a real NBD
// device + VM, so it is covered by the Stage 26R real-VM e2e, not here.

// TestBuildRootfsBackingLegacyNoOverlay: outside --nbd s3 mode there is no overlay and no
// resolved build id to retain -- both legacy paths (local-fs, and s3 without --nbd) return
// the zero backing.
func TestBuildRootfsBackingLegacyNoOverlay(t *testing.T) {
	cases := []struct {
		name string
		srv  *server
	}{
		{"local-fs", &server{}},
		{"s3 without --nbd", &server{storage: storage.NewLocal(t.TempDir())}},
	}
	for _, c := range cases {
		backing, overlay, buildID, err := c.srv.buildRootfsBacking(template.Template{Name: "default"})
		if err != nil {
			t.Errorf("%s: unexpected error: %v", c.name, err)
			continue
		}
		if backing.Device != "" || backing.Close != nil {
			t.Errorf("%s: want the zero RootfsBacking, got %+v", c.name, backing)
		}
		if overlay != nil {
			t.Errorf("%s: want no overlay outside --nbd s3 mode", c.name)
		}
		if buildID != "" {
			t.Errorf("%s: want no build id outside --nbd s3 mode, got %q", c.name, buildID)
		}
	}
}

// TestRegistryRetainsLiveSandbox: what create registers is reachable through lookup -- the
// VM, its writable overlay, and its base build id -- and destroy removes it. The overlay is
// a real block.Overlay (over an in-memory base), the VM a zero-value handle (fc.Destroy is
// nil-safe on every field), so the registry verbs run for real without KVM.
func TestRegistryRetainsLiveSandbox(t *testing.T) {
	const size = 4096
	base := block.NewLayeredBase(
		[]uffd.Extent{{Logical: 0, Length: size, Physical: 0, Owner: "A"}}, size, 0,
		func(string) (io.ReaderAt, func() error, error) {
			return bytes.NewReader(make([]byte, size)), func() error { return nil }, nil
		})
	cache, err := block.NewCache(filepath.Join(t.TempDir(), "cache"), size)
	if err != nil {
		t.Fatal(err)
	}
	overlay, err := block.NewOverlay(base, cache)
	if err != nil {
		t.Fatal(err)
	}
	defer overlay.Close()

	s := &server{sandboxes: map[string]*liveSandbox{}}
	ls := &liveSandbox{vm: &fc.MicroVM{ID: "sb_live"}, overlay: overlay, baseBuildID: "bld_base"}
	s.mu.Lock()
	s.sandboxes[ls.vm.ID] = ls
	s.mu.Unlock()

	got, ok := s.lookup("sb_live")
	if !ok {
		t.Fatal("lookup should find the registered sandbox")
	}
	if got.overlay != overlay {
		t.Error("lookup must expose the sandbox's writable overlay (what Pause exports)")
	}
	if got.baseBuildID != "bld_base" {
		t.Errorf("lookup must expose the base build id, got %q", got.baseBuildID)
	}
	if !s.destroy("sb_live") {
		t.Fatal("destroy should report the id existed")
	}
	if _, ok := s.lookup("sb_live"); ok {
		t.Error("destroy must drop the sandbox from the registry")
	}
}
