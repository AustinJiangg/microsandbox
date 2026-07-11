package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"microsandbox/services/pkg/catalog"
	pb "microsandbox/services/pkg/grpc/orchestrator"
)

// handleHealth is the api's own liveness (the test fixture waits on it before running).
func (a *api) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleCreate: POST /sandboxes {from_snapshot, template} -> gRPC Create -> record in
// the store -> 201 {id}. A missing/empty body means the defaults (cold start, default
// template), matching the pre-split behavior; the SDK is the only caller, so we stay
// lenient on decode errors.
func (a *api) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FromSnapshot bool   `json:"from_snapshot"`
		Template     string `json:"template"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	// Pick a node for this sandbox (Stage 23: BestOfK over the fleet; a one-node fleet always
	// returns that node, so this is a no-op refactor there). ErrNoNode means the whole fleet is
	// unreachable/at capacity -- a 503, distinct from a bad request.
	node, err := a.registry.Pick(nil)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no orchestrator node available: " + err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	resp, err := node.RPC.Create(ctx, &pb.SandboxCreateRequest{
		Config: &pb.SandboxConfig{Template: req.Template, FromSnapshot: req.FromSnapshot},
	})
	if err != nil {
		writeGRPCError(w, err)
		return
	}

	// Mint the per-sandbox data-plane access token (Stage 16). The rollback helper tears the
	// just-built VM down (on the node that built it) on any failure between here and a
	// successful route register, so a booted-but-unusable VM never leaks.
	rollback := func() {
		rb, cancelRB := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelRB()
		if _, derr := node.RPC.Delete(rb, &pb.SandboxDeleteRequest{SandboxId: resp.GetSandboxId()}); derr != nil {
			log.Printf("rollback: delete %s: %v", resp.GetSandboxId(), derr)
		}
	}
	token, err := newAccessToken()
	if err != nil {
		rollback()
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not mint access token: " + err.Error()})
		return
	}

	// Register the sandbox's data-path route (node + token) in the catalog (a Redis SET;
	// client-proxy reads it to route and to gate the data path). This is load-bearing -- a
	// sandbox with no route is unreachable -- so on failure (e.g. Redis down) we roll the
	// just-built VM back rather than return a booted-but-unroutable zombie. This is a direct
	// Redis write now (Stage 14a), exactly like E2B's api; before 14a it went over an RPC.
	if err := a.catalog.Set(resp.GetSandboxId(), catalog.Route{Node: node.Proxy, Token: token}); err != nil {
		rollback()
		writeJSON(w, http.StatusBadGateway,
			map[string]string{"error": "could not register sandbox route: " + err.Error()})
		return
	}

	// Record the sandbox in the durable metadata store, owned by the caller's team. Best-effort:
	// the VM is already live and routable, so a metadata write failure is logged, not surfaced --
	// in this single-node stage the orchestrator's in-memory registry is the operational truth,
	// and the store is the record that becomes authoritative across restarts / nodes.
	team := teamFromContext(r.Context())
	templateName := req.Template
	if templateName == "" {
		templateName = "default"
	}
	if err := a.store.InsertSandbox(resp.GetSandboxId(), templateName, team); err != nil {
		log.Printf("store: insert %s: %v", resp.GetSandboxId(), err)
	}
	// Hand the SDK the id, where to reach its data path (client-proxy), and the access token
	// the SDK must send (X-Access-Token) on data calls to the in-VM control services (Stage 16).
	writeJSON(w, http.StatusCreated, map[string]string{
		"id": resp.GetSandboxId(), "data_url": a.dataURL, "token": token})
}

// handleDestroy: DELETE /sandboxes/{id} -> authorise (the sandbox must belong to the caller's
// team) -> gRPC Delete -> drop from the store -> 204 (404 on unknown id or another team's
// sandbox). The ownership check precedes the gRPC Delete on purpose: we must never tear down
// a VM the caller doesn't own. A sandbox that isn't the team's is reported as 404, not 403, so
// we don't even admit it exists. The store delete is best-effort for the reason above.
func (a *api) handleDestroy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	team := teamFromContext(r.Context())
	owner, ok, err := a.store.SandboxTeam(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "ownership lookup failed: " + err.Error()})
		return
	}
	if !ok || owner != team {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such sandbox: " + id})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := a.deleteOnHoldingNode(ctx, id); err != nil {
		writeGRPCError(w, err)
		return
	}
	// Drop the catalog route (best-effort: a stale route only yields a self-healing 404)
	// and the durable metadata row.
	if err := a.catalog.Delete(id); err != nil {
		log.Printf("catalog: delete route %s: %v", id, err)
	}
	if err := a.store.DeleteSandbox(id); err != nil {
		log.Printf("store: delete %s: %v", id, err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// deleteOnHoldingNode routes a Delete to the orchestrator node that holds the sandbox. The
// catalog records it as Route.Node (the node's data-proxy address), so we resolve that to the
// node and Delete there. If the route is missing/unresolvable (a stale catalog, or Redis is
// down), we fall back to broadcasting Delete to every node: only the holder deletes it, the
// rest answer NotFound harmlessly, and the call succeeds if any node held it. At a one-node
// fleet the primary path always resolves, so this is behavior-identical to the pre-Stage-23
// single-client Delete.
func (a *api) deleteOnHoldingNode(ctx context.Context, id string) error {
	if route, ok, err := a.catalog.Get(id); err == nil && ok {
		if node, found := a.registry.NodeByProxy(route.Node); found {
			_, derr := node.RPC.Delete(ctx, &pb.SandboxDeleteRequest{SandboxId: id})
			return derr
		}
	}
	lastErr := error(status.Error(codes.NotFound, "no such sandbox: "+id))
	for _, node := range a.registry.Nodes() {
		if _, derr := node.RPC.Delete(ctx, &pb.SandboxDeleteRequest{SandboxId: id}); derr == nil {
			return nil
		} else {
			lastErr = derr
		}
	}
	return lastErr
}

// handleList: GET /sandboxes -> 200 {"sandboxes":[{id,template,status,created_at}...]}, scoped
// to the caller's team. The api lists from its own metadata store (E2B's api lists from
// Postgres), not by asking the orchestrator -- the store is the api's durable record. The
// orchestrator still exposes a live gRPC List for reconciliation; we don't need it here yet.
func (a *api) handleList(w http.ResponseWriter, r *http.Request) {
	rows, err := a.store.ListSandboxes(teamFromContext(r.Context()))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	list := make([]map[string]any, 0, len(rows))
	for _, sb := range rows {
		list = append(list, map[string]any{
			"id": sb.ID, "template": sb.Template, "status": sb.Status, "created_at": sb.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sandboxes": list})
}

// writeGRPCError maps a gRPC status code back to the HTTP status the SDK expects,
// preserving the {"error": ...} body shape the old control plane used. This is what
// keeps the SDK's behavior byte-stable across the gRPC split: a bad template still
// surfaces as 400, an unknown sandbox as 404, anything else as 500.
func writeGRPCError(w http.ResponseWriter, err error) {
	httpStatus := http.StatusInternalServerError
	switch status.Code(err) {
	case codes.InvalidArgument:
		httpStatus = http.StatusBadRequest
	case codes.NotFound:
		httpStatus = http.StatusNotFound
	}
	msg := err.Error()
	if st, ok := status.FromError(err); ok {
		msg = st.Message() // the clean message, without the gRPC "rpc error: code = ..." prefix
	}
	writeJSON(w, httpStatus, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
