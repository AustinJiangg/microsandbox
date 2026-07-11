package main

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	pb "microsandbox/services/pkg/grpc/orchestrator"
	"microsandbox/services/pkg/placement"
)

// fakeOrch is an in-process SandboxService: a real gRPC server the api's placement path dials,
// backed by a counter instead of a VM. It lets us drive handleCreate's placement (BestOfK +
// in-progress) and failover deterministically, with no KVM/Firecracker. Its List length is the
// node's load signal, exactly as a real orchestrator's is.
type fakeOrch struct {
	pb.UnimplementedSandboxServiceServer
	name      string
	createErr codes.Code // OK = Create succeeds; else Create returns this code (a failing node)

	mu      sync.Mutex
	ids     []string
	creates int // total Create attempts (proves a failing node was tried before exclusion)
}

func (f *fakeOrch) Create(context.Context, *pb.SandboxCreateRequest) (*pb.SandboxCreateResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates++
	if f.createErr != codes.OK {
		return nil, status.Error(f.createErr, "fake "+f.name+" refusing create")
	}
	id := fmt.Sprintf("sb_%s_%d", f.name, len(f.ids))
	f.ids = append(f.ids, id)
	return &pb.SandboxCreateResponse{SandboxId: id}, nil
}

func (f *fakeOrch) Delete(_ context.Context, req *pb.SandboxDeleteRequest) (*emptypb.Empty, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, id := range f.ids {
		if id == req.GetSandboxId() {
			f.ids = append(f.ids[:i], f.ids[i+1:]...)
			return &emptypb.Empty{}, nil
		}
	}
	return nil, status.Error(codes.NotFound, "no such sandbox")
}

func (f *fakeOrch) List(context.Context, *emptypb.Empty) (*pb.SandboxListResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &pb.SandboxListResponse{SandboxIds: append([]string(nil), f.ids...)}, nil
}

func (f *fakeOrch) held() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.ids)
}

func (f *fakeOrch) attempts() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creates
}

// startFakeOrch stands up a fakeOrch on a real localhost gRPC listener and returns its address
// and the server (for assertions). The listener/server are torn down at test end.
func startFakeOrch(t *testing.T, name string, createErr codes.Code) (string, *fakeOrch) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeOrch{name: name, createErr: createErr}
	s := grpc.NewServer()
	pb.RegisterSandboxServiceServer(s, f)
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)
	return lis.Addr().String(), f
}

// nodeTo builds a placement.Node with a real gRPC client to addr.
func nodeTo(t *testing.T, addr string) *placement.Node {
	t.Helper()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return placement.NewNode(addr, "proxy-"+addr, pb.NewSandboxServiceClient(conn), placement.DefaultCapacity)
}

// TestPlaceCreateSpreadsAcrossNodes: with the registry unpolled (so cached load stays 0) and K
// = N (sample every node), placement is driven purely by in-progress load. Keeping every
// placement Reserved makes it a balanced fill -- BestOfK always picks a least-loaded node, so
// after N*per creates each node holds exactly `per`. This is the in-progress load-balancing.
func TestPlaceCreateSpreadsAcrossNodes(t *testing.T) {
	const nNodes, per = 4, 20
	var nodes []*placement.Node
	for i := 0; i < nNodes; i++ {
		addr, _ := startFakeOrch(t, fmt.Sprintf("n%d", i), codes.OK)
		nodes = append(nodes, nodeTo(t, addr))
	}
	reg := placement.NewRegistry(nodes, nNodes) // K = N -> sample all; do NOT Start() (no poll)
	a := &api{registry: reg}

	tally := map[string]int{}
	var held []*placement.Node
	for i := 0; i < nNodes*per; i++ {
		node, _, err := a.placeCreate(context.Background(), &pb.SandboxConfig{})
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
		tally[node.ID]++
		held = append(held, node) // keep Reserved so in-progress balances the next pick
	}
	for _, n := range nodes {
		if tally[n.ID] != per {
			t.Errorf("node %s got %d placements, want exactly %d (balanced fill)", n.ID, tally[n.ID], per)
		}
	}
	for _, n := range held {
		n.Release()
	}
}

