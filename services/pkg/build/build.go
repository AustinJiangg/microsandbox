// Package build is the template builder: it turns a Dockerfile recipe into a template's
// (rootfs, snapshot) artifacts by wrapping the existing shell pipeline -- docker build ->
// build-rootfs.sh -> build-snapshot.sh -- writing them in place via a StorageProvider. It
// is the programmatic equivalent of scripts/build-template.sh (which stays for manual CLI
// use), driven asynchronously by the orchestrator's TemplateService. The command executor
// is injectable so the pipeline is unit-testable without docker / firecracker / KVM.
// See docs/STAGE10_DESIGN.md.
package build

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"microsandbox/services/pkg/storage"
)

// Builder runs the build pipeline for one template at a time. run is the command executor,
// injectable so tests can assert the command sequence without running anything.
type Builder struct {
	storage    storage.StorageProvider // bucket to publish to; nil in local-fs mode (no upload)
	localRoot  string                  // local output/cache root (the orchestrator's vendorDir)
	scriptsDir string                  // the repo's scripts/; build-rootfs.sh / build-snapshot.sh self-locate REPO_ROOT
	run        func(name string, args ...string) (string, error)
}

// New returns a Builder that shells out for real: it writes a template's artifacts locally under
// localRoot and, when sp is non-nil (s3 mode), publishes them to the bucket. A nil sp is local-fs
// mode (build locally only -- the orchestrator then boots from the local dir).
func New(sp storage.StorageProvider, localRoot, scriptsDir string) *Builder {
	return &Builder{storage: sp, localRoot: localRoot, scriptsDir: scriptsDir, run: runCmd}
}

// ValidateName reports whether name is a buildable template name (rejects "default" and invalid
// names). The orchestrator calls it to fail a TemplateCreate synchronously rather than as an async
// build failure.
func (b *Builder) ValidateName(name string) error {
	return storage.ValidateBuildable(name)
}

// runCmd executes a command, returning its combined output (for the build log) and an error
// carrying that output's tail on failure.
func runCmd(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s: %w", name, err)
	}
	return string(out), nil
}

// Build builds template `name` from the given Dockerfile contents into its published
// artifact dir, optionally including the warm snapshot. It is synchronous (the caller --
// the orchestrator's TemplateService -- runs it in a goroutine). On a step's failure it
// returns an error carrying that command's output tail, and later steps do not run.
func (b *Builder) Build(buildID, name, dockerfile string, withSnapshot bool) error {
	dir, err := storage.LocalTemplateDir(b.localRoot, name)
	if err != nil {
		return err // rejects "default" / invalid names before running anything
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create template dir: %w", err)
	}

	// 1) Write the recipe to a temp build context and docker build it. The context is the
	//    temp dir itself: this stage's recipes are FROM microsandbox-agent + RUN (no
	//    local-file COPY), so no repo files are needed in the context.
	ctx, err := os.MkdirTemp("", "msbx-build-"+buildID+"-")
	if err != nil {
		return fmt.Errorf("create build context: %w", err)
	}
	defer os.RemoveAll(ctx)
	dockerfilePath := filepath.Join(ctx, "Dockerfile")
	if err := os.WriteFile(dockerfilePath, []byte(dockerfile), 0o644); err != nil {
		return fmt.Errorf("write Dockerfile: %w", err)
	}
	image := "microsandbox-tmpl-" + name
	if out, err := b.run("docker", "build", "-f", dockerfilePath, "-t", image, ctx); err != nil {
		return fmt.Errorf("docker build: %w\n%s", err, tail(out))
	}

	// 2) Export the image to the template's rootfs (build-rootfs.sh injects the daemon).
	rootfs := filepath.Join(dir, "rootfs.ext4")
	if out, err := b.run(filepath.Join(b.scriptsDir, "build-rootfs.sh"), image, rootfs); err != nil {
		return fmt.Errorf("build-rootfs: %w\n%s", err, tail(out))
	}

	// 3) Optionally build the warm snapshot in place. It must be built at the rootfs's final
	//    path -- the snapshot bakes that absolute path in (see storage's package doc) -- so
	//    this writes straight into the published dir, not a staging area.
	if withSnapshot {
		snap := filepath.Join(dir, "snapshot")
		if out, err := b.run(filepath.Join(b.scriptsDir, "build-snapshot.sh"), rootfs, snap); err != nil {
			return fmt.Errorf("build-snapshot: %w\n%s", err, tail(out))
		}
	}

	// 4) Publish to object storage (Stage 15): upload the immutable {buildID}/ artifacts, then flip
	//    the aliases/<name> pointer at this build. Skipped in local-fs mode (no provider) -- there the
	//    orchestrator boots from the local dir we just wrote, which in s3 mode doubles as the
	//    materialize cache (a same-box boot is a cache hit). See docs/STAGE15_DESIGN.md.
	if b.storage != nil {
		if err := b.publish(buildID, name, dir, withSnapshot); err != nil {
			return err
		}
	}
	return nil
}

// publish uploads a freshly built template's artifacts under their immutable {buildID}/ prefix and
// points aliases/<name> at this build. The local file names map to E2B's object names (the local
// snapshot's "vmstate" is uploaded as "snapfile"). The alias is flipped only after every artifact is
// up, so a resolver never sees a half-published build.
func (b *Builder) publish(buildID, name, dir string, withSnapshot bool) error {
	ctx := context.Background()
	uploads := []struct{ local, key string }{
		{filepath.Join(dir, "rootfs.ext4"), storage.ArtifactKey(buildID, storage.RootfsName)},
	}
	if withSnapshot {
		snap := filepath.Join(dir, "snapshot")
		uploads = append(uploads,
			struct{ local, key string }{filepath.Join(snap, "vmstate"), storage.ArtifactKey(buildID, storage.SnapfileName)},
		)
	}
	for _, u := range uploads {
		if err := uploadFile(ctx, b.storage, u.local, u.key); err != nil {
			return fmt.Errorf("upload %s: %w", u.key, err)
		}
	}
	// The memfile is compacted + indexed, not uploaded raw (Stage 17): PublishMemfile uploads
	// {buildID}/memfile (present blocks only) and {buildID}/memfile.header. See docs/STAGE17_DESIGN.md.
	if withSnapshot {
		if err := storage.PublishMemfile(ctx, b.storage, filepath.Join(dir, "snapshot", "memfile"), buildID); err != nil {
			return err
		}
	}
	if err := storage.SetAlias(ctx, b.storage, name, buildID); err != nil {
		return fmt.Errorf("set alias %s -> %s: %w", name, buildID, err)
	}
	return nil
}

// uploadFile streams a local file to key with its size (PutObject needs the content length).
func uploadFile(ctx context.Context, sp storage.StorageProvider, localPath, key string) error {
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

// tail returns the trailing portion of s -- the useful end of a long build log.
func tail(s string) string {
	const max = 2000
	if len(s) <= max {
		return s
	}
	return "...(truncated)...\n" + s[len(s)-max:]
}
