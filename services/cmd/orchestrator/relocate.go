package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"microsandbox/services/pkg/storage"
)

// Per-sandbox live pause (Stage 26R): checkpoint a RUNNING sandbox to object storage as COW
// diffs over the build it booted from, reusing the Stage 20/22 layered-snapshot producer tail --
// there is no resume-a-base-and-run-a-command preamble because the VM to snapshot is already the
// running sandbox. The checkpoint lives under an api-minted build id (E2B: UpsertSnapshot ->
// SandboxPauseRequest{BuildId}) with the normal artifact shape ({buildID}/{snapfile, memfile +
// .header, rootfs.ext4 + .header, rootfs.path}), so a Resume is just a restore from an explicit
// build id -- nothing new in the read path. See docs/STAGE26R_DESIGN.md.

// pause checkpoints ls under snapBuildID: pause the vCPUs + take a Full snapshot
// (fc.MicroVM.Snapshot), then publish the same artifacts LayeredSnapshot does -- the memfile as a
// COW diff over the build the sandbox booted from, the re-snapshotted vmstate, the rootfs path
// that vmstate bakes, and the writable overlay's dirtied blocks as the rootfs diff. RAM and disk
// are captured at the same paused instant, so the pair is mutually consistent by construction.
// On success the VM is left Paused and the caller destroys it (the checkpoint replaces it); on
// failure the VM is resumed best-effort so a failed checkpoint leaves a running sandbox, not a
// frozen one.
func (s *server) pause(ctx context.Context, ls *liveSandbox, snapBuildID string) (err error) {
	vm, overlay := ls.vm, ls.overlay

	tmp, err := os.MkdirTemp("", "msb-pause-"+snapBuildID+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	// fc.MicroVM.Snapshot pauses the vCPUs before anything else, so every failure from here on may
	// leave the VM paused -- resume it on the way out (best-effort: if even that fails, the sandbox
	// is broken and the user's recourse is delete; we still surface the original error).
	defer func() {
		if err != nil {
			_ = vm.Unpause()
		}
	}()
	vmstate := filepath.Join(tmp, "vmstate")
	memfile := filepath.Join(tmp, "memfile")
	// A Full snapshot faults all guest RAM in over UFFD, so the dumped memfile is complete and its
	// unchanged pages match the base byte-for-byte -- the small-diff precondition (Stage 20).
	if err := vm.Snapshot(vmstate, memfile); err != nil {
		return fmt.Errorf("snapshot sandbox %s: %w", vm.ID, err)
	}

	// The same three publishes as LayeredSnapshot, diffing against the build this sandbox booted
	// from -- the template's alias build at create, or the previous checkpoint after a resume (the
	// flattened v2 header keeps even a pause->resume->pause chain one hop deep for readers).
	if err := storage.PublishMemfileDiff(ctx, s.storage, ls.baseBuildID, memfile, snapBuildID); err != nil {
		return fmt.Errorf("publish memfile diff for %s: %w", snapBuildID, err)
	}
	if err := storage.PublishSnapfile(ctx, s.storage, vmstate, snapBuildID); err != nil {
		return fmt.Errorf("publish snapfile for %s: %w", snapBuildID, err)
	}
	// The re-snapshot bakes the rootfs path THIS VM booted with; record it so Resume binds the
	// checkpoint's NBD device over exactly what the vmstate references (Stage 20's rootfs.path).
	if err := storage.PublishRootfsBakedPath(ctx, s.storage, snapBuildID, ls.bakedRootfsPath); err != nil {
		return fmt.Errorf("record baked rootfs path for %s: %w", snapBuildID, err)
	}

	// The disk half: the overlay's dirtied blocks, consistent with the captured RAM because the
	// vCPUs are paused. Must run before the caller's destroy closes the overlay (backing Close).
	diff, err := overlay.ExportToDiff()
	if err != nil {
		return fmt.Errorf("export rootfs diff for %s: %w", snapBuildID, err)
	}
	if err := storage.PublishRootfsDiffBlocks(ctx, s.storage, ls.baseBuildID, snapBuildID, storage.DiffBlocks{
		Data:      diff.Data,
		Dirty:     diff.Dirty,
		Empty:     diff.Empty,
		BlockSize: diff.BlockSize,
		Size:      overlay.Size(),
	}); err != nil {
		return fmt.Errorf("publish rootfs diff for %s: %w", snapBuildID, err)
	}
	return nil
}