// TestPlaceCreateFailsOverPastAFailingNode: a node whose Create returns a node-fault error
// (Internal) is excluded and the create lands on a healthy node -- every time. The good node is
// biased to look busier so the failing node is always sampled first (making "tried then
// excluded" deterministic), yet all real sandboxes still end up on the good node.
func TestPlaceCreateFailsOverPastAFailingNode(t *testing.T) {
	badAddr, bad := startFakeOrch(t, "bad", codes.Internal)
	goodAddr, good := startFakeOrch(t, "good", codes.OK)
	nodeBad, nodeGood := nodeTo(t, badAddr), nodeTo(t, goodAddr)
	nodeGood.Reserve() // bias: good looks busier, so the empty bad node is always picked first
	nodeGood.Reserve()
	reg := placement.NewRegistry([]*placement.Node{nodeBad, nodeGood}, 2)
	a := &api{registry: reg}

	const n = 10
	for i := 0; i < n; i++ {
		node, resp, err := a.placeCreate(context.Background(), &pb.SandboxConfig{})
		if err != nil {
			t.Fatalf("create %d should have failed over to the good node, got %v", i, err)
		}
		if node.Proxy != "proxy-"+goodAddr {
			t.Fatalf("create %d landed on the failing node %s", i, node.ID)
		}
		if resp.GetSandboxId() == "" {
			t.Fatalf("create %d: empty sandbox id", i)
		}
		node.Release()
	}
	if bad.attempts() != n {
		t.Errorf("the failing node should have been tried (then excluded) every time: got %d attempts, want %d", bad.attempts(), n)
	}
	if bad.held() != 0 {
		t.Errorf("the failing node must hold no sandboxes, got %d", bad.held())
	}
	if good.held() != n {
		t.Errorf("the good node should hold all %d sandboxes, got %d", n, good.held())
	}
}

// TestPlaceCreateReturnsLastErrorWhenAllNodesFail: when every node fails with a node-fault
// error, placeCreate returns that underlying error (mapped to 500), NOT ErrNoNode (503) -- so a
// genuine create failure isn't disguised as "out of capacity". Preserves the single-node 500.
func TestPlaceCreateReturnsLastErrorWhenAllNodesFail(t *testing.T) {
	a1, _ := startFakeOrch(t, "bad1", codes.Internal)
	a2, _ := startFakeOrch(t, "bad2", codes.Internal)
	reg := placement.NewRegistry([]*placement.Node{nodeTo(t, a1), nodeTo(t, a2)}, 2)
	a := &api{registry: reg}

	_, _, err := a.placeCreate(context.Background(), &pb.SandboxConfig{})
	if status.Code(err) != codes.Internal {
		t.Fatalf("want the underlying Internal error surfaced, got %v", err)
	}
}

// TestPlaceCreateDoesNotFailOverOnInvalidArgument: a request-fault error (bad template ->
// InvalidArgument) is returned immediately, WITHOUT trying other nodes -- otherwise a bad
// request would be retried across the fleet and surface as 503 instead of 400.
func TestPlaceCreateDoesNotFailOverOnInvalidArgument(t *testing.T) {
	badAddr, _ := startFakeOrch(t, "bad", codes.InvalidArgument)
	goodAddr, good := startFakeOrch(t, "good", codes.OK)
	nodeBad, nodeGood := nodeTo(t, badAddr), nodeTo(t, goodAddr)
	nodeGood.Reserve() // bias so the InvalidArgument node is picked first
	nodeGood.Reserve()
	reg := placement.NewRegistry([]*placement.Node{nodeBad, nodeGood}, 2)
	a := &api{registry: reg}

	_, _, err := a.placeCreate(context.Background(), &pb.SandboxConfig{})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("a request-fault error must be returned as-is (no failover), got %v", err)
	}
	if good.attempts() != 0 {
		t.Errorf("must NOT have failed over to the good node on a request fault, got %d attempts", good.attempts())
	}
}
