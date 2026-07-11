package placement

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	pb "microsandbox/services/pkg/grpc/orchestrator"
)

// fakeRPC is a SandboxRPC that returns a fixed sandbox list (its length is the node's load
// signal) or a fixed error, so the registry/refresh path is testable without a gRPC server.
type fakeRPC struct {
	ids     []string
	listErr error
}

func (f *fakeRPC) Create(context.Context, *pb.SandboxCreateRequest, ...grpc.CallOption) (*pb.SandboxCreateResponse, error) {
	return &pb.SandboxCreateResponse{SandboxId: "sb_fake"}, nil
}
func (f *fakeRPC) Delete(context.Context, *pb.SandboxDeleteRequest, ...grpc.CallOption) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, nil
}
func (f *fakeRPC) List(context.Context, *emptypb.Empty, ...grpc.CallOption) (*pb.SandboxListResponse, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return &pb.SandboxListResponse{SandboxIds: f.ids}, nil
}

// nodeWith builds a ready node preloaded with a settled sandbox count (white-box: the load
// fields are unexported, so only this package's tests can seed them directly).
func nodeWith(id string, count int64) *Node {
	n := NewNode(id, "proxy-"+id, nil, DefaultCapacity)
	n.count.Store(count)
	return n
}

func TestLoadLowerIsEmptier(t *testing.T) {
	empty := nodeWith("a", 0)
	busy := nodeWith("b", 10)
	if !(empty.Load() < busy.Load()) {
		t.Fatalf("emptier node should score lower: empty=%v busy=%v", empty.Load(), busy.Load())
	}
	// Reserving (in-flight placements) counts toward load just like settled sandboxes.
	empty.Reserve()
	empty.Reserve()
	if empty.Load() != float64(2)/float64(DefaultCapacity) {
		t.Fatalf("in-progress should count toward load: got %v", empty.Load())
	}
	empty.Release()
	if empty.Load() != float64(1)/float64(DefaultCapacity) {
		t.Fatalf("Release should decrement in-progress: got %v", empty.Load())
	}
}

func TestLoadZeroCapacityNeverChosen(t *testing.T) {
	n := NewNode("z", "proxy-z", nil, DefaultCapacity)
	n.Capacity = 0 // an un-provisioned node
	if !(n.Load() > 1e300) {
		t.Fatalf("zero-capacity node should score +Inf-ish, got %v", n.Load())
	}
}

// chooseAll uses K >= len(nodes) so sample() returns every eligible node, making Choose a
// deterministic global argmin -- the right tool for asserting scoring, not sampling.
func chooseAll(t *testing.T, nodes []*Node, excluded map[string]struct{}) (*Node, error) {
	t.Helper()
	return NewBestOfK(len(nodes) + 1).Choose(nodes, excluded)
}

func TestChoosePicksMinLoad(t *testing.T) {
	nodes := []*Node{nodeWith("a", 5), nodeWith("b", 1), nodeWith("c", 9)}
	best, err := chooseAll(t, nodes, nil)
	if err != nil {
		t.Fatal(err)
	}
	if best.ID != "b" {
		t.Fatalf("expected the least-loaded node b, got %s", best.ID)
	}
}

