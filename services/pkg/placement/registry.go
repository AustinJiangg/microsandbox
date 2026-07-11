package placement

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"
)

// Registry is the api's view of the orchestrator fleet: a mutable set of nodes plus the BestOfK
// algorithm that picks among them, kept in sync by a background loop that (a) reconciles the set
// against a Discovery source -- adding orchestrators that appeared, dropping ones that vanished --
// and (b) refreshes each node's cached load + readiness via its List RPC (~1s). It stands in for
// E2B's node discovery + metrics poll (packages/api/internal/orchestrator: discovery.Discovery +
// keepInSync). Before Stage 24 the node set was a fixed slice supplied once at startup; now it is
// a reconciled map, so the fleet changes at runtime with no api restart. See
// docs/STAGE24_DESIGN.md.
type Registry struct {
	algo      *BestOfK
	discovery Discovery   // the pluggable source of truth for the fleet (static flag | Redis)
	factory   NodeFactory // builds a live *Node from a discovered NodeInfo (dials gRPC)

	mu      sync.RWMutex     // guards nodes + byProxy against the poll loop / placement reads
	nodes   map[string]*Node // id -> live node
	byProxy map[string]*Node // Proxy addr -> node, so destroy reaches the node holding a sandbox
	stop    chan struct{}
}

// NewRegistry builds a registry over a Discovery source, sampling k candidates per placement and
// building each live node with factory. It reconciles once synchronously at construction so the
// fleet is populated before the first Pick -- even for callers that never Start the poll loop
// (e.g. the placement tests). Start then keeps it fresh.
func NewRegistry(discovery Discovery, factory NodeFactory, k int) *Registry {
	r := &Registry{
		algo:      NewBestOfK(k),
		discovery: discovery,
		factory:   factory,
		nodes:     map[string]*Node{},
		byProxy:   map[string]*Node{},
		stop:      make(chan struct{}),
	}
	r.reconcile(context.Background()) // prime membership so the first placement sees the fleet
	return r
}

// NewStaticRegistry builds a registry over a fixed set of pre-built nodes: a StaticDiscovery over
// their NodeInfo plus a factory that returns the matching pre-built node by id. It expresses the
// Stage-23 behavior (a fixed fleet) through the Stage-24 discovery seam, and is what the unit /
// integration tests use to inject fake- or real-conn nodes directly. The nodes' proxy addresses
// must be unique (they key the catalog routes and byProxy index).
func NewStaticRegistry(nodes []*Node, k int) *Registry {
	infos := make([]NodeInfo, len(nodes))
	byID := make(map[string]*Node, len(nodes))
	for i, n := range nodes {
		infos[i] = NodeInfo{ID: n.ID, GRPC: n.ID, Proxy: n.Proxy}
		byID[n.ID] = n
	}
	factory := func(in NodeInfo) (*Node, error) {
		if n, ok := byID[in.ID]; ok {
			return n, nil
		}
		return nil, fmt.Errorf("placement: no prebuilt node %q", in.ID)
	}
	return NewRegistry(NewStaticDiscovery(infos), factory, k)
}

// Nodes returns a snapshot of the registry's live node set (a fresh slice; safe to iterate while
// the poll loop mutates the map).
func (r *Registry) Nodes() []*Node { return r.snapshot() }

// snapshot copies the live nodes into a slice under the read lock, so BestOfK and callers iterate
// a stable set while reconcile adds/removes concurrently.
func (r *Registry) snapshot() []*Node {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Node, 0, len(r.nodes))
	for _, n := range r.nodes {
		out = append(out, n)
	}
	return out
}

// Pick chooses a node to place a new sandbox on, skipping any IDs in excluded (the create
// handler's per-request failover set). Returns ErrNoNode if none is eligible.
func (r *Registry) Pick(excluded map[string]struct{}) (*Node, error) {
	return r.algo.Choose(r.snapshot(), excluded)
}

// NodeByProxy returns the node whose data-proxy address is proxy (the catalog Route.Node), so
// destroy can route Delete to the node actually holding the sandbox.
func (r *Registry) NodeByProxy(proxy string) (*Node, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n, ok := r.byProxy[proxy]
	return n, ok
}

// Start primes the fleet's load/readiness once (so the first create sees fresh data) and then
// reconciles + refreshes on pollInterval until Stop. Safe to call once.
func (r *Registry) Start() {
	r.pollOnce() // synchronous prime before returning, so the first placement isn't blind
	go func() {
		t := time.NewTicker(pollInterval)
		defer t.Stop()
		for {
			select {
			case <-r.stop:
				return
			case <-t.C:
				r.pollOnce()
			}
		}
	}()
}

// Stop halts the poll loop. Call once, at shutdown. It does not close the live nodes' conns:
// eviction (reconcile) already closes a departed node's conn, and the process exits right after
// Stop, which drops the rest.
func (r *Registry) Stop() { close(r.stop) }

// pollOnce reconciles the fleet against discovery, then refreshes every live node's cached load +
// readiness. Reconcile first so a just-joined node is refreshed this same tick, and a just-left
// node isn't refreshed at all.
func (r *Registry) pollOnce() {
	r.reconcile(context.Background())
	for _, n := range r.snapshot() {
		n.refresh()
	}
}

// reconcile diffs the Discovery source against the live node set: it adds a discovered node that
// isn't in the fleet (building it via factory) and removes a live node that discovery no longer
// reports (closing its conn). This is E2B's keepInSync (orchestrator/cache.go). A ListNodes error
// is logged and the current fleet kept unchanged -- a transient discovery blip must not wipe every
// node (which would fail every create until the next successful poll).
func (r *Registry) reconcile(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()
	infos, err := r.discovery.ListNodes(cctx)
	if err != nil {
		log.Printf("placement: discovery ListNodes failed (%v); keeping current fleet", err)
		return
	}
	discovered := make(map[string]NodeInfo, len(infos))
	for _, in := range infos {
		discovered[in.ID] = in
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	// Add or update: a discovered node not yet in the fleet is built and joined; one already
	// present has its self-reported drain status synced (Stage 25) -- an orchestrator that entered
	// (or left) drain since the last poll flips its live node here, so BestOfK stops (or resumes)
	// picking it without any membership change.
	for id, in := range discovered {
		if existing, ok := r.nodes[id]; ok {
			existing.setDraining(in.Status == StatusDraining)
			continue
		}
		node, ferr := r.factory(in)
		if ferr != nil {
			log.Printf("placement: connect to discovered node %s (%s): %v", id, in.GRPC, ferr)
			continue
		}
		node.setDraining(in.Status == StatusDraining) // honor a node discovered already draining
		r.nodes[id] = node
		r.byProxy[node.Proxy] = node
		log.Printf("placement: node joined: %s (proxy %s)", id, node.Proxy)
	}
	// Remove: in the fleet but no longer discovered (deregistered, or its registry TTL expired).
	for id, node := range r.nodes {
		if _, ok := discovered[id]; ok {
			continue
		}
		delete(r.nodes, id)
		delete(r.byProxy, node.Proxy)
		node.Close()
		log.Printf("placement: node left: %s (proxy %s)", id, node.Proxy)
	}
}
