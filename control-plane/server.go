package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
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
// Body: {"from_snapshot": bool}. Blocks until the VM is healthy, then returns
// 201 {"id"}. The SDK reaches the VM only through the proxy (handleProxy), so it
// never needs the uds path.
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

	// Ready on delivery: block until the in-VM daemon answers /health, so the SDK
	// gets back an id only once the sandbox can actually run code. On failure the VM
	// would otherwise leak, so destroy it and report the guest serial tail.
	if err := waitHealthy(vm.udsPath, vsockPort, 10*time.Second); err != nil {
		tail := vm.consoleTail()
		vm.destroy()
		writeJSON(w, http.StatusInternalServerError,
			map[string]string{"error": fmt.Sprintf("sandbox %v; %s", err, tail)})
		return
	}

	s.mu.Lock()
	s.sandboxes[id] = vm
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
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

// handleProxy: ANY /sandboxes/{id}/<rest> -- transparently bridge the request to
// the in-VM daemon at /<rest> over vsock (the data path: /execute, /files/*,
// /commands). The control plane stays protocol-agnostic here -- it pipes bytes, so
// protocol.py remains the single source of truth.
func (s *server) handleProxy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.mu.Lock()
	vm, ok := s.sandboxes[id]
	s.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such sandbox: " + id})
		return
	}
	vsockProxy(vm.udsPath, vsockPort, r.PathValue("rest")).ServeHTTP(w, r)
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
