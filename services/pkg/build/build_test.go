package build

// Unit tests for the build pipeline, with the command executor injected so they assert the
// exact sequence of commands (docker build -> build-rootfs.sh -> build-snapshot.sh) and the
// artifact paths -- without docker, firecracker, or KVM.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"microsandbox/services/pkg/storage"
	"microsandbox/services/pkg/storage/header"
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
	// storage=nil -> local-fs mode: the command-sequence assertions below aren't perturbed by the
	// publish step (which would need the real artifact files the fake exec never creates). The
	// upload+alias path is covered separately by TestBuildPublishesToBucket.
	return &Builder{storage: nil, localRoot: root, scriptsDir: "/repo/scripts", run: rec.run}, root
}

func TestBuildWithSnapshot(t *testing.T) {
	rec := &recorder{}
	b, root := newTestBuilder(t, rec)
	if err := b.Build("bld_1", "demo", "FROM microsandbox-agent\nRUN true", "", true); err != nil {
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
	if err := b.Build("bld_2", "demo", "FROM x", "", false); err != nil {
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
	if err := b.Build("bld_3", "default", "FROM x", "", true); err == nil {
		t.Fatal("Build(\"default\") should error (the stock image is not API-buildable)")
	}
	if len(rec.calls) != 0 {
		t.Errorf("no commands should run for a rejected name, got %v", rec.calls)
	}
}

func TestBuildStopsOnFailure(t *testing.T) {
	rec := &recorder{fail: "build-rootfs"}
	b, _ := newTestBuilder(t, rec)
	if err := b.Build("bld_4", "demo", "FROM x", "", true); err == nil {
		t.Fatal("Build should fail when build-rootfs.sh fails")
	}
	// docker build + build-rootfs ran; build-snapshot did not (the failure stopped it).
	if len(rec.calls) != 2 {
		t.Fatalf("got %d commands, want 2 (stop after the failing build-rootfs): %v", len(rec.calls), rec.calls)
	}
}

// TestBuildPublishesToBucket covers the Stage 15 publish step: with a (Local dir-as-bucket)
// provider, a successful build uploads the immutable {buildID}/ artifacts and flips aliases/<name>.
// The fake exec creates the files build-rootfs/build-snapshot would produce, so publish has real
// bytes to upload -- all hermetic (no MinIO, no docker, no KVM).
func TestBuildPublishesToBucket(t *testing.T) {
	bucket := storage.NewLocal(t.TempDir())
	run := func(name string, args ...string) (string, error) {
		switch {
		case strings.Contains(name, "build-rootfs"): // build-rootfs.sh <image> <rootfsPath>
			_ = os.WriteFile(args[1], []byte("ROOTFS"), 0o644)
		case strings.Contains(name, "build-snapshot"): // build-snapshot.sh <rootfs> <snapDir>
			snap := args[1]
			_ = os.MkdirAll(snap, 0o755)
			_ = os.WriteFile(filepath.Join(snap, "vmstate"), []byte("VMSTATE"), 0o644)
			_ = os.WriteFile(filepath.Join(snap, "memfile"), []byte("MEMFILE"), 0o644)
		}
		return "", nil
	}
	b := &Builder{storage: bucket, localRoot: t.TempDir(), scriptsDir: "/repo/scripts", run: run}
	if err := b.Build("bld_pub", "demo", "FROM x", "", true); err != nil {
		t.Fatalf("Build: %v", err)
	}

	ctx := context.Background()
	for _, k := range []string{
		storage.ArtifactKey("bld_pub", storage.RootfsName),
		storage.ArtifactKey("bld_pub", storage.SnapfileName), // local "vmstate" uploaded as "snapfile"
		storage.ArtifactKey("bld_pub", storage.MemfileName),
	} {
		if ok, err := bucket.Exists(ctx, k); err != nil || !ok {
			t.Errorf("bucket missing %s (ok=%v err=%v)", k, ok, err)
		}
	}
	if got, err := storage.ResolveAlias(ctx, bucket, "demo"); err != nil || got != "bld_pub" {
		t.Errorf("ResolveAlias(demo) = %q, %v; want bld_pub", got, err)
	}
}

// TestBuildLayeredInPlace covers the Stage 19 layered path: a build with a `base` resolves + materializes
// the base, produces the child rootfs via the layout-preserving in-place builder (build-rootfs-layered.sh,
// NOT the re-mkfs build-rootfs.sh + size-pin), and publishes the rootfs as a v2 COW diff over the base
// rather than a whole upload. Hermetic: a Local dir-as-bucket seeded with a non-layered base (which
// MaterializeLayered downloads whole), a fake exec that writes the child rootfs at the output path.
func TestBuildLayeredInPlace(t *testing.T) {
	ctx := context.Background()
	const mib = 1 << 20
	bucket := storage.NewLocal(t.TempDir())
	// A non-layered base of 2 MiB (like the seeded default: whole object, no header).
	base := bytes.Repeat([]byte{1}, 2*mib)
	if err := bucket.Upload(ctx, storage.ArtifactKey("bld_base", storage.RootfsName), bytes.NewReader(base), int64(len(base))); err != nil {
		t.Fatal(err)
	}
	if err := storage.SetAlias(ctx, bucket, "base", "bld_base"); err != nil {
		t.Fatal(err)
	}

	var calls [][]string
	run := func(name string, args ...string) (string, error) {
		calls = append(calls, append([]string{name}, args...))
		if strings.Contains(name, "build-rootfs-layered") { // <child_img> <from_img> <base_rootfs> <out>
			_ = os.WriteFile(args[3], make([]byte, 2*mib), 0o644) // the child rootfs at the output path
		}
		return "", nil
	}
	b := &Builder{storage: bucket, localRoot: t.TempDir(), scriptsDir: "/repo/scripts", run: run}
	if err := b.Build("bld_d", "derived", "FROM base\nRUN true", "base", false); err != nil {
		t.Fatalf("layered Build: %v", err)
	}

	// The layered path ran build-rootfs-layered.sh -- NOT the re-mkfs build-rootfs.sh -- with the child
	// image, the FROM image parsed from the recipe, the materialized base, and the output path (no size-pin).
	var layeredCall []string
	for _, c := range calls {
		if strings.Contains(c[0], "build-rootfs-layered") {
			layeredCall = c
		} else if strings.HasSuffix(c[0], "build-rootfs.sh") {
			t.Errorf("layered build must not run the re-mkfs build-rootfs.sh: %v", c)
		}
	}
	if len(layeredCall) != 5 {
		t.Fatalf("build-rootfs-layered call = %v, want [script child from base out]", layeredCall)
	}
	if layeredCall[1] != "microsandbox-tmpl-derived" || layeredCall[2] != "base" {
		t.Errorf("layered child/from = %q/%q, want microsandbox-tmpl-derived/base", layeredCall[1], layeredCall[2])
	}

	// The rootfs was published as a v2 COW diff over the base (a header present), not a whole upload.
	h, err := storage.OpenRootfsHeader(ctx, bucket, "bld_d")
	if err != nil || h == nil {
		t.Fatalf("OpenRootfsHeader(bld_d): err=%v nil=%v", err, h == nil)
	}
	if h.Metadata.Version != header.VersionLayered || h.Metadata.BaseBuildId != "bld_base" || h.Metadata.Generation != 1 {
		t.Fatalf("layered metadata = %+v, want v%d base=bld_base gen=1", h.Metadata, header.VersionLayered)
	}
}

// fakeSnapshotter records LayeredSnapshot calls (and can fail) so the build wiring is testable without
// firecracker/KVM -- the real producer lives in the orchestrator (it resumes a base VM).
type fakeSnapshotter struct {
	calls [][3]string // {baseName, baseBuildID, childBuildID}
	err   error
}

func (f *fakeSnapshotter) LayeredSnapshot(_ context.Context, baseName, baseBuildID, childBuildID string) error {
	f.calls = append(f.calls, [3]string{baseName, baseBuildID, childBuildID})
	return f.err
}

// seedLayeredBase seeds a Local bucket with a non-layered 2 MiB base (like the default) under alias
// `base`, and returns a fake exec that writes the child rootfs the layered builder would produce.
func seedLayeredBase(t *testing.T, bucket *storage.Local) func(string, ...string) (string, error) {
	t.Helper()
	ctx := context.Background()
	const mib = 1 << 20
	base := bytes.Repeat([]byte{1}, 2*mib)
	if err := bucket.Upload(ctx, storage.ArtifactKey("bld_base", storage.RootfsName), bytes.NewReader(base), int64(len(base))); err != nil {
		t.Fatal(err)
	}
	if err := storage.SetAlias(ctx, bucket, "base", "bld_base"); err != nil {
		t.Fatal(err)
	}
	return func(name string, args ...string) (string, error) {
		if strings.Contains(name, "build-snapshot") {
			t.Errorf("a layered build must not run build-snapshot.sh (the live-VM producer replaces it): %v", args)
		}
		if strings.Contains(name, "build-rootfs-layered") { // <child_img> <from_img> <base_rootfs> <out>
			_ = os.WriteFile(args[3], make([]byte, 2*mib), 0o644)
		}
		return "", nil
	}
}

// TestBuildLayeredWithSnapshot: a layered build WITH a snapshot skips build-snapshot.sh and instead
// invokes the injected live-VM producer with (baseName, baseBuildID, childBuildID) (Stage 20), still
// publishing the rootfs as a COW diff.
func TestBuildLayeredWithSnapshot(t *testing.T) {
	bucket := storage.NewLocal(t.TempDir())
	run := seedLayeredBase(t, bucket)
	snap := &fakeSnapshotter{}
	b := &Builder{storage: bucket, localRoot: t.TempDir(), scriptsDir: "/repo/scripts", snapshotter: snap, run: run}
	if err := b.Build("bld_ds", "derived", "FROM base\nRUN true", "base", true); err != nil {
		t.Fatalf("layered Build with snapshot: %v", err)
	}
	if len(snap.calls) != 1 || snap.calls[0] != [3]string{"base", "bld_base", "bld_ds"} {
		t.Fatalf("LayeredSnapshot calls = %v, want one {base bld_base bld_ds}", snap.calls)
	}
	if h, err := storage.OpenRootfsHeader(context.Background(), bucket, "bld_ds"); err != nil || h == nil {
		t.Fatalf("rootfs still published as a COW diff: err=%v nil=%v", err, h == nil)
	}
}

// TestBuildLayeredSnapshotNeedsProducer: a layered build asking for a snapshot with no producer wired is
// rejected up front (before docker build), not run without a snapshot.
func TestBuildLayeredSnapshotNeedsProducer(t *testing.T) {
	bucket := storage.NewLocal(t.TempDir())
	var calls int
	b := &Builder{storage: bucket, localRoot: t.TempDir(), scriptsDir: "/repo/scripts", snapshotter: nil,
		run: func(string, ...string) (string, error) { calls++; return "", nil }}
	err := b.Build("bld_np", "derived", "FROM base\nRUN true", "base", true)
	if err == nil || !strings.Contains(err.Error(), "live-VM producer") {
		t.Fatalf("err = %v, want a 'live-VM producer' rejection", err)
	}
	if calls != 0 {
		t.Errorf("no commands should run for a rejected layered snapshot, got %d", calls)
	}
}

// TestFirstFromImage covers the recipe FROM parser that wires the layered builder's diff base
// (Decision 3): case-insensitive FROM, skipped comments/leading ARG, ignored --flags and "AS <stage>",
// and an error when a recipe has no FROM (a layered build would have nowhere to diff from).
func TestFirstFromImage(t *testing.T) {
	ok := []struct{ recipe, want string }{
		{"FROM base", "base"},
		{"FROM microsandbox-agent\nRUN true", "microsandbox-agent"},
		{"# a comment\nFROM x:1.2 AS build\nRUN y", "x:1.2"},
		{"FROM --platform=linux/amd64 img", "img"},
		{"ARG V=1\nFROM base:latest", "base:latest"}, // a leading ARG before the first FROM is skipped
		{"from lowercase-ok", "lowercase-ok"},        // FROM is case-insensitive
	}
	for _, c := range ok {
		got, err := firstFromImage(c.recipe)
		if err != nil || got != c.want {
			t.Errorf("firstFromImage(%q) = %q, %v; want %q", c.recipe, got, err, c.want)
		}
	}
	for _, recipe := range []string{"", "RUN true\n# no FROM here"} {
		if _, err := firstFromImage(recipe); err == nil {
			t.Errorf("firstFromImage(%q) = nil error; want an error (no FROM)", recipe)
		}
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
