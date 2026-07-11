// Package placement is the api's multi-host scheduler: it holds the set of orchestrator
// nodes the api knows about and picks one to place each new sandbox on, mirroring E2B's
// `packages/api/internal/orchestrator/placement` (the BestOfK "power of K choices"
// algorithm). Before Stage 23 the api was hard-wired to a single orchestrator; this package
// is what lets it hold a fleet and spread load across it. The data path was already
// multi-host since Stage 14a (the catalog stores a per-sandbox Route{Node}), so this is a
// purely api-side change. See docs/STAGE23_DESIGN.md.
//
// Single-machine specialization (faithful reduction of E2B's CPU-weighted score): every
// sandbox here is a fixed 1 vCPU / 512 MiB, so "CPU allocated on a node" is just "number of
// sandboxes on that node" -- which the orchestrator's existing SandboxService.List RPC
// already reports. So E2B's score collapses to (inProgress + cachedCount) / capacity, and we
// need no new metrics RPC (nor a proto change). See docs/STAGE23_DESIGN.md §3.
package placement

import (
	"context"
	"math"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	pb "microsandbox/services/pkg/grpc/orchestrator"
)

// Defaults, mirroring E2B's DefaultBestOfKConfig where an analogue exists.
const (
	// DefaultK is how many candidate nodes BestOfK samples per placement ("power of K
	// choices"). E2B's default is 3: enough to avoid herding onto one node, cheap to score.
	DefaultK = 3
	// DefaultCapacity stands in for E2B's R*CpuCount (over-commit ratio x cores). We use the
	// per-node concurrent-sandbox bound, which is pkg/network's 256-slot cap (server.go).
	DefaultCapacity = 256
	// pollInterval is how often the registry refreshes each node's cached load + readiness by
	// calling List, standing in for E2B's background metrics poll.
	pollInterval = time.Second
	// pollTimeout bounds a single List refresh so one unreachable node can't stall the loop.
	pollTimeout = 2 * time.Second
)

// SandboxRPC is the subset of the orchestrator's gRPC SandboxServiceClient this package needs:
// Create/Delete route a sandbox's lifecycle to the node holding it, and List is both the
// api's routing target and -- crucially here -- the node's load signal (its length is the
// sandbox count). pb.SandboxServiceClient satisfies this exactly; unit tests supply a fake so
// the whole package is testable without a real gRPC server or a VM.
type SandboxRPC interface {
	Create(ctx context.Context, in *pb.SandboxCreateRequest, opts ...grpc.CallOption) (*pb.SandboxCreateResponse, error)
	Delete(ctx context.Context, in *pb.SandboxDeleteRequest, opts ...grpc.CallOption) (*emptypb.Empty, error)
	List(ctx context.Context, in *emptypb.Empty, opts ...grpc.CallOption) (*pb.SandboxListResponse, error)
}

// Node is one orchestrator the api can place sandboxes on. It couples the gRPC client (RPC)
// the api calls Create/Delete on with the data-proxy address (Proxy) written to the catalog
// as Route.Node -- so a placement decision yields both "which node runs the VM" and "where
// client-proxy routes its data". The load fields are E2B's two Score inputs:
//   - count      = cached len(List()), refreshed by the registry's poll  (E2B: Metrics())
//   - inProgress = placements chosen but not yet settled                 (E2B: InProgress())
//
// ready is set by the poll: a node whose List errors is dropped from sampling until it
// answers again. All three are atomics because the poll loop writes them concurrently with
// placement reads.
type Node struct {
	ID       string     // human label + uniqueness key; the node's gRPC address
	Proxy    string     // data-proxy address -> catalog Route.Node (where client-proxy routes)
	RPC      SandboxRPC // gRPC client to this node's SandboxService
	Capacity int        // load denominator (E2B's R*CpuCount analogue); <=0 makes the node un-pickable

	count      atomic.Int64 // cached sandbox count from the last successful List
	inProgress atomic.Int64 // chosen-but-not-yet-settled placements on this node
	ready      atomic.Bool  // last List succeeded (reachable); false drops it from sampling
	draining   atomic.Bool  // self-reported drain (Stage 25); true drops it from NEW placements
	closeFn    func()       // cleanup run on eviction (closes the gRPC conn); nil for test nodes
}

