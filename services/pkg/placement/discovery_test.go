package placement

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// fakeDiscovery is a mutable Discovery: tests set its node set (or make it error) to simulate an
// orchestrator joining, leaving, or the registry backend blipping, then drive reconcile.
type fakeDiscovery struct {
	mu    sync.Mutex
	infos []NodeInfo
	err   error
}

func (d *fakeDiscovery) set(infos ...NodeInfo) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.infos, d.err = infos, nil
}

func (d *fakeDiscovery) fail(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.err = err
}

func (d *fakeDiscovery) ListNodes(context.Context) ([]NodeInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil {
		return nil, d.err
	}
	out := make([]NodeInfo, len(d.infos))
	copy(out, d.infos)
	return out, nil
}

// countingFactory builds ready nodes and records how many times each id was built and closed, so
// tests can assert "built once, closed on eviction".
func countingFactory(mu *sync.Mutex, created, closed map[string]int) NodeFactory {
	return func(in NodeInfo) (*Node, error) {
		mu.Lock()
		created[in.ID]++
		mu.Unlock()
		n := NewNode(in.ID, in.Proxy, &fakeRPC{}, DefaultCapacity)
		n.SetCloser(func() {
			mu.Lock()
			closed[in.ID]++
			mu.Unlock()
		})
		return n, nil
	}
}

func TestReconcileAddsAndRemoves(t *testing.T) {
	disco := &fakeDiscovery{}
	disco.set(NodeInfo{ID: "a", Proxy: "pa"}, NodeInfo{ID: "b", Proxy: "pb"})
	var mu sync.Mutex
	created, closed := map[string]int{}, map[string]int{}

	// NewRegistry reconciles once at construction, so a and b are present before any Start.
	reg := NewRegistry(disco, countingFactory(&mu, created, closed), 5)
	if got := len(reg.Nodes()); got != 2 {
		t.Fatalf("want 2 nodes after construction, got %d", got)
	}

	// A third orchestrator registers -> reconcile picks it up.
	disco.set(NodeInfo{ID: "a", Proxy: "pa"}, NodeInfo{ID: "b", Proxy: "pb"}, NodeInfo{ID: "c", Proxy: "pc"})
	reg.reconcile(context.Background())
	if got := len(reg.Nodes()); got != 3 {
		t.Fatalf("want 3 nodes after c joins, got %d", got)
	}
	if _, ok := reg.NodeByProxy("pc"); !ok {
		t.Fatal("c should be reachable by proxy after joining")
	}

	// b deregisters / TTL-expires -> reconcile evicts it and closes its conn.
	disco.set(NodeInfo{ID: "a", Proxy: "pa"}, NodeInfo{ID: "c", Proxy: "pc"})
	reg.reconcile(context.Background())
	if got := len(reg.Nodes()); got != 2 {
		t.Fatalf("want 2 nodes after b leaves, got %d", got)
	}
	if _, ok := reg.NodeByProxy("pb"); ok {
		t.Fatal("b should be gone from byProxy after leaving")
	}

	mu.Lock()
	defer mu.Unlock()
	if closed["b"] != 1 {
		t.Fatalf("b's conn should be closed exactly once on eviction, got %d", closed["b"])
	}
	// Idempotent: nodes that stayed were built once each, never re-created across reconciles.
	if created["a"] != 1 || created["c"] != 1 {
		t.Fatalf("surviving nodes should be built once: created=%v", created)
	}
}

func TestReconcileIdempotent(t *testing.T) {
	disco := &fakeDiscovery{}
	disco.set(NodeInfo{ID: "a", Proxy: "pa"})
	var mu sync.Mutex
	created, closed := map[string]int{}, map[string]int{}

	reg := NewRegistry(disco, countingFactory(&mu, created, closed), 5) // reconcile #1
	reg.reconcile(context.Background())                                 // #2
	reg.reconcile(context.Background())                                 // #3

	mu.Lock()
	defer mu.Unlock()
	if created["a"] != 1 {
		t.Fatalf("a should be built exactly once across repeated reconciles, got %d", created["a"])
	}
	if closed["a"] != 0 {
		t.Fatalf("a never left, so it must not be closed, got %d", closed["a"])
	}
}

