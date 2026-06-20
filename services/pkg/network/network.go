// Package network gives each sandbox microVM its own host-side network identity: a TAP
// device the VM's virtio-net attaches to, inside a dedicated network namespace, bridged to
// the host root namespace by a veth pair, with a DNAT that maps a unique per-slot routable
// address to the VM's (fixed) IP. The orchestrator dials Slot.RoutableIP:<port> and the
// netns forwards it to the VM.
//
// Why a netns per VM? A microVM snapshot freezes the whole guest, including its network
// config -- every VM restored from one snapshot comes up with the SAME eth0 IP and MAC. To
// run N of them at once (the warm pool) without an address collision, each lives in its own
// netns, where identical guest addresses don't conflict; uniqueness lives host-side in the
// per-slot veth address + DNAT. This is the exact structural twin of Stage 5's vsock_override
// (the snapshot baked a fixed vsock UDS path, so each restored VM got its own UDS): same
// problem, same shape of fix. See docs/STAGE12_DESIGN.md (Decision 1).
//
// Setup shells out to `ip`/`iptables` and so needs CAP_NET_ADMIN in the host root namespace
// (the orchestrator runs as root -- Decision 7). The command executor is injectable so the
// slot's name/address derivation and command plan are unit-testable without root or a VM,
// mirroring pkg/build.
package network

