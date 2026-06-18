package main

import (
	"context"
	"net/http"
	"net/http/httputil"
)

// nodeCtxKey carries the resolved node address from handleData into the shared reverse
// proxy's Rewrite. One ReverseProxy instance serves every request; the per-request target
// (which orchestrator) rides in the request context rather than being baked into a closure.
type nodeCtxKey struct{}

// newDataProxy builds the single reverse proxy used for every data request. FlushInterval
// -1 flushes every write so the daemon's SSE (/execute) streams live, matching the proxy
// discipline the api/orchestrator already use. Path and headers (including X-Sandbox-Id)
// pass through unchanged: the orchestrator data proxy routes by the header and treats the
// path as the daemon endpoint (/execute, /files/*, /commands).
func newDataProxy() *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			node, _ := pr.In.Context().Value(nodeCtxKey{}).(string)
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = node
			pr.Out.Host = node
		},
		FlushInterval: -1,
	}
}

// handleData is client-proxy's edge data path: read X-Sandbox-Id, resolve the node in the
// catalog, and reverse-proxy to that orchestrator's data proxy (-> vsock -> envd). A
// missing header is 400; an unknown sandbox is 404. This is the role the api's temporary
// passthrough plays in Stage 8 (api/handlers.go handleProxy); Stage 9 moves it here.
func (s *clientProxy) handleData(w http.ResponseWriter, r *http.Request) {
	id := r.Header.Get("X-Sandbox-Id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing X-Sandbox-Id header"})
		return
	}
	node, ok := s.catalog.Get(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no route for sandbox: " + id})
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), nodeCtxKey{}, node))
	s.proxy.ServeHTTP(w, r)
}
