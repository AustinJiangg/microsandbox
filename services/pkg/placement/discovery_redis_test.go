package placement

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// discoveredByID lists the registry and indexes it by node id, for concise membership assertions.
func discoveredByID(t *testing.T, d *RedisDiscovery) map[string]NodeInfo {
	t.Helper()
	infos, err := d.ListNodes(context.Background())
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	m := make(map[string]NodeInfo, len(infos))
	for _, in := range infos {
		m[in.ID] = in
	}
	return m
}

// TestRedisDiscoveryRegistrarRoundTrip drives the register -> discover -> deregister contract
// against a live Redis (self-skips without REDIS_ADDR, the discipline pkg/catalog's Redis test
// uses, so a bare `go test` stays hermetic). Two orchestrators register; both are discovered with
// their fields carried; one deregisters and drops out immediately (DEL on Stop) while the other
// stays.
func TestRedisDiscoveryRegistrarRoundTrip(t *testing.T) {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Skip("REDIS_ADDR not set; skipping live Redis discovery test")
	}
	disco := NewRedisDiscovery(addr)
	defer disco.Close()

	a := NodeInfo{ID: "test-node-a", GRPC: "127.0.0.1:19090", Proxy: "127.0.0.1:15007"}
	b := NodeInfo{ID: "test-node-b", GRPC: "127.0.0.1:19091", Proxy: "127.0.0.1:15017"}
	regA := NewRegistrar(addr, a, 2*time.Second, 500*time.Millisecond)
	regB := NewRegistrar(addr, b, 2*time.Second, 500*time.Millisecond)
	regA.Start()
	regB.Start()

	stopped := map[*Registrar]bool{}
	stop := func(r *Registrar) {
		if !stopped[r] {
			r.Stop()
			stopped[r] = true
		}
	}
	defer stop(regA) // cleanup even if an assertion below fails
	defer stop(regB)

	// register() is synchronous in Start, so both are discoverable right away.
	got := discoveredByID(t, disco)
	if _, ok := got[a.ID]; !ok {
		t.Fatalf("node a should be discovered after Start, got %v", keysOf(got))
	}
	if _, ok := got[b.ID]; !ok {
		t.Fatalf("node b should be discovered after Start, got %v", keysOf(got))
	}
	if got[a.ID].Proxy != a.Proxy || got[a.ID].GRPC != a.GRPC {
		t.Fatalf("node a's fields should round-trip, got %+v", got[a.ID])
	}

	// Deregister a: DEL on Stop -> gone immediately; b stays.
	stop(regA)
	got = discoveredByID(t, disco)
	if _, ok := got[a.ID]; ok {
		t.Fatalf("a deregistered node should be gone, still saw %s", a.ID)
	}
	if _, ok := got[b.ID]; !ok {
		t.Fatalf("node b should still be registered after a leaves")
	}
}

// TestRedisDiscoveryTTLExpiry proves the crash path: a node key written once with a short TTL and
// never heartbeated (a crashed orchestrator) disappears from discovery once the TTL lapses -- no
// deregister needed. This is the discovery-layer version of Stage 24c's real-VM kill test.
func TestRedisDiscoveryTTLExpiry(t *testing.T) {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Skip("REDIS_ADDR not set; skipping live Redis discovery TTL test")
	}
	disco := NewRedisDiscovery(addr)
	defer disco.Close()

	self := NodeInfo{ID: "test-node-ttl", GRPC: "127.0.0.1:19099", Proxy: "127.0.0.1:15099"}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	defer rdb.Close()
	blob, _ := json.Marshal(self)
	// Write once with a 1s TTL and never refresh -- simulates a node that registered then crashed.
	if err := rdb.Set(context.Background(), nodeKey(self.ID), blob, time.Second).Err(); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	if _, ok := discoveredByID(t, disco)[self.ID]; !ok {
		t.Fatal("the node should be discovered right after registering")
	}
	time.Sleep(1500 * time.Millisecond) // let the TTL lapse
	if _, ok := discoveredByID(t, disco)[self.ID]; ok {
		t.Fatal("a node that stopped heartbeating should expire out of discovery")
	}
}

func keysOf(m map[string]NodeInfo) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
