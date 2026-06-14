package main

import (
	"encoding/json"
	"net/http"
	"sync"
)

// server owns the microVM fleet. Stage 4a fills in create/destroy; Stage 4b
// fills in the transparent vsock proxy. In this scaffold step only /health is
// real -- the lifecycle and proxy handlers return 501 until the next sub-step.
type server struct {
	vendorDir string

	mu        sync.Mutex          // guards sandboxes
	sandboxes map[string]*microVM // sandbox id -> running VM
}

func newServer(vendorDir string) *server {
	return &server{vendorDir: vendorDir, sandboxes: map[string]*microVM{}}
}

// handleHealth is the control plane's *own* liveness, not a VM's. The SDK uses
// it to wait for the service to come up (e.g. the test fixture after launching
// the binary).
func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleCreate: POST /sandboxes -- spawn or restore a microVM. (Stage 4a, next sub-step.)
func (s *server) handleCreate(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "create not implemented yet"})
}

// handleDestroy: DELETE /sandboxes/{id} -- kill the VM and clean up. (Stage 4a, next sub-step.)
func (s *server) handleDestroy(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "destroy not implemented yet"})
}

// handleProxy: ANY /sandboxes/{id}/... -- transparent vsock bridge to the in-VM
// daemon. (Stage 4b.)
func (s *server) handleProxy(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "proxy not implemented yet"})
}

// destroyAll terminates every running VM. Called on shutdown so nothing leaks.
func (s *server) destroyAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, vm := range s.sandboxes {
		vm.destroy()
		delete(s.sandboxes, id)
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
