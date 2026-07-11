package placement

import (
	"time"
)

// Registry is the api's view of the orchestrator fleet: the set of nodes plus the BestOfK
// algorithm that picks among them, and a background loop that keeps each node's cached load
// and readiness fresh by polling its List RPC (~1s). It stands in for E2B's node discovery
// (Nomad node list) + metrics poll -- here the node set is static (supplied at construction
// from the api's --nodes flag), since Nomad/Consul stay deferred (docs/STAGE23_DESIGN.md §4).
type Registry struct {
	algo    *BestOfK
	nodes   []*Node
	byProxy map[string]*Node // Proxy addr -> Node, so destroy can reach the node holding a sandbox
	stop    chan struct{}
}

// NewRegistry builds a registry over a fixed set of nodes, sampling k candidates per
// placement. The nodes' data-proxy addresses must be unique (they key the catalog routes and
// the byProxy index); NewRegistry does not dedupe -- the caller (the api's flag parser)
// guarantees it.
func NewRegistry(nodes []*Node, k int) *Registry {
	byProxy := make(map[string]*Node, len(nodes))
	for _, n := range nodes {
		byProxy[n.Proxy] = n
	}
	return &Registry{
		algo:    NewBestOfK(k),
		nodes:   nodes,
		byProxy: byProxy,
		stop:    make(chan struct{}),
	}
}

// Nodes returns the registry's node set (read-only; the slice is not copied).
func (r *Registry) Nodes() []*Node { return r.nodes }

// Pick chooses a node to place a new sandbox on, skipping any IDs in excluded (the create
// handler's per-request failover set). Returns ErrNoNode if none is eligible.
func (r *Registry) Pick(excluded map[string]struct{}) (*Node, error) {
	return r.algo.Choose(r.nodes, excluded)
}

// NodeByProxy returns the node whose data-proxy address is proxy (the catalog Route.Node),
// so destroy can route Delete to the node actually holding the sandbox.
func (r *Registry) NodeByProxy(proxy string) (*Node, bool) {
	n, ok := r.byProxy[proxy]
	return n, ok
}

// Start primes each node's load/readiness once (so the first create sees fresh data) and then
// refreshes on pollInterval until Stop. Safe to call once.
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

// Stop halts the poll loop. Idempotent is not guaranteed -- call once, at shutdown.
func (r *Registry) Stop() { close(r.stop) }

// pollOnce refreshes every node's cached load + readiness. Nodes are refreshed sequentially;
// on one box the fleet is tiny and List is a cheap in-memory count, so a fan-out isn't worth
// the complexity (E2B polls per-node too, just across many machines).
func (r *Registry) pollOnce() {
	for _, n := range r.nodes {
		n.refresh()
	}
}
