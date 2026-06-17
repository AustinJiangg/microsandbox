package main

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	pb "microsandbox/services/pkg/grpc/orchestrator"
)

// handleHealth is the api's own liveness (the test fixture waits on it before running).
func (a *api) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleCreate: POST /sandboxes {from_snapshot, template} -> gRPC Create -> 201 {id}.
// A missing/empty body means the defaults (cold start, default template), matching the
// pre-split behavior; the SDK is the only caller, so we stay lenient on decode errors.
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
	writeJSON(w, http.StatusCreated, map[string]string{"id": resp.GetSandboxId()})
}

// handleDestroy: DELETE /sandboxes/{id} -> gRPC Delete -> 204 (or 404 on unknown id).
func (a *api) handleDestroy(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if _, err := a.client.Delete(ctx, &pb.SandboxDeleteRequest{SandboxId: r.PathValue("id")}); err != nil {
		writeGRPCError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleList: GET /sandboxes -> gRPC List -> 200 {"sandboxes":[...]}. New in Stage 8b
// (the old control plane had no list endpoint); for now it reflects the orchestrator's
// live registry. Stage 8c serves it from the persisted metadata store instead.
func (a *api) handleList(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	resp, err := a.client.List(ctx, &emptypb.Empty{})
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sandboxes": resp.GetSandboxIds()})
}

// handleProxy: ANY /sandboxes/{id}/{rest...} -> the temporary data-path passthrough to
// the orchestrator's data proxy (Stage 8 only; removed in Stage 9).
func (a *api) handleProxy(w http.ResponseWriter, r *http.Request) {
	a.dataProxy.ServeHTTP(w, r)
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
