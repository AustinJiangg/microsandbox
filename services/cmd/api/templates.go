package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	pbt "microsandbox/services/pkg/grpc/templatemanager"
)

// handleTemplateCreate: POST /templates {name, dockerfile, with_snapshot} -> gRPC
// TemplateCreate -> record a build row -> 201 {build_id}. The build runs asynchronously in
// the orchestrator; the SDK polls GET /templates/builds/{id}.
func (a *api) handleTemplateCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name         string `json:"name"`
		Dockerfile   string `json:"dockerfile"`
		WithSnapshot *bool  `json:"with_snapshot"` // pointer: an absent field means the default (true)
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	withSnapshot := true
	if req.WithSnapshot != nil {
		withSnapshot = *req.WithSnapshot
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	resp, err := a.templates.TemplateCreate(ctx, &pbt.TemplateCreateRequest{
		Name: req.Name, Dockerfile: req.Dockerfile, WithSnapshot: withSnapshot,
	})
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	// Record the build (best-effort: the orchestrator's in-memory registry is the live
	// truth for polling; the store is the durable record for GET /templates).
	if err := a.store.InsertBuild(resp.GetBuildId(), req.Name); err != nil {
		log.Printf("store: insert build %s: %v", resp.GetBuildId(), err)
	}
	writeJSON(w, http.StatusCreated, map[string]string{"build_id": resp.GetBuildId()})
}

// handleTemplateBuildStatus: GET /templates/builds/{id} -> gRPC TemplateBuildStatus ->
// update the build row -> 200 {build_id, state, detail} (or 404 for an unknown id).
func (a *api) handleTemplateBuildStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	resp, err := a.templates.TemplateBuildStatus(ctx, &pbt.TemplateBuildStatusRequest{BuildId: id})
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	state := buildStateString(resp.GetState())
	if err := a.store.UpdateBuild(id, state, resp.GetDetail()); err != nil {
		log.Printf("store: update build %s: %v", id, err)
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"build_id": id, "state": state, "detail": resp.GetDetail(),
	})
}

// handleTemplateList: GET /templates -> 200 {"builds":[...]} from the store (the api's
// durable record of template builds, like E2B's api lists templates from Postgres).
func (a *api) handleTemplateList(w http.ResponseWriter, r *http.Request) {
	rows, err := a.store.ListBuilds()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	list := make([]map[string]any, 0, len(rows))
	for _, b := range rows {
		list = append(list, map[string]any{
			"build_id": b.BuildID, "name": b.Name, "state": b.State,
			"detail": b.Detail, "created_at": b.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"builds": list})
}

// buildStateString maps the gRPC build-state enum to the lowercase string the SDK and the
// metadata store use ("building" / "success" / "failed").
func buildStateString(s pbt.TemplateBuildStatusResponse_State) string {
	switch s {
	case pbt.TemplateBuildStatusResponse_SUCCESS:
		return "success"
	case pbt.TemplateBuildStatusResponse_FAILED:
		return "failed"
	default:
		return "building"
	}
}