// SetCloser attaches a cleanup invoked when the node is evicted from the registry -- reconcile
// drops it because discovery no longer reports it (Stage 24). The api's node factory sets it to
// close the node's gRPC conn; prebuilt test nodes leave it nil, so Close is then a no-op.
func (n *Node) SetCloser(fn func()) { n.closeFn = fn }

// Close runs the node's cleanup, if any. The registry calls it when the node leaves the fleet
// (dynamic discovery) so a departed node's gRPC conn is released rather than leaked.
func (n *Node) Close() {
	if n.closeFn != nil {
		n.closeFn()
	}
}

// NewNode builds a node. It starts ready=true (optimistic): a freshly configured node is
// assumed reachable so the very first create works before the first poll runs; the poll
// flips it to false on the first List error and self-corrects. capacity <=0 falls back to
// DefaultCapacity.
func NewNode(id, proxy string, rpc SandboxRPC, capacity int) *Node {
	if capacity <= 0 {
		capacity = DefaultCapacity
	}
	n := &Node{ID: id, Proxy: proxy, RPC: rpc, Capacity: capacity}
	n.ready.Store(true)
	return n
}

// Reserve marks that a placement onto this node has been chosen but not yet settled
// (inProgress++). The create handler calls it right after Pick and Release once the create
// finishes (catalog written, or rolled back), so a burst of concurrent creates spreads
// instead of all landing on whichever node looked emptiest before the next poll.
func (n *Node) Reserve() { n.inProgress.Add(1) }

// Release undoes a Reserve (inProgress--). Always paired with a Reserve.
func (n *Node) Release() { n.inProgress.Add(-1) }

// Ready reports whether the node's last List succeeded (so it is eligible for sampling).
func (n *Node) Ready() bool { return n.ready.Load() }

// Draining reports whether the node has self-reported drain (Stage 25). A draining node keeps
// serving its existing sandboxes -- Delete/List still route to it -- but BestOfK never picks it for
// a new placement. It is set by the registry's reconcile from the discovered NodeInfo.Status.
func (n *Node) Draining() bool { return n.draining.Load() }

// setDraining records the node's self-reported drain state. reconcile calls it each poll with the
// discovered NodeInfo.Status, so an orchestrator entering (or leaving) drain flips the live node on
// the next reconcile tick.
func (n *Node) setDraining(v bool) { n.draining.Store(v) }

// Load is the node's placement score: lower means emptier means preferred. It is E2B's
// Score specialized to homogeneous 1-vCPU sandboxes -- (reserved) / capacity, where reserved
// combines the cached settled count with the in-flight placements. A non-positive capacity
// yields +Inf so the node is never chosen (E2B returns MaxFloat64 on a zero CpuCount).
func (n *Node) Load() float64 {
	if n.Capacity <= 0 {
		return math.MaxFloat64
	}
	reserved := n.count.Load() + n.inProgress.Load()
	return float64(reserved) / float64(n.Capacity)
}

// refresh polls the node's List once and updates its cached count + readiness. A List error
// (node down/unreachable) marks it not-ready; a success updates the count and marks it ready.
// This is the per-node body of the registry's poll loop.
func (n *Node) refresh() {
	ctx, cancel := context.WithTimeout(context.Background(), pollTimeout)
	defer cancel()
	resp, err := n.RPC.List(ctx, &emptypb.Empty{})
	if err != nil {
		n.ready.Store(false)
		return
	}
	n.count.Store(int64(len(resp.GetSandboxIds())))
	n.ready.Store(true)
}
