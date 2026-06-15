package main

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
)

// Firecracker microVM topology -- must match the rootfs's /init and the snapshot.
// Ported from client.py's _MICROVM_* constants.
const (
	vsockPort = 1024 // vsock port the daemon listens on inside the VM
	guestCID  = 3    // guest's vsock CID (host is fixed at 2)
	vcpus     = 1
	memMiB    = 512 // a Jupyter kernel runs inside the VM; 256 is tight, so give 512

	accessR = 0x4 // R_OK
	accessW = 0x2 // W_OK
)

// microVM is a handle to one running Firecracker process and its per-VM working
// directory (config.json / api.sock / console.log / -- for cold start -- the
// vsock UDS). Ported from client.py's _spawn_microvm / _restore_microvm / close.
type microVM struct {
	id      string
	proc    *exec.Cmd
	workdir string
	udsPath string // Firecracker multiplexes the guest vsock onto this UDS; the SDK connects to it
}

// newID mints a unique sandbox id (no external uuid dependency).
func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "sb_" + hex.EncodeToString(b)
}

// checkHostArtifacts surfaces template-independent environment problems before
// startup with actionable guidance: the firecracker binary, the guest kernel, and
// an accessible /dev/kvm. The per-template rootfs / snapshot are checked separately
// by spawnMicroVM / restoreMicroVM against the resolved template (Stage 6) -- they
// now live under vendor/templates/<name>/ rather than at one fixed path. Ported from
// client.py's _check_microvm_available.
func checkHostArtifacts(vendorDir string) error {
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

// spawnMicroVM cold-starts a Firecracker microVM (from the template's rootfs) with
// the daemon running inside, exposed via vsock. Ported from client.py's _spawn_microvm.
func spawnMicroVM(id, vendorDir string, tmpl template) (*microVM, error) {
	if err := checkHostArtifacts(vendorDir); err != nil {
		return nil, err
	}
	if _, err := os.Stat(tmpl.rootfs); err != nil {
		return nil, fmt.Errorf("missing rootfs %s for template %q; run scripts/build-rootfs.sh"+
			" (or scripts/build-template.sh %s) first", tmpl.rootfs, tmpl.name, tmpl.name)
	}
	workdir, err := os.MkdirTemp("", "microsandbox-vm-")
	if err != nil {
		return nil, err
	}
	uds := filepath.Join(workdir, "fc.vsock")
	configPath := filepath.Join(workdir, "config.json")

	// A single JSON declares the whole VM (--config-file, easy to read at a glance).
	config := map[string]any{
		"boot-source": map[string]any{
			"kernel_image_path": filepath.Join(vendorDir, "vmlinux"),
			// read-only root; init=/init runs our minimal PID 1, which execs the daemon.
			"boot_args": "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda ro init=/init",
		},
		"drives": []any{map[string]any{
			"drive_id":       "rootfs",
			"path_on_host":   tmpl.rootfs,
			"is_root_device": true,
			"is_read_only":   true, // all writes go to the in-VM tmpfs /tmp
		}},
		"machine-config": map[string]any{"vcpu_count": vcpus, "mem_size_mib": memMiB},
		// no network-interfaces: the sandbox is fully offline; management flows over vsock.
		"vsock": map[string]any{"guest_cid": guestCID, "uds_path": uds},
	}
	data, _ := json.Marshal(config)
	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		os.RemoveAll(workdir)
		return nil, err
	}

	vm, err := startFirecracker(id, vendorDir, workdir, uds,
		"--api-sock", filepath.Join(workdir, "api.sock"), "--config-file", configPath)
	if err != nil {
		os.RemoveAll(workdir)
		return nil, err
	}
	return vm, nil
}

