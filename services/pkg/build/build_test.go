package build

// Unit tests for the build pipeline, with the command executor injected so they assert the
// exact sequence of commands (docker build -> build-rootfs.sh -> build-snapshot.sh) and the
// artifact paths -- without docker, firecracker, or KVM.

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"microsandbox/services/pkg/storage"
)

// recorder is an injectable exec that records calls instead of running them. If fail is set,
// the call whose program name contains it returns an error (to test stop-on-failure).
type recorder struct {
	mu    sync.Mutex
	calls [][]string
	fail  string
}

func (r *recorder) run(name string, args ...string) (string, error) {
	r.mu.Lock()
	r.calls = append(r.calls, append([]string{name}, args...))
	r.mu.Unlock()
	if r.fail != "" && strings.Contains(name, r.fail) {
		return "boom", fmt.Errorf("simulated failure")
	}
	return "", nil
}

func newTestBuilder(t *testing.T, rec *recorder) (*Builder, string) {
	t.Helper()
	root := t.TempDir()
	return &Builder{storage: storage.NewLocal(root), scriptsDir: "/repo/scripts", run: rec.run}, root
}

func TestBuildWithSnapshot(t *testing.T) {
	rec := &recorder{}
	b, root := newTestBuilder(t, rec)
	if err := b.Build("bld_1", "demo", "FROM microsandbox-agent\nRUN true", true); err != nil {
		t.Fatalf("Build: %v", err)
	}
	dir := filepath.Join(root, "templates", "demo")
	if len(rec.calls) != 3 {
		t.Fatalf("got %d commands, want 3: %v", len(rec.calls), rec.calls)
	}
	// docker build ... -t microsandbox-tmpl-demo (the -f path + context are a temp dir)
	if rec.calls[0][0] != "docker" || !containsArg(rec.calls[0], "microsandbox-tmpl-demo") {
		t.Errorf("call[0] = %v, want `docker build ... -t microsandbox-tmpl-demo`", rec.calls[0])
	}
	wantRootfs := []string{filepath.Join("/repo/scripts", "build-rootfs.sh"), "microsandbox-tmpl-demo", filepath.Join(dir, "rootfs.ext4")}
	if !equal(rec.calls[1], wantRootfs) {
		t.Errorf("call[1] = %v, want %v", rec.calls[1], wantRootfs)
	}
	wantSnap := []string{filepath.Join("/repo/scripts", "build-snapshot.sh"), filepath.Join(dir, "rootfs.ext4"), filepath.Join(dir, "snapshot")}
	if !equal(rec.calls[2], wantSnap) {
		t.Errorf("call[2] = %v, want %v", rec.calls[2], wantSnap)
	}
}

func TestBuildNoSnapshot(t *testing.T) {
	rec := &recorder{}
	b, _ := newTestBuilder(t, rec)
	if err := b.Build("bld_2", "demo", "FROM x", false); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(rec.calls) != 2 {
		t.Fatalf("got %d commands, want 2 (no snapshot): %v", len(rec.calls), rec.calls)
	}
	if strings.Contains(rec.calls[len(rec.calls)-1][0], "build-snapshot") {
		t.Error("build-snapshot.sh ran with withSnapshot=false")
	}
}

func TestBuildRejectsDefault(t *testing.T) {
	rec := &recorder{}
	b, _ := newTestBuilder(t, rec)
	if err := b.Build("bld_3", "default", "FROM x", true); err == nil {
		t.Fatal("Build(\"default\") should error (the stock image is not API-buildable)")
	}
	if len(rec.calls) != 0 {
		t.Errorf("no commands should run for a rejected name, got %v", rec.calls)
	}
}

func TestBuildStopsOnFailure(t *testing.T) {
	rec := &recorder{fail: "build-rootfs"}
	b, _ := newTestBuilder(t, rec)
	if err := b.Build("bld_4", "demo", "FROM x", true); err == nil {
		t.Fatal("Build should fail when build-rootfs.sh fails")
	}
	// docker build + build-rootfs ran; build-snapshot did not (the failure stopped it).
	if len(rec.calls) != 2 {
		t.Fatalf("got %d commands, want 2 (stop after the failing build-rootfs): %v", len(rec.calls), rec.calls)
	}
}

func containsArg(call []string, want string) bool {
	for _, a := range call {
		if a == want {
			return true
		}
	}
	return false
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
