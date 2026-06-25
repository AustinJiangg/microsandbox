package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

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

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	resp, err := a.client.Create(ctx, &pb.SandboxCreateRequest{
		Config: &pb.SandboxConfig{Template: req.Template, FromSnapshot: req.FromSnapshot},
	})
	if err != nil {
		writeGRPCError(w, err)
		return
	}

	// Register the sandbox's data-path route in the catalog (a Redis SET; client-proxy reads
	// it to route data requests). This is load-bearing -- a sandbox with no route is
	// unreachable -- so on failure (e.g. Redis down) we roll the just-built VM back (gRPC
	// Delete) rather than return a booted-but-unroutable zombie. This is a direct Redis write
	// now (Stage 14a), exactly like E2B's api; before 14a it went over an RPC to client-proxy.
	if err := a.catalog.Set(resp.GetSandboxId(), a.nodeAddr); err != nil {
		rb, cancelRB := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelRB()
		if _, derr := a.client.Delete(rb, &pb.SandboxDeleteRequest{SandboxId: resp.GetSandboxId()}); derr != nil {
			log.Printf("rollback: delete %s after failed route register: %v", resp.GetSandboxId(), derr)
		}
		writeJSON(w, http.StatusBadGateway,
			map[string]string{"error": "could not register sandbox route: " + err.Error()})
		return
	}

	// Record the sandbox in the durable metadata store. Best-effort: the VM is already
	// live and routable, so a metadata write failure is logged, not surfaced -- in this
	// single-node stage the orchestrator's in-memory registry is the operational truth,
	// and the store is the record that becomes authoritative across restarts / nodes.
	templateName := req.Template
	if templateName == "" {
		templateName = "default"
	}
	if err := a.store.InsertSandbox(resp.GetSandboxId(), templateName); err != nil {
		log.Printf("store: insert %s: %v", resp.GetSandboxId(), err)
	}
	// Hand the SDK the id plus where to reach its data path (client-proxy). The data_url
	// is constant across sandboxes this stage; Stage 12 makes it a per-sandbox hostname.
	writeJSON(w, http.StatusCreated, map[string]string{"id": resp.GetSandboxId(), "data_url": a.dataURL})
}

// handleDestroy: DELETE /sandboxes/{id} -> gRPC Delete -> drop from the store -> 204
// (or 404 on unknown id). The store delete is best-effort for the same reason as above.
func (a *api) handleDestroy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if _, err := a.client.Delete(ctx, &pb.SandboxDeleteRequest{SandboxId: id}); err != nil {
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

// handleList: GET /sandboxes -> 200 {"sandboxes":[{id,template,status,created_at}...]}.
// The api lists from its own metadata store (E2B's api lists from Postgres), not by
// asking the orchestrator -- the store is the api's durable record. The orchestrator
// still exposes a live gRPC List for reconciliation; we don't need it here yet.
func (a *api) handleList(w http.ResponseWriter, r *http.Request) {
	rows, err := a.store.ListSandboxes()
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
