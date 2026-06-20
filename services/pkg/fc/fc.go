// Package fc is the Firecracker microVM lifecycle: cold start from a rootfs, restore
// from a snapshot, and destroy. Ported verbatim from control-plane/microvm.go (Stage
// 8a: relocated; MicroVM.ID / MicroVM.UDSPath and the Spawn/Restore/Destroy entry
// points are exported now that the orchestrator drives them from cmd/orchestrator).
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
)

// Firecracker microVM topology -- must match the rootfs's /init and the snapshot.
// Ported from client.py's _MICROVM_* constants.
const (
	VsockPort = 1024 // envd's vsock port inside the VM (exported: callers probe/proxy it)
	// CodeInterpreterVsockPort is the second in-VM service (Stage 11): the code-interpreter
	// (the stateful Python kernel). Firecracker multiplexes both ports onto the one vsock
	// UDS via CONNECT <port>; the orchestrator routes /codeinterpreter.* here.
	CodeInterpreterVsockPort = 1025
	// Stage 12a: the daemon ALSO listens on these TCP ports (E2B's), reachable over the VM's
	// NIC at Slot.RoutableIP, alongside vsock. Must match daemon/main.go's TCP listeners.
	EnvdTCPPort            = 49983
	CodeInterpreterTCPPort = 49999
	guestCID               = 3 // guest's vsock CID (host is fixed at 2)
	vcpus                  = 1
	memMiB                 = 512 // a Jupyter kernel runs inside the VM; 256 is tight, so give 512

	accessR = 0x4 // R_OK
	accessW = 0x2 // W_OK
)

// MicroVM is a handle to one running Firecracker process and its per-VM working
// directory (config.json / api.sock / console.log / -- for cold start -- the
// vsock UDS). Ported from client.py's _spawn_microvm / _restore_microvm / close.
type MicroVM struct {
	ID      string
	proc    *exec.Cmd
	workdir string
	UDSPath string // Firecracker multiplexes the guest vsock onto this UDS; the data proxy connects to it

	// Stage 12a: this VM's network slot (its own netns + TAP + veth + DNAT), set on the
	// cold-start (Spawn) path. nil on the vsock-only restore path. Destroy frees it.
	Slot   *network.Slot
	netMgr *network.Manager
}

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

// Spawn cold-starts a Firecracker microVM (from the template's rootfs) with the daemon
// running inside, exposed via vsock. Ported from client.py's _spawn_microvm.
func Spawn(id, vendorDir string, tmpl template.Template, netMgr *network.Manager) (*MicroVM, error) {
	if err := CheckHostArtifacts(vendorDir); err != nil {
		return nil, err
	}
	if _, err := os.Stat("/dev/net/tun"); err != nil {
		return nil, fmt.Errorf("/dev/net/tun missing; it is needed for per-sandbox networking" +
			" (Stage 12). See docs/MICROVM_DESIGN.md")
	}
	if _, err := os.Stat(tmpl.Rootfs); err != nil {
		return nil, fmt.Errorf("missing rootfs %s for template %q; run scripts/build-rootfs.sh"+
			" (or scripts/build-template.sh %s) first", tmpl.Rootfs, tmpl.Name, tmpl.Name)
	}

	// Stage 12a: allocate this VM's network slot -- its own netns with a TAP (the VM's NIC),
	// a veth pair to the host, and a DNAT mapping slot.RoutableIP to the VM's fixed guest IP.
	// firecracker is launched inside that netns below; vsock is kept alongside (additive).
	slot, err := netMgr.Allocate()
	if err != nil {
		return nil, fmt.Errorf("allocate network slot: %w", err)
	}

	workdir, err := os.MkdirTemp("", "microsandbox-vm-")
	if err != nil {
		netMgr.Free(slot)
		return nil, err
	}
	uds := filepath.Join(workdir, "fc.vsock")
	configPath := filepath.Join(workdir, "config.json")

	// A single JSON declares the whole VM (--config-file, easy to read at a glance).
	config := map[string]any{
		"boot-source": map[string]any{
			"kernel_image_path": filepath.Join(vendorDir, "vmlinux"),
			// read-only root; init=/init runs our minimal PID 1, which execs the daemon. The
			// ip= fragment makes the guest kernel configure eth0 at boot (no `ip` in the rootfs).
			"boot_args": "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda ro init=/init " + network.BootIPArg,
		},
		"drives": []any{map[string]any{
			"drive_id":       "rootfs",
			"path_on_host":   tmpl.Rootfs,
			"is_root_device": true,
			"is_read_only":   true, // all writes go to the in-VM tmpfs /tmp
		}},
		"machine-config": map[string]any{"vcpu_count": vcpus, "mem_size_mib": memMiB},
		// Stage 12a: a virtio-net NIC backed by the netns's TAP, so the host can reach the VM
		// over TCP. vsock stays too (additive) -- the data path doesn't move until 12b.
		"network-interfaces": []any{map[string]any{
			"iface_id":      "eth0",
			"host_dev_name": network.TapDevice,
			"guest_mac":     network.GuestMAC,
		}},
		"vsock": map[string]any{"guest_cid": guestCID, "uds_path": uds},
	}
	data, _ := json.Marshal(config)
	if err := os.WriteFile(configPath, data, 0o644); err != nil {
		os.RemoveAll(workdir)
		netMgr.Free(slot)
		return nil, err
	}

	vm, err := startFirecracker(id, vendorDir, workdir, uds, slot.Netns,
		"--api-sock", filepath.Join(workdir, "api.sock"), "--config-file", configPath)
	if err != nil {
		os.RemoveAll(workdir)
		netMgr.Free(slot)
		return nil, err
	}
	vm.Slot = slot
	vm.netMgr = netMgr
	return vm, nil
}

