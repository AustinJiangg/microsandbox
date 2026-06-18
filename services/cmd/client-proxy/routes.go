package main

import (
	"encoding/json"
	"net/http"
)

// handleRouteSet: PUT /routes/{id} {"node": "host:port"} -> catalog.Set. The api calls this
// when a sandbox is created, registering which orchestrator data proxy holds it. This is
// the single-machine stand-in for "the api writes the catalog store" (E2B writes Redis);
// it lives on the internal control port, not the public data port.
func (s *clientProxy) handleRouteSet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Node string `json:"node"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Node == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": `want {"node": "host:port"}`})
		return
	}
	s.catalog.Set(r.PathValue("id"), body.Node)
	w.WriteHeader(http.StatusNoContent)
}

// handleRouteDelete: DELETE /routes/{id} -> catalog.Delete. The api calls this on destroy.
// Idempotent: deregistering an absent id is fine (the catalog Delete is a no-op).
func (s *clientProxy) handleRouteDelete(w http.ResponseWriter, r *http.Request) {
	s.catalog.Delete(r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}
