// Package fc is the Firecracker microVM lifecycle: cold start from a rootfs, restore
// from a snapshot, and destroy. Ported verbatim from control-plane/microvm.go (Stage
// 8a: relocated; MicroVM.ID and the Spawn/Restore/Destroy entry points are exported now
// that the orchestrator drives them from cmd/orchestrator).
// The host side shells out to the `firecracker` binary -- there is no Go VM library.
package fc

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"microsandbox/services/pkg/network"
	"microsandbox/services/pkg/template"
	"microsandbox/services/pkg/uffd"
)

// Firecracker microVM topology -- must match the rootfs's /init and the snapshot.
// Ported from client.py's _MICROVM_* constants.
const (
	// The daemon's two in-VM TCP ports (E2B's), reached over the VM's NIC at Slot.RoutableIP
	// through the per-sandbox netns DNAT. Must match daemon/main.go's TCP listeners. Stage 12c
	// retired the vsock transport, so these are the daemon's only ports now.
	EnvdTCPPort            = 49983
	CodeInterpreterTCPPort = 49999
	vcpus                  = 1
	memMiB                 = 512 // a Jupyter kernel runs inside the VM; 256 is tight, so give 512

	accessR = 0x4 // R_OK
	accessW = 0x2 // W_OK
)

// MicroVM is a handle to one running Firecracker process and its per-VM working directory
// (config.json / api.sock / console.log). Ported from client.py's _spawn_microvm /
// _restore_microvm / close.
type MicroVM struct {
	ID      string
	proc    *exec.Cmd
	workdir string

	// This VM's network slot (its own netns + TAP + veth + DNAT) -- the data path reaches the
	// daemon at Slot.RoutableIP over TCP. Set on every VM (cold-start + restore); Destroy frees it.
	Slot   *network.Slot
	netMgr *network.Manager

	// uffd is the userfaultfd page-fault handler, set only when this VM was restored over the
	// Uffd memory backend (--uffd, Stage 13b); nil for the File backend and for cold starts.
	// Destroy stops it after firecracker dies, so the memfile mmap + fds don't leak.
	uffd *uffd.Handler

	// rootfsClose tears down this VM's NBD rootfs backing (disconnect the device, return it to the
	// pool, close the base) when set (--nbd, Stage 21c); nil for the legacy materialized-file rootfs.
	// Destroy runs it after firecracker dies, so the device is idle before we disconnect it.
	rootfsClose func() error
}

// RootfsBacking selects how a VM's rootfs drive is provided (Stage 21c). The zero value is the legacy
// path: the drive/snapshot uses the whole rootfs materialized at tmpl.Rootfs. When Device is set, the
// rootfs is served over that NBD device (streamed from object storage by the orchestrator's block stack,
// never materialized whole):
//
//   - Spawn (cold start) binds Device over tmpl.Rootfs (a stable path) in a per-VM mount namespace and
//     boots path_on_host=tmpl.Rootfs -- so the snapshot a cold-started VM produces bakes a stable rootfs
//     path, not a device node like /dev/nbdX that is not stable across VMs (Stage 22 E1). Before Stage 22
//     it pointed path_on_host straight at Device.
//   - Restore cannot retarget the snapshot's baked rootfs path, so it likewise bind-mounts Device over that
//     path inside a per-VM mount namespace -- firecracker then opens the baked path and gets this VM's device.
//
// Close tears the backing down on Destroy (disconnect + return the device + close the base); nil = no-op.
type RootfsBacking struct {
	Device string
	// BakedPath overrides the Restore bind target -- the path the snapshot's drive was baked at. It is
	// normally tmpl.Rootfs (a template restored from its own snapshot), but a Stage-20 layered child's
	// snapshot is a re-snapshot of its BASE, so it bakes the base template's rootfs path, not the child's;
	// the orchestrator sets this to that recorded path so the child's device binds over what FC will open.
	// Empty => bind over tmpl.Rootfs (the default). Ignored by Spawn (a fresh cold-start config).
	BakedPath string
	Close     func() error
}

