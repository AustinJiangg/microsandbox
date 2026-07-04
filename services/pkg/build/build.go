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
	"strings"

	"microsandbox/services/pkg/storage"
)

// Snapshotter produces a layered template's snapshot by a live-VM re-snapshot (Stage 20): resume the
// base self-consistently, re-snapshot, and publish the child's snapfile + COW memfile diff over the base.
// It is implemented by the orchestrator (which owns firecracker + the network manager); the builder
// depends only on this interface so it stays docker/KVM-free and unit-testable with a fake. nil => no
// live-VM producer, so a layered build that also asks for a snapshot is rejected (a Stage-17 single-build
// memfile is meaningless for a layer -- two independently-booted RAM images differ everywhere).
type Snapshotter interface {
	LayeredSnapshot(ctx context.Context, baseName, baseBuildID, childBuildID string) error
}

// Builder runs the build pipeline for one template at a time. run is the command executor,
// injectable so tests can assert the command sequence without running anything.
type Builder struct {
	storage     storage.StorageProvider // bucket to publish to; nil in local-fs mode (no upload)
	localRoot   string                  // local output/cache root (the orchestrator's vendorDir)
	scriptsDir  string                  // the repo's scripts/; build-rootfs.sh / build-snapshot.sh self-locate REPO_ROOT
	snapshotter Snapshotter             // Stage 20: live-VM producer for a LAYERED snapshot; nil = unavailable
	run         func(name string, args ...string) (string, error)
}

