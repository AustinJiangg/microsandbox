package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"microsandbox/services/pkg/fc"
	"microsandbox/services/pkg/storage"
	"microsandbox/services/pkg/template"
)

// makeSnapshot creates a template's warm snapshot the E2B way -- over the SAME writable NBD block stack
// the snapshot will later be resumed on -- and publishes it into object storage. It is the Go replacement
// for build-snapshot.sh, which boots the base over a plain file (path_on_host=$ROOTFS) and so cannot
// exercise our userspace NBD device. Creating the base snapshot over NBD is Stage 22's chosen fix
// (docs/STAGE22_DESIGN.md §12): it removes the file-backed -> NBD backend transition our restore path took,
// which the bisect pinned as the cause of the writable-re-snapshot virtio-blk panic. It runs as a one-shot
// orchestrator mode (--make-snapshot <name>) and the process exits when it returns.
//
// Steps (mirroring build-snapshot.sh): cold-start the template over NBD at a stable rootfs path
// (spawnHealthy) -> warm the Jupyter kernel (a code-interpreter Execute of `pass`, so a restore skips the
// kernel cold start) -> pause + Full snapshot to temp files (fc.Snapshot) -> publish the vmstate as the
// snapfile, the guest RAM as the Stage-17 compacted+indexed memfile, and the baked rootfs path, all under
// the template's current build id. Requires --nbd + --storage s3 (the whole point is the NBD stack).
func (s *server) makeSnapshot(name string) error {
	if s.storage == nil {
		return fmt.Errorf("--make-snapshot needs object storage (--storage s3)")
	}
	if !s.useNBD {
		return fmt.Errorf("--make-snapshot needs --nbd (the base snapshot is created over the writable NBD stack)")
	}
	tmpl, err := template.Resolve(s.vendorDir, name)
	if err != nil {
		return fmt.Errorf("resolve template %q: %w", name, err)
	}
	ctx := context.Background()
	// The snapshot is keyed under the template's current build (its rootfs must already be seeded there,
	// since the cold start streams it over NBD). --make-snapshot then adds snapfile + memfile + rootfs.path
	// under that same build id, exactly as msb-seed would from a locally-built snapshot.
	buildID, err := storage.ResolveAlias(ctx, s.storage, name)
	if err != nil {
		return fmt.Errorf("resolve build for %q (seed its rootfs first): %w", name, err)
	}

	// Cold-start over NBD at a stable rootfs path (Stage 22 E1: Spawn binds the device over tmpl.Rootfs),
	// health-probed. We own this VM (spawnHealthy does not register it); Destroy it when done.
	vm, err := s.spawnHealthy(tmpl)
	if err != nil {
		return fmt.Errorf("cold-start %q for snapshot: %w", name, err)
	}
	defer vm.Destroy()

	// Warm the Jupyter kernel over the code-interpreter port, so a restore skips the kernel cold start
	// (build-snapshot.sh runs a `pass` for the same reason).
	if err := warmKernel(vm.Slot.Addr(fc.CodeInterpreterTCPPort)); err != nil {
		return fmt.Errorf("warm kernel for %q: %w", name, err)
	}

	// Pause + Full snapshot to temp files, then publish. A Full snapshot over UFFD faults all RAM in, so
	// the memfile is complete; PublishMemfile compacts + indexes it (Stage 17).
	tmp, err := os.MkdirTemp("", "msb-mksnap-"+name+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	vmstate := filepath.Join(tmp, "vmstate")
	memfile := filepath.Join(tmp, "memfile")
	if err := vm.Snapshot(vmstate, memfile); err != nil {
		return fmt.Errorf("snapshot %q: %w", name, err)
	}
	if err := storage.PublishSnapfile(ctx, s.storage, vmstate, buildID); err != nil {
		return fmt.Errorf("publish snapfile: %w", err)
	}
	if err := storage.PublishMemfile(ctx, s.storage, memfile, buildID); err != nil {
		return fmt.Errorf("publish memfile: %w", err)
	}
	// Record the rootfs path the vmstate bakes (tmpl.Rootfs, since Spawn booted path_on_host=tmpl.Rootfs),
	// so a restore binds this build's NBD device over exactly what firecracker will open.
	if err := storage.PublishRootfsBakedPath(ctx, s.storage, buildID, tmpl.Rootfs); err != nil {
		return fmt.Errorf("record baked rootfs path: %w", err)
	}

	// Invalidate the local materialize cache of this snapshot's vmstate. We just rewrote the bucket's
	// snapfile + memfile under the mutable "default" alias, but prepareRestore's storage.Materialize keys
	// its cache on the local file's existence -- so a leftover vmstate from a PREVIOUS --make-snapshot would
	// be reused against this run's freshly-streamed memfile, and Firecracker rejects the mismatched
	// virtqueue state on load (InvalidAvailIdx). Removing it forces the next restore to re-materialize the
	// matching vmstate. (In production a build's artifacts are immutable under its buildID, so this can't
	// arise; --make-snapshot is dev/test glue that republishes a mutable alias.) The memfile is never
	// materialized in s3 mode, but drop a local-fs leftover too. Runs as root here, so a root-owned cache
	// from an earlier root orchestrator is removable.
	_ = os.Remove(filepath.Join(tmpl.SnapshotDir, "vmstate"))
	_ = os.Remove(filepath.Join(tmpl.SnapshotDir, "memfile"))
	return nil
}

// warmKernel drives one code-interpreter Execute of `pass` against the VM's code-interpreter port, reading
// the streamed Connect response to EOF -- which blocks until the cell finishes, i.e. until the Jupyter
// kernel has started. It is the Go port of build-snapshot.sh's Python warm-up (a raw Connect envelope over
// stdlib net/http, no SDK import). Proxy is disabled: the slot address (10.0.<i>.2) is private, and a stray
// HTTP(S)_PROXY in the orchestrator's env would otherwise hijack the loopback dial (the WSL2 proxy trap).
func warmKernel(addr string) error {
	msg, _ := json.Marshal(map[string]any{"code": "pass", "language": "python", "timeoutSeconds": 60})
	// Connect server-streaming envelope: [flags=0][4-byte big-endian length][json].
	var env bytes.Buffer
	env.WriteByte(0)
	_ = binary.Write(&env, binary.BigEndian, uint32(len(msg)))
	env.Write(msg)

	req, err := http.NewRequest(http.MethodPost,
		"http://"+addr+"/codeinterpreter.CodeInterpreterService/Execute", &env)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/connect+json")
	req.Header.Set("Connect-Protocol-Version", "1")

	client := &http.Client{
		Timeout:   90 * time.Second, // the first Execute pays the kernel cold start
		Transport: &http.Transport{Proxy: nil},
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil { // block until the cell (kernel start) finishes
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("warm-up Execute: HTTP %d", resp.StatusCode)
	}
	return nil
}