// bindMount, when set, bind-mounts Device over Target in a per-VM mount namespace before exec'ing
// firecracker -- the Restore-over-NBD trick that makes the snapshot's baked rootfs path resolve to this
// VM's device without rewriting the snapshot (Stage 21c). nil = no mount-namespace wrapping.
type bindMount struct{ device, target string }

// NewID mints a unique sandbox id (no external uuid dependency).
func NewID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "sb_" + hex.EncodeToString(b)
}

// CheckHostArtifacts surfaces template-independent environment problems before
// startup with actionable guidance: the firecracker binary, the guest kernel, and
// an accessible /dev/kvm. The per-template rootfs / snapshot are checked separately
// by Spawn / Restore against the resolved template (Stage 6) -- they now live under
// vendor/templates/<name>/ rather than at one fixed path. Ported from client.py's
// _check_microvm_available.
func CheckHostArtifacts(vendorDir string) error {
	for _, f := range []struct{ path, hint string }{
		{filepath.Join(vendorDir, "firecracker"), "see docs/MICROVM_DESIGN.md §7 for setup"},
		{filepath.Join(vendorDir, "vmlinux"), "see docs/MICROVM_DESIGN.md §7 for setup"},
	} {
		if _, err := os.Stat(f.path); err != nil {
			return fmt.Errorf("missing %s; %s", f.path, f.hint)
		}
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return fmt.Errorf("/dev/kvm does not exist: (nested) hardware virtualization is not enabled")
	}
	if err := syscall.Access("/dev/kvm", accessR|accessW); err != nil {
		return fmt.Errorf("no permission to access /dev/kvm: add the user to the kvm group" +
			" (sudo usermod -aG kvm $USER) and restart WSL")
	}
	return nil
}