// New returns a Builder that shells out for real: it writes a template's artifacts locally under
// localRoot and, when sp is non-nil (s3 mode), publishes them to the bucket. A nil sp is local-fs
// mode (build locally only -- the orchestrator then boots from the local dir). snapshotter is the
// Stage-20 live-VM producer for layered snapshots (the orchestrator passes itself); nil disables it.
func New(sp storage.StorageProvider, localRoot, scriptsDir string, snapshotter Snapshotter) *Builder {
	return &Builder{storage: sp, localRoot: localRoot, scriptsDir: scriptsDir, snapshotter: snapshotter, run: runCmd}
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
// artifact dir, optionally including the warm snapshot. When base is non-empty the build is
// LAYERED: its rootfs is produced by mutating a copy of the base's rootfs in place and published
// as a copy-on-write diff over it (Stage 19). It is synchronous (the caller -- the orchestrator's
// TemplateService -- runs it in a goroutine). On a step's failure it returns an error carrying
// that command's output tail, and later steps do not run.
func (b *Builder) Build(buildID, name, dockerfile, base string, withSnapshot bool) error {
	dir, err := storage.LocalTemplateDir(b.localRoot, name)
	if err != nil {
		return err // rejects "default" / invalid names before running anything
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create template dir: %w", err)
	}

	// 0) A layered build (base set) publishes its rootfs as a COW diff over the base and produces that
	//    rootfs by mutating a COPY of the base's rootfs in place (Stage 19) -- not by re-mkfs, so there
	//    is no size-pin (the copy is byte-for-byte the base's size). Resolve the base build and the
	//    child's FROM image up front -- before the slow docker build -- and fail early if either is off.
	var baseBuildID, fromImage string
	if base != "" {
		if b.storage == nil {
			return fmt.Errorf("layered build of %q over base %q needs object storage (s3 mode)", name, base)
		}
		if withSnapshot && b.snapshotter == nil {
			return fmt.Errorf("layered snapshot of %q needs the live-VM producer (Stage 20); none configured", name)
		}
		baseBuildID, err = storage.ResolveAlias(context.Background(), b.storage, base)
		if err != nil {
			return fmt.Errorf("resolve base template %q: %w", base, err)
		}
		// The layout-preserving builder diffs the child image against the image it was built FROM, so the
		// child recipe must FROM the base template's image (Decision 3 in docs/STAGE19_DESIGN.md). Parse
		// the recipe's first FROM to pass that image to build-rootfs-layered.sh.
		fromImage, err = firstFromImage(dockerfile)
		if err != nil {
			return fmt.Errorf("layered build of %q: %w", name, err)
		}
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

	// 2) Produce the template's rootfs. A non-layered build exports the image afresh (build-rootfs.sh
	//    injects the daemon + sizes from content). A LAYERED build (Stage 19) instead materializes the
	//    base's rootfs and produces the child as a layout-preserving in-place edit of a COPY of it
	//    (build-rootfs-layered.sh: cp base -> debugfs-apply the child's file delta), so every unchanged
	//    file keeps its exact blocks and the published diff (step 4) is ~the genuine delta rather than
	//    ~half the disk -- the Stage-18 re-mkfs reshuffled the ext4 layout (see docs/STAGE19_DESIGN.md).
	rootfs := filepath.Join(dir, "rootfs.ext4")
	if base == "" {
		if out, err := b.run(filepath.Join(b.scriptsDir, "build-rootfs.sh"), image, rootfs); err != nil {
			return fmt.Errorf("build-rootfs: %w\n%s", err, tail(out))
		}
	} else {
		baseDir, err := os.MkdirTemp("", "msb-layered-base-"+buildID+"-")
		if err != nil {
			return fmt.Errorf("create base rootfs dir: %w", err)
		}
		defer os.RemoveAll(baseDir)
		// Materialize the base's full rootfs (assembling it if the base is itself layered) for the layered
		// builder to copy. PublishRootfsDiff (step 4) materializes it again to diff against; the two copies
		// are byte-identical (deterministic assembly), which is exactly what makes the diff come out as the
		// genuine delta. The extra materialize is a build-time cost, not a hot path.
		baseRootfs := filepath.Join(baseDir, "rootfs.ext4")
		if err := storage.MaterializeLayered(context.Background(), b.storage, baseBuildID, baseRootfs); err != nil {
			return fmt.Errorf("materialize base rootfs %q: %w", baseBuildID, err)
		}
		if out, err := b.run(filepath.Join(b.scriptsDir, "build-rootfs-layered.sh"), image, fromImage, baseRootfs, rootfs); err != nil {
			return fmt.Errorf("build-rootfs-layered: %w\n%s", err, tail(out))
		}
	}

	// 3) Optionally build the warm snapshot. A NON-layered build boots the fresh rootfs and snapshots it in
	//    place (build-snapshot.sh; it bakes the rootfs's absolute path, so it writes into the published dir,
	//    not a staging area). A LAYERED build does NOT boot its own rootfs here: a fresh-boot memfile would
	//    differ from the base everywhere (no COW win). Instead the publish step's live-VM producer (Stage 20)
	//    resumes the base and re-snapshots, storing the memfile as a COW diff over the base.
	if withSnapshot && base == "" {
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
		if err := b.publish(buildID, name, base, dir, baseBuildID, withSnapshot); err != nil {
			return err
		}
	}
	return nil
}

// publish uploads a freshly built template's artifacts under their immutable {buildID}/ prefix and
// points aliases/<name> at this build. The local file names map to E2B's object names (the local
// snapshot's "vmstate" is uploaded as "snapfile"). The alias is flipped only after every artifact is
// up, so a resolver never sees a half-published build.
//
// The rootfs is stored differently depending on baseBuildID: a layered build (baseBuildID set) stores
// only its copy-on-write diff over the base + a flattened {buildID}/rootfs.ext4.header (Stage 18); a
// non-layered build uploads the whole rootfs (Stage 15). The SNAPSHOT diverges the same way (Stage 20): a
// non-layered build uploads its fresh-boot vmstate + compacted memfile whole; a layered build has no local
// snapshot (step 3 skipped it) and instead invokes the live-VM producer, which resumes the base, re-snapshots,
// and stores the memfile as a COW diff over the base. The base name identifies the template to resume.
func (b *Builder) publish(buildID, name, base, dir, baseBuildID string, withSnapshot bool) error {
	ctx := context.Background()
	rootfsLocal := filepath.Join(dir, "rootfs.ext4")
	if baseBuildID != "" {
		if err := storage.PublishRootfsDiff(ctx, b.storage, baseBuildID, rootfsLocal, buildID); err != nil {
			return fmt.Errorf("publish rootfs diff over %s: %w", baseBuildID, err)
		}
	} else if err := uploadFile(ctx, b.storage, rootfsLocal, storage.ArtifactKey(buildID, storage.RootfsName)); err != nil {
		return fmt.Errorf("upload %s: %w", storage.ArtifactKey(buildID, storage.RootfsName), err)
	}
	if withSnapshot {
		if baseBuildID == "" {
			// Non-layered: upload the fresh-boot snapshot whole -- vmstate as {buildID}/snapfile, and the
			// memfile compacted + indexed (Stage 17: present blocks only + {buildID}/memfile.header).
			snap := filepath.Join(dir, "snapshot")
			if err := uploadFile(ctx, b.storage, filepath.Join(snap, "vmstate"), storage.ArtifactKey(buildID, storage.SnapfileName)); err != nil {
				return fmt.Errorf("upload %s: %w", storage.ArtifactKey(buildID, storage.SnapfileName), err)
			}
			if err := storage.PublishMemfile(ctx, b.storage, filepath.Join(snap, "memfile"), buildID); err != nil {
				return err
			}
		} else {
			// Layered: the live-VM producer resumes the base and re-snapshots, publishing {buildID}/snapfile
			// + the COW memfile diff ({buildID}/memfile + .header) + the baked rootfs path (Stage 20). It runs
			// before SetAlias, so a resolver never sees a half-published layered build.
			if err := b.snapshotter.LayeredSnapshot(ctx, base, baseBuildID, buildID); err != nil {
				return fmt.Errorf("layered snapshot over %s: %w", baseBuildID, err)
			}
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

// firstFromImage returns the image named by the recipe's first FROM instruction -- the image the
// layout-preserving layered builder diffs the child against, which must be the base template's image
// (Decision 3 in docs/STAGE19_DESIGN.md). It skips blank/comment lines and any leading ARG, is
// case-insensitive on FROM, and ignores --flag=... options and a trailing "AS <stage>". A recipe with
// no FROM is an error (a layered build has nowhere to diff from). This assumes a single-stage recipe,
// which Decision 3 already constrains.
func firstFromImage(dockerfile string) (string, error) {
	for _, line := range strings.Split(dockerfile, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if !strings.EqualFold(fields[0], "FROM") {
			continue // e.g. a leading ARG before the first FROM
		}
		for _, tok := range fields[1:] {
			if strings.HasPrefix(tok, "--") {
				continue // --platform=... and friends
			}
			return tok, nil // the image reference (the token before any "AS <stage>")
		}
		return "", fmt.Errorf("FROM with no image: %q", line)
	}
	return "", fmt.Errorf("recipe has no FROM instruction")
}

// tail returns the trailing portion of s -- the useful end of a long build log.
func tail(s string) string {
	const max = 2000
	if len(s) <= max {
		return s
	}
	return "...(truncated)...\n" + s[len(s)-max:]
}