// Restore restores a microVM from a pre-generated snapshot (~30ms to ready vs ~0.94s
// cold start). Ported from client.py's _restore_microvm.
//
// The snapshot bakes in a fixed vsock uds path (vendor/snapshot/fc.vsock), which
// alone would limit us to one restore at a time. We override it per-VM at load
// time via Firecracker v1.16.0's vsock_override, so N VMs can be restored from the
// one snapshot -- each listening on its own workdir/fc.vsock. This is the basis for
// the warm pool (Stage 5). See docs/STAGE5_DESIGN.md.
func Restore(id, vendorDir string, tmpl template.Template) (*MicroVM, error) {
	if err := CheckHostArtifacts(vendorDir); err != nil {
		return nil, err
	}
	snap := tmpl.SnapshotDir
	vmstate := filepath.Join(snap, "vmstate")
	memfile := filepath.Join(snap, "memfile")
	if _, err := os.Stat(vmstate); err != nil {
		return nil, fmt.Errorf("missing snapshot (%s) for template %q; run scripts/build-snapshot.sh"+
			" (or scripts/build-template.sh %s) first", snap, tmpl.Name, tmpl.Name)
	}
	if _, err := os.Stat(memfile); err != nil {
		return nil, fmt.Errorf("missing snapshot (%s) for template %q; run scripts/build-snapshot.sh"+
			" (or scripts/build-template.sh %s) first", snap, tmpl.Name, tmpl.Name)
	}
	// The snapshot references its rootfs by the absolute path baked in at build time,
	// so that rootfs must still be present for the load to succeed (Stage 6: it lives
	// under the template's own dir).
	if _, err := os.Stat(tmpl.Rootfs); err != nil {
		return nil, fmt.Errorf("missing rootfs %s for template %q (the snapshot references it);"+
			" rebuild with scripts/build-template.sh %s", tmpl.Rootfs, tmpl.Name, tmpl.Name)
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

	// Restore stays vsock-only this sub-step (a snapshot can't gain a NIC it was not captured
	// with), so it gets no netns -- pass "". 12b rebuilds the snapshot with eth0 and a slot.
	vm, err := startFirecracker(id, vendorDir, workdir, uds, "", "--api-sock", apiSock)
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
		tail := vm.ConsoleTail()
		vm.Destroy()
		return nil, fmt.Errorf("snapshot/load failed: status=%d err=%v; %s", status, err, tail)
	}
	return vm, nil
}

// startFirecracker launches the firecracker process with the given args, wiring
// its stdout/stderr (the guest serial console) to workdir/console.log. We can't
// use a pipe: the guest console writes continuously and a full pipe buffer would
// stall the VM, so it lands in a file we can tail for diagnostics.
func startFirecracker(id, vendorDir, workdir, uds, netns string, args ...string) (*MicroVM, error) {
	console, err := os.Create(filepath.Join(workdir, "console.log"))
	if err != nil {
		return nil, err
	}
	fcPath := filepath.Join(vendorDir, "firecracker")
	var cmd *exec.Cmd
	if netns != "" {
		// Stage 12a: launch firecracker inside the sandbox's netns, so the TAP it opens is the
		// one in that namespace. `ip netns exec` needs CAP_NET_ADMIN -- the orchestrator runs as
		// root (Decision 7). It execs (no fork) into firecracker, so cmd.Process is the VM and
		// SIGTERM/Wait below still target it. Restore passes "" (vsock-only this sub-step).
		cmd = exec.Command("ip", append([]string{"netns", "exec", netns, fcPath}, args...)...)
	} else {
		cmd = exec.Command(fcPath, args...)
	}
	cmd.Stdout = console
	cmd.Stderr = console
	err = cmd.Start()
	console.Close() // the parent's copy; the child keeps its own dup'd fd and keeps writing
	if err != nil {
		return nil, err
	}
	return &MicroVM{ID: id, proc: cmd, workdir: workdir, UDSPath: uds}, nil
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
	if vm.workdir != "" {
		os.RemoveAll(vm.workdir)
	}
	// Stage 12a: tear down the network slot (netns/veth/TAP/DNAT) after the VM is gone. nil on
	// the vsock-only restore path. Done last, so firecracker has released the netns's TAP.
	if vm.Slot != nil && vm.netMgr != nil {
		vm.netMgr.Free(vm.Slot)
	}
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