// Spawn cold-starts a Firecracker microVM (from the template's rootfs) with the daemon running
// inside, reachable over the VM's NIC (TCP). Ported from client.py's _spawn_microvm.
//
// rootfs selects the rootfs backing (Stage 21c): the zero value uses the materialized file at
// tmpl.Rootfs; a set Device points the drive straight at that NBD device (served from object storage,
// never materialized whole). Spawn owns rootfs.Close -- it runs it on any failure before the VM takes
// ownership, so a half-built VM never leaks the device.
func Spawn(id, vendorDir string, tmpl template.Template, netMgr *network.Manager, rootfs RootfsBacking) (_ *MicroVM, err error) {
	attached := false // once the VM owns rootfs.Close, Destroy tears it down instead of this defer
	defer func() {
		if !attached && rootfs.Close != nil {
			_ = rootfs.Close()
		}
	}()
	if err := CheckHostArtifacts(vendorDir); err != nil {
		return nil, err
	}
	if _, err := os.Stat("/dev/net/tun"); err != nil {
		return nil, fmt.Errorf("/dev/net/tun missing; it is needed for per-sandbox networking" +
			" (Stage 12). See docs/MICROVM_DESIGN.md")
	}
	// The drive always boots from tmpl.Rootfs (a stable path). Over NBD (Stage 22 E1) the device is
	// bind-mounted over that path in a per-VM mount namespace -- exactly as Restore does -- so a cold start
	// and the snapshot it produces bake a stable rootfs path (a device node like /dev/nbdX is not stable
	// across VMs). The materialized-file path (no device) must have the real rootfs at tmpl.Rootfs; the NBD
	// path only needs a placeholder there (the orchestrator's prepareSpawn ensures it) that the bind shadows.
	rootfsPath := tmpl.Rootfs
	var bind *bindMount
	if rootfs.Device != "" {
		bind = &bindMount{device: rootfs.Device, target: tmpl.Rootfs}
	} else if _, err := os.Stat(tmpl.Rootfs); err != nil {
		return nil, fmt.Errorf("missing rootfs %s for template %q; run scripts/build-rootfs.sh"+
			" (or scripts/build-template.sh %s) first", tmpl.Rootfs, tmpl.Name, tmpl.Name)
	}

	// Allocate this VM's network slot -- its own netns with a TAP (the VM's NIC), a veth pair to
	// the host, and a DNAT mapping slot.RoutableIP to the VM's fixed guest IP. firecracker is
	// launched inside that netns below; the daemon is reached at slot.RoutableIP over TCP.
	slot, err := netMgr.Allocate()
	if err != nil {
		return nil, fmt.Errorf("allocate network slot: %w", err)
	}

	workdir, err := os.MkdirTemp("", "microsandbox-vm-")
	if err != nil {
		netMgr.Free(slot)
		return nil, err
	}
	configPath := filepath.Join(workdir, "config.json")

	// Stage 22b: an NBD-backed rootfs is a per-VM private writable overlay (block.Overlay over a shared
	// read-only base), so the guest mounts root rw and its writes land in that VM's own cache -- E2B's
	// model, and what lets the layer producer capture a disk diff. The legacy materialized-file path
	// stays read-only: it is a shared per-template file, so a rw mount would corrupt it across VMs. So
	// writability tracks whether the drive is an NBD device.
	rootMode, readOnly := "ro", true
	if rootfs.Device != "" {
		rootMode, readOnly = "rw", false
	}

	// A single JSON declares the whole VM (--config-file, easy to read at a glance).
	config := map[string]any{
		"boot-source": map[string]any{
			"kernel_image_path": filepath.Join(vendorDir, "vmlinux"),
			// init=/init runs our minimal PID 1, which execs the daemon. The ip= fragment makes the guest
			// kernel configure eth0 at boot (no `ip` in the rootfs). root is rw over NBD (private overlay),
			// else ro (shared file).
			"boot_args": "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda " + rootMode + " init=/init " + network.BootIPArg,
		},
		"drives": []any{map[string]any{
			"drive_id":       "rootfs",
			"path_on_host":   rootfsPath, // tmpl.Rootfs; over NBD the device binds over it (Stage 22 E1)
			"is_root_device": true,
			"is_read_only":   readOnly, // writable over NBD (private overlay); read-only for the shared file
			// io_engine Async (io_uring) matches E2B's rootfs drive (fc/client.go setRootfsDrive). Baked into
			// the cold-start snapshot here, it propagates through --make-snapshot -> the producer's re-snapshot
			// -> the child restore, so the whole chain uses the same block engine E2B does (docs/STAGE22_DESIGN.md
			// §14). The default (Sync) is what a UFFD-restored writable-virtio-blk re-snapshot won't restore on.
			"io_engine": "Async",
		}},
		"machine-config": map[string]any{"vcpu_count": vcpus, "mem_size_mib": memMiB},
		// A virtio-net NIC backed by the netns's TAP -- the daemon's only transport now (Stage 12c
		// retired vsock); the host reaches it at Slot.RoutableIP over TCP.
		"network-interfaces": []any{map[string]any{
			"iface_id":      "eth0",
			"host_dev_name": network.TapDevice,
			"guest_mac":     network.GuestMAC,
		}},
	}
	data, _ := json.Marshal(config)
	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		os.RemoveAll(workdir)
		netMgr.Free(slot)
		return nil, err
	}

	vm, err := startFirecracker(id, vendorDir, workdir, slot.Netns, bind,
		"--api-sock", filepath.Join(workdir, "api.sock"), "--config-file", configPath)
	if err != nil {
		os.RemoveAll(workdir)
		netMgr.Free(slot)
		return nil, err
	}
	vm.Slot = slot
	vm.netMgr = netMgr
	vm.rootfsClose = rootfs.Close // the VM owns the NBD backing now; Destroy tears it down
	attached = true
	return vm, nil
}

