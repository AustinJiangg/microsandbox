package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"microsandbox/services/pkg/fc"
	"microsandbox/services/pkg/proxy"
)

// handleData is the orchestrator's per-node data proxy: it bridges an HTTP request to
// the in-VM daemon (envd) over Firecracker's vsock. The sandbox is identified by the
// X-Sandbox-Id header (set by the api in Stage 8, by client-proxy in Stage 9), and the
// request path is the daemon endpoint (/execute, /files/*, /commands). This replaces
// the Stage-8a path-based handleProxy: header routing is exactly the contract
// client-proxy will speak, so Stage 9 just slots in front of it. See docs/STAGE8_DESIGN.md.
func (s *server) handleData(w http.ResponseWriter, r *http.Request) {
	id := r.Header.Get("X-Sandbox-Id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing X-Sandbox-Id header"})
		return
	}
	vm, ok := s.lookup(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such sandbox: " + id})
		return
	}
	// The daemon endpoint is the request path (e.g. /execute); VsockProxy re-adds the
	// leading slash, so hand it the path without it.
	rest := strings.TrimPrefix(r.URL.Path, "/")
	proxy.VsockProxy(vm.UDSPath, fc.VsockPort, rest).ServeHTTP(w, r)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
