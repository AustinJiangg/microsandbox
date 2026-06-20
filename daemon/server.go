// Package main is the in-VM daemon (Stage 7+): a Go rewrite of the Python server.py +
// backend.py, matching E2B's envd. As of Stage 11 the file / command / execute paths are
// ConnectRPC services -- envd.go (Filesystem + Process) and codeinterpreter.go (the
// stateful kernel) -- so the only plain-HTTP route left here is the GET /health liveness
// probe the orchestrator waits on. This file just wires the envd mux. See
// docs/STAGE11_DESIGN.md.
package main

import (
	"encoding/json"
	"net/http"
)

// newMux wires the envd HTTP surface: the /health probe plus the ConnectRPC Filesystem +
// Process services (envd.go). The code-interpreter service is served on its own listener
// (main.go). Kept separate from the listener so tests drive it over httptest -- no vsock.
func newMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	registerEnvdServices(mux)
	return mux
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// writeJSON marshals a small JSON body with a status (the /health reply).
func writeJSON(w http.ResponseWriter, status int, body any) {
	data, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
