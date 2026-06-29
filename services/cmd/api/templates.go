package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	pbt "microsandbox/services/pkg/grpc/templatemanager"
)

// handleTemplateCreate: POST /templates {name, dockerfile, with_snapshot, from} -> gRPC
// TemplateCreate -> record a build row -> 201 {build_id}. The build runs asynchronously in
// the orchestrator; the SDK polls GET /templates/builds/{id}. An optional `from` names a base
// template, making this a copy-on-write layered build (Stage 18): the rootfs is stored as a diff
// over `from`'s rather than whole.
func (a *api) handleTemplateCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name         string `json:"name"`
		Dockerfile   string `json:"dockerfile"`
		WithSnapshot *bool  `json:"with_snapshot"` // pointer: an absent field means the default (true)
		From         string `json:"from"`          // base template for a layered (COW) build; empty = a flat build
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
		Name: req.Name, Dockerfile: req.Dockerfile, WithSnapshot: withSnapshot, Base: req.From,
	})
	if err != nil {
		writeGRPCError(w, err)
		return
	}
	// Record the build owned by the caller's team (best-effort: the orchestrator's in-memory
	// registry is the live truth for polling; the store is the durable record for GET /templates).
	if err := a.store.InsertBuild(resp.GetBuildId(), req.Name, teamFromContext(r.Context())); err != nil {
		log.Printf("store: insert build %s: %v", resp.GetBuildId(), err)
	}
	writeJSON(w, http.StatusCreated, map[string]string{"build_id": resp.GetBuildId()})
}

// handleTemplateBuildStatus: GET /templates/builds/{id} -> authorise (the build must be the
// caller's team's) -> gRPC TemplateBuildStatus -> update the build row -> 200 {build_id, state,
// detail} (404 for an unknown id or another team's build).
func (a *api) handleTemplateBuildStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	owner, ok, err := a.store.BuildTeam(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "ownership lookup failed: " + err.Error()})
		return
	}
	if !ok || owner != teamFromContext(r.Context()) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such build: " + id})
		return
	}
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

// handleTemplateList: GET /templates -> 200 {"builds":[...]} from the store, scoped to the
// caller's team (the api's durable record of template builds, like E2B's api lists templates
// from Postgres).
func (a *api) handleTemplateList(w http.ResponseWriter, r *http.Request) {
	rows, err := a.store.ListBuilds(teamFromContext(r.Context()))
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
