package placement

import "context"

// Node lifecycle statuses a discovered orchestrator can self-report (Stage 25). We model only the
// two that matter for placement; E2B's fuller set (Ready/Draining/Unhealthy/Standby, plus a
// Connecting derived from the live gRPC state) reduces to these here because reachability is
// already tracked separately (Node.ready, from the List poll) -- see docs/STAGE25_DESIGN.md D2/D4.
const (
	// StatusActive is a node accepting new placements. It is the zero value of NodeInfo.Status, so
	// an old registry key (Stage 24, pre-status) or the static --nodes flag deserializes to active.
	StatusActive = ""
	// StatusDraining is a node that is alive and still serving its existing sandboxes (Delete/List
	// still route to it) but is excluded from NEW placements -- E2B's NodeStatusDraining.
	StatusDraining = "draining"
)

// NodeInfo is one orchestrator as reported by a Discovery backend: its unique id, the gRPC
// address the api calls Create/Delete on, the data-proxy address written to the catalog as
// Route.Node (where client-proxy routes that sandbox's data path), and its self-reported lifecycle
// Status (Stage 25). It mirrors E2B's discovery.Node{ShortID, OrchestratorAddress} -- we carry
// Proxy too, because the api routes the data path by it (E2B derives the proxy address elsewhere),
// and Status because, unlike E2B (which polls it over a separate ServiceInfo RPC), our orchestrator
// self-reports it through the same registrar heartbeat that advertises membership. Status is empty
// (StatusActive) unless the orchestrator has entered drain. See docs/STAGE24_DESIGN.md §3 +
// docs/STAGE25_DESIGN.md D1.
type NodeInfo struct {
	ID     string
	GRPC   string
	Proxy  string
	Status string
}

// Discovery enumerates the orchestrators the api currently knows about. It is E2B's
// discovery.Discovery (packages/api/internal/orchestrator/discovery/discovery.go): the pluggable
// source of truth for the fleet, behind which the static --nodes flag and the Redis service
// registry are just two implementations. The registry's reconcile loop calls ListNodes each poll
// and diffs the result against the live node set -- adding orchestrators that appeared, dropping
// ones that vanished (E2B's keepInSync). This is what makes the fleet dynamic (Stage 24) rather
// than a fixed startup slice (Stage 23).
type Discovery interface {
	ListNodes(ctx context.Context) ([]NodeInfo, error)
}

// NodeFactory builds a live *Node from a discovered NodeInfo -- dialing the gRPC client and
// attaching a closer that releases the conn when the node is later evicted. It is injected by the
// api (which owns the grpc dialing) so this package stays dial-free and unit-testable with a fake
// factory. A factory error (e.g. a bad address) is logged by the registry and the node skipped;
// the next reconcile retries it.
type NodeFactory func(NodeInfo) (*Node, error)

// StaticDiscovery is a Discovery that always returns the same fixed set -- E2B's
// clusters/discovery/static.go ("always responds with the given items"), used here to wrap the
// Stage-23 --nodes flag so the static path is just one Discovery implementation. The fleet never
// changes, so the registry's reconcile is a no-op after the first poll and behavior is identical
// to Stage 23.
type StaticDiscovery struct{ nodes []NodeInfo }

// NewStaticDiscovery returns a StaticDiscovery over a copy of nodes (so a later mutation of the
// caller's slice can't change the advertised fleet).
func NewStaticDiscovery(nodes []NodeInfo) *StaticDiscovery {
	cp := make([]NodeInfo, len(nodes))
	copy(cp, nodes)
	return &StaticDiscovery{nodes: cp}
}

// ListNodes returns a fresh copy of the fixed node set (never errors).
func (s *StaticDiscovery) ListNodes(context.Context) ([]NodeInfo, error) {
	out := make([]NodeInfo, len(s.nodes))
	copy(out, s.nodes)
	return out, nil
}