func TestReconcileDiscoveryErrorKeepsFleet(t *testing.T) {
	disco := &fakeDiscovery{}
	disco.set(NodeInfo{ID: "a", Proxy: "pa"}, NodeInfo{ID: "b", Proxy: "pb"})
	factory := func(in NodeInfo) (*Node, error) {
		return NewNode(in.ID, in.Proxy, &fakeRPC{}, DefaultCapacity), nil
	}
	reg := NewRegistry(disco, factory, 5)
	if got := len(reg.Nodes()); got != 2 {
		t.Fatalf("want 2 nodes, got %d", got)
	}
	// A transient discovery failure (e.g. Redis down) must not wipe the fleet -- else every
	// create would fail until the next good poll.
	disco.fail(errors.New("redis unreachable"))
	reg.reconcile(context.Background())
	if got := len(reg.Nodes()); got != 2 {
		t.Fatalf("a discovery error must keep the current fleet, got %d nodes", got)
	}
}

func TestReconcileSkipsFactoryErrorThenRetries(t *testing.T) {
	disco := &fakeDiscovery{}
	disco.set(NodeInfo{ID: "good", Proxy: "pg"}, NodeInfo{ID: "bad", Proxy: "pbad"})
	var dialBad bool
	factory := func(in NodeInfo) (*Node, error) {
		if in.ID == "bad" && !dialBad {
			return nil, errors.New("dial refused")
		}
		return NewNode(in.ID, in.Proxy, &fakeRPC{}, DefaultCapacity), nil
	}
	reg := NewRegistry(disco, factory, 5) // bad's factory errors -> only good is added
	if _, ok := reg.NodeByProxy("pg"); !ok {
		t.Fatal("the good node should be added despite the bad node's factory error")
	}
	if _, ok := reg.NodeByProxy("pbad"); ok {
		t.Fatal("a node whose factory errored must not be added")
	}
	// Still discovered, not in the fleet -> the next reconcile retries it; now let it succeed.
	dialBad = true
	reg.reconcile(context.Background())
	if _, ok := reg.NodeByProxy("pbad"); !ok {
		t.Fatal("the bad node should be added on retry once its factory succeeds")
	}
}

func TestReconcileSyncsDrainStatus(t *testing.T) {
	disco := &fakeDiscovery{}
	disco.set(NodeInfo{ID: "a", Proxy: "pa"}, NodeInfo{ID: "b", Proxy: "pb"})
	factory := func(in NodeInfo) (*Node, error) {
		return NewNode(in.ID, in.Proxy, &fakeRPC{}, DefaultCapacity), nil
	}
	reg := NewRegistry(disco, factory, 5)
	for _, n := range reg.Nodes() {
		if n.Draining() {
			t.Fatalf("node %s should start active", n.ID)
		}
	}

	// b enters drain -- same membership, only its self-reported status changed. reconcile must
	// flip the LIVE node in place (not re-create it) so BestOfK stops picking it (Stage 25).
	disco.set(NodeInfo{ID: "a", Proxy: "pa"}, NodeInfo{ID: "b", Proxy: "pb", Status: StatusDraining})
	reg.reconcile(context.Background())
	if nb, _ := reg.NodeByProxy("pb"); nb == nil || !nb.Draining() {
		t.Fatal("b should be draining after reconcile picks up its status")
	}
	if na, _ := reg.NodeByProxy("pa"); na == nil || na.Draining() {
		t.Fatal("a's status was unchanged; it must stay active")
	}

	// b leaves drain -> reconcile clears it (drain is reversible).
	disco.set(NodeInfo{ID: "a", Proxy: "pa"}, NodeInfo{ID: "b", Proxy: "pb"})
	reg.reconcile(context.Background())
	if nb, _ := reg.NodeByProxy("pb"); nb == nil || nb.Draining() {
		t.Fatal("b left drain; reconcile should clear draining")
	}
}

func TestReconcileHonorsInitialDrainStatus(t *testing.T) {
	disco := &fakeDiscovery{}
	disco.set(NodeInfo{ID: "a", Proxy: "pa", Status: StatusDraining})
	factory := func(in NodeInfo) (*Node, error) {
		return NewNode(in.ID, in.Proxy, &fakeRPC{}, DefaultCapacity), nil
	}
	reg := NewRegistry(disco, factory, 5)
	// A node discovered already draining must be built draining, so a node that was draining before
	// this api started (or before it first saw the node) isn't picked until it re-reports active.
	if na, ok := reg.NodeByProxy("pa"); !ok || !na.Draining() {
		t.Fatal("a was discovered already draining; the freshly built node must start draining")
	}
}

func TestStaticDiscoveryReturnsIsolatedCopy(t *testing.T) {
	src := []NodeInfo{{ID: "a", GRPC: "a", Proxy: "pa"}}
	d := NewStaticDiscovery(src)
	src[0].ID = "mutated" // must not leak into the discovery's fixed set
	got, err := d.ListNodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("StaticDiscovery should return an isolated copy of its fixed list, got %v", got)
	}
}
