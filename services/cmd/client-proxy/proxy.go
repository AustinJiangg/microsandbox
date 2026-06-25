package main

import (
	"context"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
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

// handleData is client-proxy's edge data path (Stage 12b): parse the <port>-<id> hostname from
// the Host header, resolve the node in the catalog by id, and reverse-proxy to that
// orchestrator's data proxy (-> TCP -> the VM's NIC), handing the port over in X-Sandbox-Port.
// A malformed host is 400; an unknown sandbox is 404. Through Stage 11 this routed by an
// X-Sandbox-Id header and the orchestrator picked the in-VM service by a path prefix; Stage 12b
// lets the port in the hostname select it (envd 49983, code-interpreter 49999, user ports).
func (s *clientProxy) handleData(w http.ResponseWriter, r *http.Request) {
	port, id, ok := parseHostRoute(r.Host)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed host, want <port>-<sandboxId>"})
		return
	}
	node, found, err := s.catalog.Get(id)
	if err != nil {
		// The catalog (Redis) is unreachable: a dependency failure, not a missing sandbox.
		// 502 (not 404) so a transient Redis outage isn't mistaken for "no such sandbox" --
		// the same status the api uses for catalog trouble on the write path.
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "catalog lookup failed: " + err.Error()})
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no route for sandbox: " + id})
		return
	}
	// Hand the orchestrator the id (to find the VM) and the port (to dial); its data proxy still
	// routes by these headers, so the catalog interface stays the id->node map it was.
	r.Header.Set("X-Sandbox-Id", id)
	r.Header.Set("X-Sandbox-Port", port)
	r = r.WithContext(context.WithValue(r.Context(), nodeCtxKey{}, node))
	s.proxy.ServeHTTP(w, r)
}

// parseHostRoute splits a <port>-<sandboxId> host (optionally with a :port or a .suffix, which
// real wildcard DNS would carry) into its port and id. The id is sb_<hex> and has no dash, so
// the first dash is the separator; the port must be numeric.
func parseHostRoute(host string) (port, id string, ok bool) {
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i] // strip a :port if the client included one
	}
	if i := strings.IndexByte(host, '.'); i >= 0 {
		host = host[:i] // strip a .domain suffix (a real <port>-<id>.host deployment)
	}
	dash := strings.IndexByte(host, '-')
	if dash <= 0 {
		return "", "", false
	}
	port, id = host[:dash], host[dash+1:]
	if id == "" {
		return "", "", false
	}
	if _, err := strconv.Atoi(port); err != nil {
		return "", "", false
	}
	return port, id, true
}
