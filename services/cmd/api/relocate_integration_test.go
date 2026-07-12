package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"google.golang.org/grpc/codes"

	"microsandbox/services/pkg/catalog"
	"microsandbox/services/pkg/placement"
	"microsandbox/services/pkg/store"
)

// mutableDiscovery is a Discovery whose node set + per-node status the test can flip, so it can
// drive a node into drain the way production does (discovery -> reconcile) rather than reach into
// placement's unexported state. Registry.Start()'s first poll reconciles synchronously, so a status
// change becomes visible without waiting on the background ticker.
type mutableDiscovery struct {
	mu    sync.Mutex
	infos []placement.NodeInfo
}

func (m *mutableDiscovery) ListNodes(context.Context) ([]placement.NodeInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]placement.NodeInfo(nil), m.infos...), nil
}

func (m *mutableDiscovery) setStatus(id, status string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.infos {
		if m.infos[i].ID == id {
			m.infos[i].Status = status
		}
	}
}

// callAPI drives one api handler in-process with an authenticated team + a path {id}, returning the
// recorder. It stands in for the withAuth middleware (which injects the team into the context) and
// the mux (which fills the path value), so the handler runs exactly as it would behind a live server.
func callAPI(h http.HandlerFunc, method, target, id, team, body string) *httptest.ResponseRecorder {
	ctx := context.WithValue(context.Background(), teamCtxKey{}, team)
	req := httptest.NewRequest(method, target, strings.NewReader(body)).WithContext(ctx)
	if id != "" {
		req.SetPathValue("id", id)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// relocReg builds an api backed by two fake orchestrators (A, B) over real localhost gRPC, a real
// SQLite store, and an in-memory catalog, plus the mutable discovery driving the fleet. The registry
// is NOT Started yet -- the caller Starts it after any drain flip so the synchronous first reconcile
// applies it. Returns the api, the two fakes, their nodes, and the discovery.
func relocSetup(t *testing.T) (*api, *fakeOrch, *fakeOrch, *placement.Node, *placement.Node, *mutableDiscovery) {
	t.Helper()
	addrA, fakeA := startFakeOrch(t, "A", codes.OK)
	addrB, fakeB := startFakeOrch(t, "B", codes.OK)
	nodeA, nodeB := nodeTo(t, addrA), nodeTo(t, addrB)
	disco := &mutableDiscovery{infos: []placement.NodeInfo{
		{ID: nodeA.ID, GRPC: nodeA.ID, Proxy: nodeA.Proxy},
		{ID: nodeB.ID, GRPC: nodeB.ID, Proxy: nodeB.Proxy},
	}}
	byID := map[string]*placement.Node{nodeA.ID: nodeA, nodeB.ID: nodeB}
	factory := func(in placement.NodeInfo) (*placement.Node, error) { return byID[in.ID], nil }
	reg := placement.NewRegistry(disco, factory, placement.DefaultK) // reconciles A,B (active) now

	st, err := store.Open(filepath.Join(t.TempDir(), "reloc.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	a := &api{registry: reg, store: st, catalog: catalog.NewInMemory(), dataURL: "http://data"}
	return a, fakeA, fakeB, nodeA, nodeB, disco
}

// createOn forces a create onto `target` by biasing the other node to look busier (both start at
// load 0 pre-poll), then returns the new sandbox id. It asserts the sandbox landed on target.
func createOn(t *testing.T, a *api, team string, target *fakeOrch, other *placement.Node) string {
	t.Helper()
	other.Reserve() // bias: the other node looks busier, so BestOfK picks the emptier target
	other.Reserve()
	rec := callAPI(a.handleCreate, "POST", "/sandboxes", "", team, `{"from_snapshot":true}`)
	other.Release()
	other.Release()
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if !target.holds(created.ID) {
		t.Fatalf("create should have landed on the target node, but it does not hold %s", created.ID)
	}
	return created.ID
}

// TestSandboxRelocatesOffDrainingNode is Stage 26's in-process proof (D5): a sandbox created on node
// A, paused, then resumed after A enters drain comes back on node B, and the catalog route + store
// follow the move. This is the multi-node relocation on one box (Stage 23/24 rationale: two real
// orchestrators on one box is not an E2B concept, so multi-node behavior is verified with fakes).
func TestSandboxRelocatesOffDrainingNode(t *testing.T) {
	const team = "team_reloc"
	a, fakeA, fakeB, nodeA, nodeB, disco := relocSetup(t)

	id := createOn(t, a, team, fakeA, nodeB)
	if route, ok, _ := a.catalog.Get(id); !ok || route.Node != nodeA.Proxy {
		t.Fatalf("create route: got %+v (ok=%v), want node=%s", route, ok, nodeA.Proxy)
	}

	// Pause: the sandbox leaves A, the route is dropped, the store records it paused on A.
	if rec := callAPI(a.handleSandboxPause, "POST", "/sandboxes/"+id+"/pause", id, team, ""); rec.Code != http.StatusOK {
		t.Fatalf("pause: got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if fakeA.holds(id) {
		t.Fatal("after pause A should no longer hold the sandbox")
	}
	if _, ok, _ := a.catalog.Get(id); ok {
		t.Fatal("after pause the catalog route should be gone")
	}
	if origin, _, paused, _ := a.store.PausedSandbox(id); !paused || origin != nodeA.Proxy {
		t.Fatalf("after pause store: paused=%v origin=%s, want true %s", paused, origin, nodeA.Proxy)
	}

	// A enters drain (discovery -> reconcile). Start()'s synchronous first poll applies it.
	disco.setStatus(nodeA.ID, placement.StatusDraining)
	a.registry.Start()
	defer a.registry.Stop()
	if n, _ := a.registry.NodeByID(nodeA.ID); n == nil || !n.Draining() {
		t.Fatal("A should be draining after reconcile")
	}

	// Resume: A is draining, so PickPreferred drops the origin affinity and relocates to B.
	if rec := callAPI(a.handleSandboxResume, "POST", "/sandboxes/"+id+"/resume", id, team, ""); rec.Code != http.StatusOK {
		t.Fatalf("resume: got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if fakeA.holds(id) || !fakeB.holds(id) {
		t.Fatalf("resume should have relocated to B: A.holds=%v B.holds=%v", fakeA.holds(id), fakeB.holds(id))
	}
	// The catalog route is rewritten to B, so the data path follows the sandbox to its new node.
	if route, ok, _ := a.catalog.Get(id); !ok || route.Node != nodeB.Proxy {
		t.Fatalf("resume route: got %+v (ok=%v), want node=%s", route, ok, nodeB.Proxy)
	}
	// The store shows it running again.
	if _, _, paused, _ := a.store.PausedSandbox(id); paused {
		t.Fatal("after resume the sandbox should no longer be paused")
	}
}

// TestSandboxResumeHonorsActiveOrigin is the complement: when the origin node is still eligible,
// resume comes back on it (affinity), NOT wherever load points -- so relocation happens only when
// the origin is actually draining, not on every resume. We bias the origin to look busier at resume
// time and assert it still returns there, proving affinity beats load.
func TestSandboxResumeHonorsActiveOrigin(t *testing.T) {
	const team = "team_affinity"
	a, fakeA, fakeB, nodeA, nodeB, _ := relocSetup(t)

	id := createOn(t, a, team, fakeA, nodeB)
	if rec := callAPI(a.handleSandboxPause, "POST", "/sandboxes/"+id+"/pause", id, team, ""); rec.Code != http.StatusOK {
		t.Fatalf("pause: got %d, want 200", rec.Code)
	}

	// A stays active. Bias A to look busier than B so a load-only choice would pick B; affinity must
	// override that and bring the sandbox back to A.
	nodeA.Reserve()
	nodeA.Reserve()
	defer func() { nodeA.Release(); nodeA.Release() }()
	if rec := callAPI(a.handleSandboxResume, "POST", "/sandboxes/"+id+"/resume", id, team, ""); rec.Code != http.StatusOK {
		t.Fatalf("resume: got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !fakeA.holds(id) || fakeB.holds(id) {
		t.Fatalf("resume should have honored the active origin A: A.holds=%v B.holds=%v", fakeA.holds(id), fakeB.holds(id))
	}
	if route, ok, _ := a.catalog.Get(id); !ok || route.Node != nodeA.Proxy {
		t.Fatalf("resume route: got %+v (ok=%v), want node=%s (origin)", route, ok, nodeA.Proxy)
	}
}