// Restore restores a microVM from a pre-generated snapshot (~30ms to ready vs ~0.94s
// cold start). Ported from client.py's _restore_microvm.
//
// The snapshot bakes a fixed eth0 (IP + MAC), so every VM restored from the one snapshot comes
// up identical. To run N of them at once (the warm pool, Stage 5) without an address collision,
// each gets its own network slot (netns) from netMgr and firecracker is launched inside it,
// whose tap0 the snapshot's NIC reattaches to (the tap name is constant across slots; uniqueness
// is the netns). Through Stage 11 this same per-VM isolation was done for the vsock UDS via
// vsock_override; Stage 12 replaced vsock with the netns. See docs/STAGE5_DESIGN.md +
// docs/STAGE12_DESIGN.md.
//
// memSource selects the snapshot memory backend (Stage 15 generalized Stage 13's File/Uffd flag into
// a page source the caller supplies): nil = File (firecracker mmaps the local memfile and the kernel
// demand-pages it, with us outside); non-nil = Uffd (our pkg/uffd handler becomes the VM's memory
// supplier, serving each guest page fault from memSource -- a local mmap in local-fs --uffd mode, or a
// bucket reader streaming pages from object storage in s3 mode). See docs/STAGE13_DESIGN.md +
// docs/STAGE15_DESIGN.md.
func Restore(id, vendorDir string, tmpl template.Template, netMgr *network.Manager, memSource uffd.PageSource, rootfs RootfsBacking) (_ *MicroVM, err error) {
	// We take ownership of memSource: if we fail before handing it to uffd.Serve, close it so the
	// caller's mmap / open object doesn't leak. Once Serve is called (served=true) the handler owns it.
	served := false
	if memSource != nil {
		defer func() {
			if !served {
				_ = memSource.Close()
			}
		}()
	}
	// Likewise take ownership of the NBD rootfs backing: close it on any failure before the VM owns it.
	attached := false
	defer func() {
		if !attached && rootfs.Close != nil {
			_ = rootfs.Close()
		}
	}()
	if err := CheckHostArtifacts(vendorDir); err != nil {
		return nil, err
	}
	if _, err := os.Stat("/dev/net/tun"); err != nil {
		return nil, fmt.Errorf("/dev/net/tun missing; it is needed for per-sandbox networking" +
			" (Stage 12). See docs/MICROVM_DESIGN.md")
	}
	snap := tmpl.SnapshotDir
	vmstate := filepath.Join(snap, "vmstate")
	memfile := filepath.Join(snap, "memfile")
	if _, err := os.Stat(vmstate); err != nil {
		return nil, fmt.Errorf("missing snapshot (%s) for template %q; run scripts/build-snapshot.sh"+
			" (or scripts/build-template.sh %s) first", snap, tmpl.Name, tmpl.Name)
	}
	// The memfile must be local only for the File backend; with a memSource (UFFD) it is streamed
	// from object storage (or already opened by the caller), so we don't require it on disk.
	if memSource == nil {
		if _, err := os.Stat(memfile); err != nil {
			return nil, fmt.Errorf("missing snapshot (%s) for template %q; run scripts/build-snapshot.sh"+
				" (or scripts/build-template.sh %s) first", snap, tmpl.Name, tmpl.Name)
		}
	}
	// The snapshot references its rootfs by the absolute path baked in at build time,
	// so that rootfs must still be present for the load to succeed (Stage 6: it lives
	// under the template's own dir).
	if _, err := os.Stat(tmpl.Rootfs); err != nil {
		return nil, fmt.Errorf("missing rootfs %s for template %q (the snapshot references it);"+
			" rebuild with scripts/build-template.sh %s", tmpl.Rootfs, tmpl.Name, tmpl.Name)
	}

	// Each restored VM gets its own network slot, just like Spawn -- the snapshot bakes a
	// configured eth0, and every VM restored from it comes up with the SAME guest IP/MAC, so each
	// must live in its own netns to coexist (Stage 5 did the same per-VM trick for the vsock UDS).
	slot, err := netMgr.Allocate()
	if err != nil {
		return nil, fmt.Errorf("allocate network slot: %w", err)
	}

	workdir, err := os.MkdirTemp("", "microsandbox-vm-")
	if err != nil {
		netMgr.Free(slot)
		return nil, err
	}
	apiSock := filepath.Join(workdir, "api.sock")

	// firecracker runs inside the slot's netns so the snapshot's NIC reattaches to that netns's
	// tap0. The api.sock lives in workdir on the host fs, so the netns doesn't isolate it -- the
	// orchestrator still reaches it. Over NBD (Stage 21c) it also runs in a per-VM mount namespace that
	// binds the device over the snapshot's baked rootfs path, so /snapshot/load opens this VM's device.
	var bind *bindMount
	if rootfs.Device != "" {
		target := tmpl.Rootfs
		if rootfs.BakedPath != "" { // a layered child bakes its base's path, not tmpl.Rootfs (Stage 20)
			target = rootfs.BakedPath
		}
		bind = &bindMount{device: rootfs.Device, target: target}
	}
	vm, err := startFirecracker(id, vendorDir, workdir, slot.Netns, bind, "--api-sock", apiSock)
	if err != nil {
		os.RemoveAll(workdir)
		netMgr.Free(slot)
		return nil, err
	}
	// Record the slot + NBD backing now (before the load) so a load failure's vm.Destroy() frees them.
	vm.Slot = slot
	vm.netMgr = netMgr
	vm.rootfsClose = rootfs.Close
	attached = true

	// Choose the snapshot memory backend (Stage 15 generalized Stage 13's File/Uffd switch into the
	// caller's page-source choice). memSource == nil => File: firecracker mmaps the local memfile and
	// the kernel demand-pages it, with us on the outside. memSource != nil => Uffd: our pkg/uffd
	// handler becomes the VM's memory supplier, serving each guest page fault from memSource -- a local
	// mmap (--uffd) or a bucket reader streaming pages from object storage (s3 mode). See
	// docs/STAGE13_DESIGN.md + docs/STAGE15_DESIGN.md.
	memBackend := map[string]any{"backend_type": "File", "backend_path": memfile}
	if memSource != nil {
		// The handler must be listening BEFORE /snapshot/load: firecracker connects to its socket
		// during the load to hand over the uffd fd + guest layout. uffd.Serve takes ownership of
		// memSource (closes it on its own failure, and on Destroy via the handler).
		udsPath := filepath.Join(workdir, "uffd.sock")
		served = true
		h, herr := uffd.Serve(udsPath, memSource)
		if herr != nil {
			vm.Destroy()
			return nil, fmt.Errorf("start uffd handler: %w", herr)
		}
		vm.uffd = h
		memBackend = map[string]any{"backend_type": "Uffd", "backend_path": udsPath}
	}

	// Snapshot load + resume can't go through --config-file, so use the REST API. Load PAUSED
	// (resume_vm:false), then resume in a SEPARATE PATCH /vm {Resumed} below -- E2B's exact sequence
	// (loadSnapshot ResumeVM:false -> resumeVM, fc/client.go). This matters for the Stage-22 producer:
	// a load-with-resume kicks the virtio devices during the load itself, before the block device's
	// serialized queue state is fully reconciled, which leaves a UFFD-restored writable virtio-blk VM in
	// a state whose re-snapshot won't restore (InvalidAvailIdx; docs/STAGE22_DESIGN.md §14). Loading
	// paused lets firecracker restore the full device state first; the separate resume then kicks the
	// queues from a consistent point. It is also the right order for UFFD (load hands over the fd, we
	// confirm the handshake, then resume so the first faults are served by a ready handler).
	status, err := firecrackerAPI(apiSock, "PUT", "/snapshot/load", map[string]any{
		"snapshot_path": vmstate,
		"mem_backend":   memBackend,
		"resume_vm":     false,
	}, 15*time.Second)
	if err != nil || (status != 200 && status != 204) {
		tail := vm.ConsoleTail()
		vm.Destroy()
		return nil, fmt.Errorf("snapshot/load failed: status=%d err=%v; %s", status, err, tail)
	}
	// With UFFD the handshake (fd + layout) completes during the load above. If the handler hit a
	// fatal error receiving it, the guest would hang on its first page fault, so surface it now
	// rather than waiting for the health probe to time out (Decision 3).
	if vm.uffd != nil {
		if herr := vm.uffd.Err(); herr != nil {
			tail := vm.ConsoleTail()
			vm.Destroy()
			return nil, fmt.Errorf("uffd handler failed during snapshot load: %w; %s", herr, tail)
		}
	}
	// Resume the loaded-paused VM (E2B's separate resumeVM step). Only now do the vcpus run and the
	// devices get kicked -- from the fully-restored device state.
	rstatus, rerr := firecrackerAPI(apiSock, "PATCH", "/vm", map[string]any{"state": "Resumed"}, 15*time.Second)
	if rerr != nil || (rstatus != 200 && rstatus != 204) {
		tail := vm.ConsoleTail()
		vm.Destroy()
		return nil, fmt.Errorf("resume after snapshot/load failed: status=%d err=%v; %s", rstatus, rerr, tail)
	}
	return vm, nil
}

