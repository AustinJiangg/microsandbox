package network

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

// recorder is a fake runner: it records every command and optionally fails the first command
// whose joined form contains failOn -- enough to drive Allocate's rollback path without root.
type recorder struct {
	mu     sync.Mutex
	cmds   [][]string
	failOn string
}

func (r *recorder) run(name string, args ...string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cmd := append([]string{name}, args...)
	r.cmds = append(r.cmds, cmd)
	if r.failOn != "" && strings.Contains(strings.Join(cmd, " "), r.failOn) {
		return "boom", fmt.Errorf("forced failure")
	}
	return "", nil
}

func (r *recorder) joined() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.cmds))
	for i, c := range r.cmds {
		out[i] = strings.Join(c, " ")
	}
	return out
}

func contains(cmds []string, want string) bool {
	for _, c := range cmds {
		if c == want {
			return true
		}
	}
	return false
}

func TestNewSlotDerivation(t *testing.T) {
	s := newSlot(7)
	if s.Netns != "msb-ns-7" || s.RoutableIP != "10.0.7.2" || s.hostIP != "10.0.7.1" {
		t.Fatalf("bad derivation: %+v", s)
	}
	if s.hostVeth != "msb-h7" || s.peerVeth != "msb-c7" {
		t.Fatalf("bad veth names: %+v", s)
	}
	if got := s.Addr(49983); got != "10.0.7.2:49983" {
		t.Fatalf("Addr = %q, want 10.0.7.2:49983", got)
	}
}

func TestBootIPArg(t *testing.T) {
	// The guest kernel parses this verbatim; pin the exact string so a typo can't slip the VM
	// onto a wrong gateway/netmask silently.
	want := "ip=169.254.0.21::169.254.0.22:255.255.255.252::eth0:off"
	if BootIPArg != want {
		t.Fatalf("BootIPArg = %q, want %q", BootIPArg, want)
	}
}

func TestSetupPlanKeyCommands(t *testing.T) {
	plan := setupPlan(newSlot(3))
	joined := make([]string, len(plan))
	for i, c := range plan {
		joined[i] = strings.Join(c, " ")
	}
	for _, want := range []string{
		"ip netns add msb-ns-3",
		"ip link add msb-h3 type veth peer name msb-c3",
		"ip link set msb-c3 netns msb-ns-3",
		"ip addr add 10.0.3.1/30 dev msb-h3",
		"ip netns exec msb-ns-3 ip tuntap add tap0 mode tap",
		"ip netns exec msb-ns-3 ip addr add 169.254.0.22/30 dev tap0",
		"ip netns exec msb-ns-3 sysctl -q -w net.ipv4.ip_forward=1",
		"ip netns exec msb-ns-3 iptables -t nat -A PREROUTING -d 10.0.3.2 -j DNAT --to-destination 169.254.0.21",
	} {
		if !contains(joined, want) {
			t.Errorf("setup plan missing command:\n  want: %s\n  got:  %v", want, joined)
		}
	}
	// The netns must exist before anything runs inside it.
	if joined[0] != "ip netns add msb-ns-3" {
		t.Errorf("netns add must be first, got %q", joined[0])
	}
}

func TestAllocateRunsPlanAndAssignsIndex(t *testing.T) {
	rec := &recorder{}
	m := newManager(4, rec.run)
	s, err := m.Allocate()
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if s.Index != 0 {
		t.Fatalf("first slot index = %d, want 0", s.Index)
	}
	if len(rec.cmds) != len(setupPlan(s)) {
		t.Fatalf("ran %d commands, want the full plan of %d", len(rec.cmds), len(setupPlan(s)))
	}
	if rec.joined()[0] != "ip netns add msb-ns-0" {
		t.Errorf("first command = %q, want netns add", rec.joined()[0])
	}
}

func TestAllocateRollsBackAndFreesIndexOnFailure(t *testing.T) {
	rec := &recorder{failOn: "DNAT"} // fail on the last setup command
	m := newManager(4, rec.run)
	if _, err := m.Allocate(); err == nil {
		t.Fatal("expected Allocate to fail when a setup command fails")
	}
	// Rollback must have run teardown (so no half-built slot leaks)...
	if !contains(rec.joined(), "ip netns del msb-ns-0") {
		t.Errorf("rollback did not tear the slot down: %v", rec.joined())
	}
	// ...and released the index (so it is reusable).
	m.mu.Lock()
	inUse := len(m.used)
	m.mu.Unlock()
	if inUse != 0 {
		t.Errorf("index not released after failed Allocate: %d still in use", inUse)
	}
}

func TestAllocateExhaustion(t *testing.T) {
	rec := &recorder{}
	m := newManager(2, rec.run)
	if _, err := m.Allocate(); err != nil {
		t.Fatalf("slot 0: %v", err)
	}
	if _, err := m.Allocate(); err != nil {
		t.Fatalf("slot 1: %v", err)
	}
	_, err := m.Allocate()
	if err == nil || !strings.Contains(err.Error(), "no free network slot") {
		t.Fatalf("third Allocate err = %v, want exhaustion", err)
	}
}

func TestFreeReleasesIndexForReuse(t *testing.T) {
	rec := &recorder{}
	m := newManager(2, rec.run)
	s0, _ := m.Allocate() // index 0
	s1, _ := m.Allocate() // index 1
	if s0.Index != 0 || s1.Index != 1 {
		t.Fatalf("indices = %d,%d want 0,1", s0.Index, s1.Index)
	}
	m.Free(s0)
	if !contains(rec.joined(), "ip netns del msb-ns-0") {
		t.Errorf("Free did not tear msb-ns-0 down: %v", rec.joined())
	}
	s2, err := m.Allocate() // should reuse index 0
	if err != nil || s2.Index != 0 {
		t.Fatalf("reuse: index=%d err=%v, want index 0", s2.Index, err)
	}
}

func TestMaxClampedTo256(t *testing.T) {
	if m := newManager(1000, (&recorder{}).run); m.max != 256 {
		t.Fatalf("max = %d, want clamp to 256", m.max)
	}
}