import (
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// VM-side network constants. These are IDENTICAL in every sandbox's netns (and baked into
// the snapshot), which is exactly what the per-VM netns makes safe (Decision 1). The guest
// kernel configures eth0 from BootIPArg at boot -- no `ip` binary needed in the minimal
// rootfs (the same constraint that makes daemon/loopback.go raise lo via a raw ioctl).
const (
	vmIP      = "169.254.0.21"      // the VM's eth0 (constant); 169.254.0.20/30 holds it + the gateway
	vmGateway = "169.254.0.22"      // the netns-side TAP address = the VM's default gateway
	vmPrefix  = "30"                // a point-to-point /30 per link
	vmNetmask = "255.255.255.252"   // /30 as a dotted mask, for the kernel ip= boot arg
	vmMAC     = "06:00:AC:10:00:15" // a fixed locally-administered MAC (constant)
	tapName   = "tap0"              // the TAP inside each netns (constant; firecracker runs in the netns)
)

// BootIPArg is the kernel command-line fragment that configures the guest's eth0 at boot,
// of the form ip=<client>:<server>:<gw>:<netmask>:<host>:<dev>:<autoconf>. It is constant
// across VMs (uniqueness is host-side); pkg/fc appends it to the VM's boot_args.
var BootIPArg = fmt.Sprintf("ip=%s::%s:%s::eth0:off", vmIP, vmGateway, vmNetmask)

// GuestMAC / TapDevice expose the constants pkg/fc needs for its network-interfaces config.
const (
	GuestMAC  = vmMAC
	TapDevice = tapName
)

// Slot is one sandbox's host-side network. The orchestrator dials RoutableIP:<port> and the
// netns DNATs it to the VM at the fixed vmIP:<port>; pkg/fc launches firecracker inside Netns
// with a virtio-net backed by that netns's TAP.
type Slot struct {
	Index      int    // the allocation index; also the per-slot third octet
	Netns      string // "msb-ns-<i>"
	RoutableIP string // "10.0.<i>.2" -- what the orchestrator dials (host-reachable, DNAT'd to the VM)

	hostVeth string // "msb-h<i>" (root ns)
	peerVeth string // "msb-c<i>" (inside the netns)
	hostIP   string // "10.0.<i>.1" (root-ns end of the veth /30)
}

// Addr is RoutableIP:port -- the address the orchestrator's data proxy / health probe dials.
func (s *Slot) Addr(port int) string {
	return net.JoinHostPort(s.RoutableIP, strconv.Itoa(port))
}

// newSlot derives all of a slot's names and addresses from its index. Slot i uses the i-th
// /30 carved by third octet (10.0.i.0/30): .1 the host veth end, .2 the routable/peer end.
// Readable over dense (slot 7 <-> 10.0.7.x); the third octet caps us at 256 concurrent
// sandboxes, ample for one machine. The interface names stay within IFNAMSIZ (15).
func newSlot(i int) *Slot {
	return &Slot{
		Index:      i,
		Netns:      fmt.Sprintf("msb-ns-%d", i),
		RoutableIP: fmt.Sprintf("10.0.%d.2", i),
		hostVeth:   fmt.Sprintf("msb-h%d", i),
		peerVeth:   fmt.Sprintf("msb-c%d", i),
		hostIP:     fmt.Sprintf("10.0.%d.1", i),
	}
}

// runner runs one command, returning its combined output (for diagnostics) and an error.
// Injected so tests assert the command plan without root, mirroring pkg/build's `run`.
type runner func(name string, args ...string) (string, error)

// Manager allocates and frees network slots, bounded to max concurrent. It owns only the
// index bookkeeping; the heavy lifting is the ip/iptables plan run through `run`.
type Manager struct {
	max int
	run runner

	mu   sync.Mutex
	used map[int]bool
}

// NewManager returns a Manager that shells out to ip/iptables for real, allowing up to max
// concurrent slots (clamped to 256, the third-octet cap).
func NewManager(max int) *Manager { return newManager(max, runCmd) }

func newManager(max int, run runner) *Manager {
	if max > 256 {
		max = 256
	}
	return &Manager{max: max, run: run, used: map[int]bool{}}
}

// Allocate reserves a free index and sets up its netns/veth/TAP/DNAT, returning the live
// Slot. On any setup-command failure it rolls the partial setup back (teardown) and frees
// the index, so a failure leaks neither a half-built slot nor the index.
func (m *Manager) Allocate() (*Slot, error) {
	idx, err := m.reserve()
	if err != nil {
		return nil, err
	}
	s := newSlot(idx)
	for _, cmd := range setupPlan(s) {
		if out, err := m.run(cmd[0], cmd[1:]...); err != nil {
			m.teardown(s) // roll back whatever did get created
			m.release(idx)
			return nil, fmt.Errorf("network slot %d: %q failed: %w\n%s", idx, strings.Join(cmd, " "), err, out)
		}
	}
	return s, nil
}

// Free tears a slot down (best-effort: teardown commands ignore errors, since a half-torn
// slot must still release its index) and returns the index to the pool.
func (m *Manager) Free(s *Slot) {
	m.teardown(s)
	m.release(s.Index)
}

func (m *Manager) reserve() (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := 0; i < m.max; i++ {
		if !m.used[i] {
			m.used[i] = true
			return i, nil
		}
	}
	return 0, fmt.Errorf("no free network slot (%d in use, max %d)", len(m.used), m.max)
}

func (m *Manager) release(idx int) {
	m.mu.Lock()
	delete(m.used, idx)
	m.mu.Unlock()
}

func (m *Manager) teardown(s *Slot) {
	for _, cmd := range teardownPlan(s) {
		_, _ = m.run(cmd[0], cmd[1:]...)
	}
}

// setupPlan is the exact ip/iptables sequence that builds a slot -- a pure function of the
// Slot, so tests assert it without running anything (the sequence was validated live in the
// Stage 12a privilege spike). `ip netns exec <ns>` runs a command inside the slot's netns.
func setupPlan(s *Slot) [][]string {
	in := func(args ...string) []string { // prefix: run inside the netns
		return append([]string{"ip", "netns", "exec", s.Netns}, args...)
	}
	return [][]string{
		{"ip", "netns", "add", s.Netns},
		// veth pair: the host end stays in the root ns, the peer end moves into the netns.
		{"ip", "link", "add", s.hostVeth, "type", "veth", "peer", "name", s.peerVeth},
		{"ip", "link", "set", s.peerVeth, "netns", s.Netns},
		{"ip", "addr", "add", s.hostIP + "/" + vmPrefix, "dev", s.hostVeth},
		{"ip", "link", "set", s.hostVeth, "up"},
		in("ip", "link", "set", "lo", "up"),
		in("ip", "addr", "add", s.RoutableIP+"/"+vmPrefix, "dev", s.peerVeth),
		in("ip", "link", "set", s.peerVeth, "up"),
		// TAP for the VM's virtio-net, addressed as the VM's gateway.
		in("ip", "tuntap", "add", tapName, "mode", "tap"),
		in("ip", "addr", "add", vmGateway+"/"+vmPrefix, "dev", tapName),
		in("ip", "link", "set", tapName, "up"),
		// Forward between the veth and the TAP, and DNAT the routable IP to the VM. Any port
		// dialed on RoutableIP reaches the same port on the VM; conntrack reverses the replies.
		// No MASQUERADE: the VM is reachable inbound but has no route out (Decision 6).
		in("sysctl", "-q", "-w", "net.ipv4.ip_forward=1"),
		in("iptables", "-t", "nat", "-A", "PREROUTING", "-d", s.RoutableIP, "-j", "DNAT", "--to-destination", vmIP),
	}
}

// teardownPlan removes a slot. Deleting the netns takes the peer veth, the TAP, and the
// netns's iptables rules with it; the root-ns veth end is then deleted explicitly (it usually
// vanishes with its peer, so this command is best-effort).
func teardownPlan(s *Slot) [][]string {
	return [][]string{
		{"ip", "netns", "del", s.Netns},
		{"ip", "link", "del", s.hostVeth},
	}
}

// runCmd is the production runner: combined output (for diagnostics) + an error on failure.
func runCmd(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s: %w", name, err)
	}
	return string(out), nil
}