// startFirecracker launches the firecracker process with the given args, wiring
// its stdout/stderr (the guest serial console) to workdir/console.log. We can't
// use a pipe: the guest console writes continuously and a full pipe buffer would
// stall the VM, so it lands in a file we can tail for diagnostics.
//
// The launch is composed of up to three nested exec-only wrappers, so cmd.Process stays firecracker
// (SIGTERM/Wait below still target the VM): `ip netns exec <netns>` enters the sandbox's netns (the TAP
// it opens is that namespace's), then -- over NBD (bind != nil) -- `unshare --mount` gives it a private
// mount namespace where `mount --bind <device> <baked path>` makes the snapshot's rootfs path resolve to
// this VM's NBD device, then `exec firecracker`. `ip netns exec` / `unshare` / the bind need CAP_*; the
// orchestrator runs as root (Stage 12 Decision 7).
func startFirecracker(id, vendorDir, workdir, netns string, bind *bindMount, args ...string) (*MicroVM, error) {
	console, err := os.Create(filepath.Join(workdir, "console.log"))
	if err != nil {
		return nil, err
	}
	fcPath := filepath.Join(vendorDir, "firecracker")
	launch := append([]string{fcPath}, args...)
	if bind != nil {
		// Bind the device over the baked path in a private mount ns, then exec firecracker in it. sh's
		// `exec` keeps the PID, so the whole chain remains a single process ending in firecracker.
		script := "mount --bind " + shellQuote(bind.device) + " " + shellQuote(bind.target) +
			" && exec " + shellJoin(launch)
		launch = []string{"unshare", "--mount", "--propagation", "private", "sh", "-c", script}
	}
	if netns != "" {
		launch = append([]string{"ip", "netns", "exec", netns}, launch...)
	}
	cmd := exec.Command(launch[0], launch[1:]...)
	cmd.Stdout = console
	cmd.Stderr = console
	err = cmd.Start()
	console.Close() // the parent's copy; the child keeps its own dup'd fd and keeps writing
	if err != nil {
		return nil, err
	}
	return &MicroVM{ID: id, proc: cmd, workdir: workdir}, nil
}

