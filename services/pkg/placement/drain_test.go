package placement

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestDrainCommandReflectedInHeartbeat drives the full Stage-25 drain channel against a live Redis
// (self-skips without REDIS_ADDR, like the discovery round-trip test): the api issues a Drain
// command, the orchestrator's Registrar reads it and reflects StatusDraining in the NodeInfo it
// heartbeats, and RedisDiscovery surfaces it; Resume flips it back to active. This is the
// orchestrator-authoritative, api-initiated path Decision D3 describes.
func TestDrainCommandReflectedInHeartbeat(t *testing.T) {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Skip("REDIS_ADDR not set; skipping live Redis drain test")
	}
	disco := NewRedisDiscovery(addr)
	defer disco.Close()
	cmds := NewDrainCommands(addr)
	defer cmds.Close()

	self := NodeInfo{ID: "test-node-drain", GRPC: "127.0.0.1:19190", Proxy: "127.0.0.1:15190"}
	// Start clean: a prior failed run may have left a drain command for this id.
	_ = cmds.Resume(context.Background(), self.ID)
	defer cmds.Resume(context.Background(), self.ID) // and don't leak the command out of this test

	reg := NewRegistrar(addr, self, 2*time.Second, 300*time.Millisecond)
	reg.Start()
	defer reg.Stop()

	statusOf := func() string {
		infos, err := disco.ListNodes(context.Background())
		if err != nil {
			t.Fatalf("ListNodes: %v", err)
		}
		for _, in := range infos {
			if in.ID == self.ID {
				return in.Status
			}
		}
		t.Fatalf("node %s not discovered", self.ID)
		return ""
	}

	// register() is synchronous in Start with no drain command set -> the node self-reports active.
	if s := statusOf(); s != StatusActive {
		t.Fatalf("a fresh node should self-report active, got %q", s)
	}

	// api drains it -> the next heartbeat (~300ms) reflects StatusDraining.
	if err := cmds.Drain(context.Background(), self.ID); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	if !waitFor(func() bool { return statusOf() == StatusDraining }, 3*time.Second) {
		t.Fatal("node should self-report draining within a heartbeat of the Drain command")
	}

	// api resumes it -> back to active on the next heartbeat.
	if err := cmds.Resume(context.Background(), self.ID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !waitFor(func() bool { return statusOf() == StatusActive }, 3*time.Second) {
		t.Fatal("node should self-report active again within a heartbeat of Resume")
	}
}

// waitFor polls cond every 50ms until it holds or the timeout elapses.
func waitFor(cond func() bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return cond()
}