// restoreMicroVM restores a microVM from a pre-generated snapshot (~30ms to
// ready vs ~0.94s cold start). Ported from client.py's _restore_microvm.
//
// The snapshot bakes in a fixed vsock uds path (vendor/snapshot/fc.vsock), which
// alone would limit us to one restore at a time. We override it per-VM at load
// time via Firecracker v1.16.0's vsock_override, so N VMs can be restored from the
// one snapshot -- each listening on its own workdir/fc.vsock. This is the basis for
// the warm pool (Stage 5). See docs/STAGE5_DESIGN.md.
func restoreMicroVM(id, vendorDir string, tmpl template) (*microVM, error) {
	if err := checkHostArtifacts(vendorDir); err != nil {
		return nil, err
	}
	snap := tmpl.snapshotDir
	vmstate := filepath.Join(snap, "vmstate")
	memfile := filepath.Join(snap, "memfile")
	if _, err := os.Stat(vmstate); err != nil {
		return nil, fmt.Errorf("missing snapshot (%s) for template %q; run scripts/build-snapshot.sh"+
			" (or scripts/build-template.sh %s) first", snap, tmpl.name, tmpl.name)
	}
	if _, err := os.Stat(memfile); err != nil {
		return nil, fmt.Errorf("missing snapshot (%s) for template %q; run scripts/build-snapshot.sh"+
			" (or scripts/build-template.sh %s) first", snap, tmpl.name, tmpl.name)
	}
	// The snapshot references its rootfs by the absolute path baked in at build time,
	// so that rootfs must still be present for the load to succeed (Stage 6: it lives
	// under the template's own dir).
	if _, err := os.Stat(tmpl.rootfs); err != nil {
		return nil, fmt.Errorf("missing rootfs %s for template %q (the snapshot references it);"+
			" rebuild with scripts/build-template.sh %s", tmpl.rootfs, tmpl.name, tmpl.name)
	}

	workdir, err := os.MkdirTemp("", "microsandbox-vm-")
	if err != nil {
		return nil, err
	}
	apiSock := filepath.Join(workdir, "api.sock")
	// This VM's own vsock socket, inside its private workdir. We override the path
	// baked into the snapshot at load time (below), so concurrent restores never
	// collide on a shared socket.
	uds := filepath.Join(workdir, "fc.vsock")

	vm, err := startFirecracker(id, vendorDir, workdir, uds, "--api-sock", apiSock)
	if err != nil {
		os.RemoveAll(workdir)
		return nil, err
	}

	// Snapshot load + resume can't go through --config-file, so use the REST API.
	status, err := firecrackerAPI(apiSock, "PUT", "/snapshot/load", map[string]any{
		"snapshot_path":  vmstate,
		"mem_backend":    map[string]any{"backend_type": "File", "backend_path": memfile},
		"vsock_override": map[string]any{"uds_path": uds}, // v1.16.0: per-VM uds, overriding the snapshot's baked path
		"resume_vm":      true,
	}, 15*time.Second)
	if err != nil || (status != 200 && status != 204) {
		tail := vm.consoleTail()
		vm.destroy()
		return nil, fmt.Errorf("snapshot/load failed: status=%d err=%v; %s", status, err, tail)
	}
	return vm, nil
}

// startFirecracker launches the firecracker process with the given args, wiring
// its stdout/stderr (the guest serial console) to workdir/console.log. We can't
// use a pipe: the guest console writes continuously and a full pipe buffer would
// stall the VM, so it lands in a file we can tail for diagnostics.
func startFirecracker(id, vendorDir, workdir, uds string, args ...string) (*microVM, error) {
	console, err := os.Create(filepath.Join(workdir, "console.log"))
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(filepath.Join(vendorDir, "firecracker"), args...)
	cmd.Stdout = console
	cmd.Stderr = console
	err = cmd.Start()
	console.Close() // the parent's copy; the child keeps its own dup'd fd and keeps writing
	if err != nil {
		return nil, err
	}
	return &microVM{id: id, proc: cmd, workdir: workdir, udsPath: uds}, nil
}

// destroy kills the firecracker process (which destroys the whole VM -- memory
// and device state vanish with the process) and removes the working directory.
// Ported from client.py's close().
func (vm *microVM) destroy() {
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
	if vm.workdir != "" {
		os.RemoveAll(vm.workdir)
	}
}

// consoleTail grabs the tail of the guest serial log, for startup-failure
// diagnostics only. Ported from client.py's _microvm_log.
func (vm *microVM) consoleTail() string {
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