// shellQuote single-quotes s for safe use inside the `sh -c` script that wraps firecracker over NBD
// (paths are ours -- tmp dirs, /dev/nbdX -- but quoting is correct hygiene). shellJoin quotes+joins argv.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func shellJoin(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = shellQuote(a)
	}
	return strings.Join(q, " ")
}

// Destroy kills the firecracker process (which destroys the whole VM -- memory
// and device state vanish with the process) and removes the working directory.
// Ported from client.py's close().
func (vm *MicroVM) Destroy() {
	if vm.proc != nil && vm.proc.Process != nil {
		_ = vm.proc.Process.Signal(syscall.SIGTERM)
		done := make(chan error, 1)
		go func() { done <- vm.proc.Wait() }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = vm.proc.Process.Kill()
			<-done
		}
	}
	// Stop the UFFD handler if this VM was restored over the Uffd backend (Stage 13b). firecracker
	// is dead now, so its uffd has already hit EOF; Stop also wakes the loop deterministically,
	// waits for it to munmap the memfile, and removes the socket -- no fd/mapping leaks across the
	// warm pool's churn (Decision 5). nil (the File backend / cold start) makes this a no-op.
	if vm.uffd != nil {
		vm.uffd.Stop()
	}
	// Tear down the NBD rootfs backing after firecracker is gone (so the device is idle): disconnect it,
	// return it to the pool, close the base. nil (legacy materialized rootfs) makes this a no-op. The
	// per-VM mount ns + its bind vanished when firecracker (its last process) exited, so no unmount.
	if vm.rootfsClose != nil {
		_ = vm.rootfsClose()
	}
	if vm.workdir != "" {
		os.RemoveAll(vm.workdir)
	}
	// Tear down the network slot (netns/veth/TAP/DNAT) after the VM is gone -- done last, so
	// firecracker has already released the netns's TAP. Every VM has a slot since Stage 12.
	if vm.Slot != nil && vm.netMgr != nil {
		vm.netMgr.Free(vm.Slot)
	}
}