func TestChooseSkipsNotReady(t *testing.T) {
	a := nodeWith("a", 0) // emptiest, but...
	a.ready.Store(false)  // ...not ready -> must be skipped
	b := nodeWith("b", 5)
	best, err := chooseAll(t, []*Node{a, b}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if best.ID != "b" {
		t.Fatalf("a is not ready; expected b, got %s", best.ID)
	}
}

func TestChooseSkipsExcluded(t *testing.T) {
	a := nodeWith("a", 0) // emptiest, but excluded (its Create just failed)
	b := nodeWith("b", 5)
	best, err := chooseAll(t, []*Node{a, b}, map[string]struct{}{"a": {}})
	if err != nil {
		t.Fatal(err)
	}
	if best.ID != "b" {
		t.Fatalf("a is excluded; expected b, got %s", best.ID)
	}
}

func TestChooseInProgressShiftsChoice(t *testing.T) {
	a := nodeWith("a", 0)
	b := nodeWith("b", 0) // tie on settled count
	a.Reserve()           // but a has an in-flight placement -> b is now strictly emptier
	a.Reserve()
	best, err := chooseAll(t, []*Node{a, b}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if best.ID != "b" {
		t.Fatalf("a has 2 in-flight placements; expected b, got %s", best.ID)
	}
}

func TestChooseNoEligibleNode(t *testing.T) {
	a := nodeWith("a", 0)
	a.ready.Store(false)
	b := nodeWith("b", 0)
	_, err := chooseAll(t, []*Node{a, b}, map[string]struct{}{"b": {}})
	if !errors.Is(err, ErrNoNode) {
		t.Fatalf("all nodes excluded/not-ready should yield ErrNoNode, got %v", err)
	}
	// An empty fleet also yields ErrNoNode, not a panic.
	if _, err := chooseAll(t, nil, nil); !errors.Is(err, ErrNoNode) {
		t.Fatalf("empty fleet should yield ErrNoNode, got %v", err)
	}
}

func TestSampleInvariants(t *testing.T) {
	// 6 nodes, 2 of them not-ready, one excluded -> 3 eligible. Sampling K=2 must return 2
	// distinct, ready, non-excluded nodes; K=5 (> eligible) must return exactly the 3 eligible.
	nodes := make([]*Node, 6)
	for i := range nodes {
		nodes[i] = nodeWith(string(rune('a'+i)), int64(i))
	}
	nodes[1].ready.Store(false)
	nodes[4].ready.Store(false)
	excluded := map[string]struct{}{"c": {}} // nodes[2]
	eligible := map[string]bool{"a": true, "d": true, "f": true}

	check := func(out []*Node, wantLen int) {
		if len(out) != wantLen {
			t.Fatalf("sample returned %d nodes, want %d", len(out), wantLen)
		}
		seen := map[string]bool{}
		for _, n := range out {
			if !eligible[n.ID] {
				t.Fatalf("sample returned ineligible node %s", n.ID)
			}
			if seen[n.ID] {
				t.Fatalf("sample returned duplicate node %s", n.ID)
			}
			seen[n.ID] = true
		}
	}
	// Run many times: sampling is randomized, so assert the invariants hold every draw.
	for i := 0; i < 200; i++ {
		check(NewBestOfK(2).sample(nodes, excluded), 2)
		check(NewBestOfK(5).sample(nodes, excluded), 3) // only 3 eligible, pool exhausts
	}
}

func TestSampleDeterministicWithInjectedRand(t *testing.T) {
	// With rnd forced to always pick pool index 0, the partial Fisher-Yates draws a fixed
	// order; assert K bounds the result exactly.
	nodes := []*Node{nodeWith("a", 0), nodeWith("b", 0), nodeWith("c", 0), nodeWith("d", 0)}
	b := &BestOfK{K: 2, rnd: func(int) int { return 0 }}
	out := b.sample(nodes, nil)
	if len(out) != 2 {
		t.Fatalf("K=2 must bound the sample to 2, got %d", len(out))
	}
}

func TestNodeRefresh(t *testing.T) {
	rpc := &fakeRPC{ids: []string{"sb_1", "sb_2", "sb_3"}}
	n := NewNode("a", "proxy-a", rpc, DefaultCapacity)
	n.refresh()
	if got := n.count.Load(); got != 3 {
		t.Fatalf("refresh should cache the List length: got count=%d, want 3", got)
	}
	if !n.Ready() {
		t.Fatal("a successful List should mark the node ready")
	}
	// A List error marks the node not-ready (dropped from sampling) and leaves the last count.
	rpc.listErr = errors.New("connection refused")
	n.refresh()
	if n.Ready() {
		t.Fatal("a failed List should mark the node not-ready")
	}
	if got := n.count.Load(); got != 3 {
		t.Fatalf("a failed refresh should not clobber the last good count: got %d", got)
	}
}

func TestRegistryNodeByProxy(t *testing.T) {
	a := NewNode("grpc-a", "proxy-a", &fakeRPC{}, DefaultCapacity)
	b := NewNode("grpc-b", "proxy-b", &fakeRPC{}, DefaultCapacity)
	reg := NewStaticRegistry([]*Node{a, b}, DefaultK)
	if n, ok := reg.NodeByProxy("proxy-b"); !ok || n.ID != "grpc-b" {
		t.Fatalf("NodeByProxy(proxy-b) = %v, %v; want grpc-b", n, ok)
	}
	if _, ok := reg.NodeByProxy("proxy-unknown"); ok {
		t.Fatal("NodeByProxy of an unknown proxy should report not-found")
	}
}

func TestRegistryStartPrimesLoad(t *testing.T) {
	// Start does a synchronous prime poll before returning, so the first Pick sees real counts.
	a := NewNode("grpc-a", "proxy-a", &fakeRPC{ids: []string{"x", "y"}}, DefaultCapacity)
	b := NewNode("grpc-b", "proxy-b", &fakeRPC{ids: []string{"z"}}, DefaultCapacity)
	reg := NewStaticRegistry([]*Node{a, b}, len([]*Node{a, b})+1)
	reg.Start()
	defer reg.Stop()
	best, err := reg.Pick(nil)
	if err != nil {
		t.Fatal(err)
	}
	if best.ID != "grpc-b" {
		t.Fatalf("after prime poll b (1 sandbox) is emptier than a (2); got %s", best.ID)
	}
}
