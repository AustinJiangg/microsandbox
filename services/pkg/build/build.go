// Package build is the template builder: it turns a Dockerfile recipe into a template's
// (rootfs, snapshot) artifacts by wrapping the existing shell pipeline -- docker build ->
// build-rootfs.sh -> build-snapshot.sh -- writing them in place via a StorageProvider. It
// is the programmatic equivalent of scripts/build-template.sh (which stays for manual CLI
// use), driven asynchronously by the orchestrator's TemplateService. The command executor
// is injectable so the pipeline is unit-testable without docker / firecracker / KVM.
// See docs/STAGE10_DESIGN.md.
package build

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"microsandbox/services/pkg/storage"
)

// Builder runs the build pipeline for one template at a time. run is the command executor,
// injectable so tests can assert the command sequence without running anything.
type Builder struct {
	storage    storage.StorageProvider
	scriptsDir string // the repo's scripts/; build-rootfs.sh / build-snapshot.sh self-locate REPO_ROOT
	run        func(name string, args ...string) (string, error)
}

// New returns a Builder that shells out for real, writing artifacts through sp.
func New(sp storage.StorageProvider, scriptsDir string) *Builder {
	return &Builder{storage: sp, scriptsDir: scriptsDir, run: runCmd}
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
	dir, err := b.storage.TemplateDir(name)
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
	return nil
}

// tail returns the trailing portion of s -- the useful end of a long build log.
func tail(s string) string {
	const max = 2000
	if len(s) <= max {
		return s
	}
	return "...(truncated)...\n" + s[len(s)-max:]
}