// Snapshot pauses the VM and writes a Full Firecracker snapshot -- device/CPU state to vmstatePath and
// guest RAM to memfilePath. It is the live-VM re-snapshot capability the Stage-20 memfile-COW producer
// needs: the orchestrator restores a base template self-consistently, calls Snapshot, then diffs the
// resulting memfile against the base (docs/STAGE20_DESIGN.md). Ported from build-snapshot.sh's PATCH /vm
// + PUT /snapshot/create, which until now lived only in shell (fc.Restore only *loaded* snapshots).
//
// A Full snapshot reads ALL guest memory, so a VM restored over UFFD faults every page in through our
// handler while the snapshot is written -- the produced memfile is therefore complete, and its unchanged
// pages match the base byte-for-byte (the small-diff precondition COW relies on). The VM is left Paused;
// the caller Destroys it. The create timeout is generous: writing ~512 MiB (and faulting it in from the
// bucket over UFFD in s3 mode) is slower than a control call.
func (vm *MicroVM) Snapshot(vmstatePath, memfilePath string) error {
	apiSock := filepath.Join(vm.workdir, "api.sock")
	if status, err := firecrackerAPI(apiSock, "PATCH", "/vm",
		map[string]any{"state": "Paused"}, 15*time.Second); err != nil || (status != 200 && status != 204) {
		return fmt.Errorf("pause VM for snapshot: status=%d err=%v", status, err)
	}
	if status, err := firecrackerAPI(apiSock, "PUT", "/snapshot/create", map[string]any{
		"snapshot_type": "Full",
		"snapshot_path": vmstatePath,
		"mem_file_path": memfilePath,
	}, 120*time.Second); err != nil || (status != 200 && status != 204) {
		return fmt.Errorf("create snapshot: status=%d err=%v", status, err)
	}
	return nil
}

// ConsoleTail grabs the tail of the guest serial log, for startup-failure
// diagnostics only. Ported from client.py's _microvm_log.
func (vm *MicroVM) ConsoleTail() string {
	data, err := os.ReadFile(filepath.Join(vm.workdir, "console.log"))
	if err != nil {
		return ""
	}
	s := strings.TrimSpace(string(data))
	if len(s) > 1500 {
		s = s[len(s)-1500:]
	}
	return s
}

// firecrackerAPI sends one request to Firecracker's REST API (HTTP over the
// api.sock unix socket), returning the status code. The socket may not exist yet
// right after the process starts, so connection failures are retried until the
// timeout. Ported from client.py's _firecracker_api.
func firecrackerAPI(sockPath, method, path string, body any, timeout time.Duration) (int, error) {
	payload, _ := json.Marshal(body)
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sockPath)
			},
		},
	}
	deadline := time.Now().Add(timeout)
	for {
		req, err := http.NewRequest(method, "http://localhost"+path, bytes.NewReader(payload))
		if err != nil {
			return 0, err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			if time.Now().After(deadline) {
				return 0, err
			}
			time.Sleep(5 * time.Millisecond)
			continue
		}
		resp.Body.Close()
		return resp.StatusCode, nil
	}
}
