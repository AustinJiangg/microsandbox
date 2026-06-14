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

// handleCreate: POST /sandboxes -- spawn (or restore from snapshot) a microVM.
// Body: {"from_snapshot": bool}. On success returns 201 {"id", "uds_path"}; in
// Stage 4a the SDK uses uds_path to connect over vsock and waits for health
// itself (Stage 4b moves both the proxy and the health probe in here).
func (s *server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FromSnapshot bool `json:"from_snapshot"`
	}
	// A missing/empty/invalid body just means the defaults (cold start); the SDK
	// is the only caller, so we stay lenient rather than 400 on decode errors.
	_ = json.NewDecoder(r.Body).Decode(&req)

	id := newID()
	var (
		vm  *microVM
		err error
	)
	if req.FromSnapshot {
		vm, err = restoreMicroVM(id, s.vendorDir)
	} else {
		vm, err = spawnMicroVM(id, s.vendorDir)
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	s.mu.Lock()
	s.sandboxes[id] = vm
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]string{"id": id, "uds_path": vm.udsPath})
}

// handleDestroy: DELETE /sandboxes/{id} -- kill the VM and clean up. Ported from
// client.py's close(). Unknown id -> 404.
func (s *server) handleDestroy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	vm, ok := s.sandboxes[id]
	if ok {
		delete(s.sandboxes, id)
	}
	s.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such sandbox: " + id})
		return
	}
	vm.destroy()
	w.WriteHeader(http.StatusNoContent)
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
