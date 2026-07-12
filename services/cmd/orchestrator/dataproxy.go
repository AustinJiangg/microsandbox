package main

import (
	"encoding/json"
	"net/http"
	"strconv"

	"microsandbox/services/pkg/proxy"
)

// handleData is the orchestrator's per-node data proxy: it bridges an HTTP request to the
// in-VM daemon over the VM's NIC (TCP). client-proxy identifies the sandbox + target port by
// parsing the <port>-<id> hostname and hands them over as X-Sandbox-Id + X-Sandbox-Port (Stage
// 12b); the request path is the daemon's ConnectRPC method. Through Stage 11 this bridged over
// vsock and picked the code-interpreter by a /codeinterpreter. path prefix; Stage 12b moved the
// data path onto TCP and let the port in the hostname select the service. See docs/STAGE12_DESIGN.md.
func (s *server) handleData(w http.ResponseWriter, r *http.Request) {
	id := r.Header.Get("X-Sandbox-Id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing X-Sandbox-Id header"})
		return
	}
	ls, ok := s.lookup(id)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such sandbox: " + id})
		return
	}
	// Stage 12b-2b: the data path is TCP over the VM's NIC now. client-proxy parsed the
	// <port>-<id> hostname and handed us the port in X-Sandbox-Port; the port -- not a path
	// prefix -- selects the in-VM service (49983 envd, 49999 code-interpreter, or any user port
	// in 12c). Dial the slot's routable address at that port; TCPProxy preserves the request path.
	port, err := strconv.Atoi(r.Header.Get("X-Sandbox-Port"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing or invalid X-Sandbox-Port header"})
		return
	}
	if ls.vm.Slot == nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "sandbox has no network slot: " + id})
		return
	}
	proxy.TCPProxy(ls.vm.Slot.Addr(port)).ServeHTTP(w, r)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
