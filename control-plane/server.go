package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// server owns the microVM fleet: it creates VMs (cold start or snapshot restore,
// optionally handed out from a warm pool), transparently proxies the data path to
// them over vsock, and destroys them. See docs/STAGE4_DESIGN.md (the control-plane
// split) and docs/STAGE5_DESIGN.md (the warm pool).
type server struct {
	vendorDir string
	pool      *pool // warm pool for from_snapshot creates; nil when --pool-size 0

	mu        sync.Mutex          // guards sandboxes
	sandboxes map[string]*microVM // sandbox id -> running VM
}

// newServer builds the server and, when poolSize > 0, a warm pool that pre-restores
// that many snapshot VMs in the background (each already health-probed). poolSize 0
// keeps the original behavior of restoring on the request path. The pool's "make one
// VM" step is restoreHealthy -- the very same one the unpooled from_snapshot path uses.
func newServer(vendorDir string, poolSize int) *server {
	s := &server{vendorDir: vendorDir, sandboxes: map[string]*microVM{}}
	if poolSize > 0 {
		// 6a: the pool pre-warms the default template; 6c makes this a per-template
		// map. resolveTemplate(default) never errors.
		def, _ := resolveTemplate(vendorDir, defaultTemplate)
		s.pool = newPool(poolSize, func() (*microVM, error) { return restoreHealthy(vendorDir, def) })
		s.pool.start()
	}
	return s
}

// handleHealth is the control plane's *own* liveness, not a VM's. The SDK uses
// it to wait for the service to come up (e.g. the test fixture after launching
// the binary).
func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// healthyOrDestroy probes a freshly created VM and returns it once its in-VM daemon
// answers /health; on failure it destroys the VM (so nothing leaks) and returns an
// error carrying the guest serial tail for diagnostics. Shared by the cold-start, the
// unpooled restore, and the warm pool's pre-warm paths -- "ready on delivery" in one
// place.
func healthyOrDestroy(vm *microVM, err error) (*microVM, error) {
	if err != nil {
		return nil, err
	}
	if err := waitHealthy(vm.udsPath, vsockPort, 10*time.Second); err != nil {
		tail := vm.consoleTail()
		vm.destroy()
		return nil, fmt.Errorf("sandbox %v; %s", err, tail)
	}
	return vm, nil
}

// restoreHealthy / spawnHealthy mint an id, create a VM (restored from the snapshot /
// cold-started), and block until it is healthy.
func restoreHealthy(vendorDir string, tmpl template) (*microVM, error) {
	return healthyOrDestroy(restoreMicroVM(newID(), vendorDir, tmpl))
}

func spawnHealthy(vendorDir string, tmpl template) (*microVM, error) {
	return healthyOrDestroy(spawnMicroVM(newID(), vendorDir, tmpl))
}

// handleCreate: POST /sandboxes -- spawn (or restore from snapshot) a microVM.
// Body: {"from_snapshot": bool}. Returns 201 {"id"} only once the VM is healthy
// ("ready on delivery"). The SDK reaches the VM through the proxy (handleProxy), so
// it never needs the uds path.
//
// A from_snapshot create is served from the warm pool when one is configured
// (pool.get hands out a pre-warmed VM in ~ms, or restores synchronously if the pool
// is momentarily empty); otherwise it restores inline. Either way the VM is healthy
// before we return.
func (s *server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FromSnapshot bool `json:"from_snapshot"`
	}
	// A missing/empty/invalid body just means the defaults (cold start); the SDK
	// is the only caller, so we stay lenient rather than 400 on decode errors.
	_ = json.NewDecoder(r.Body).Decode(&req)

	// 6a: every sandbox uses the default template. 6b lets POST /sandboxes pick one
	// via a "template" field -- this resolve call is where that name will flow in --
	// and 6c serves it from a per-template warm pool. For the default constant resolve
	// never fails, but the error path is wired now so 6b only swaps the name in.
	tmpl, err := resolveTemplate(s.vendorDir, defaultTemplate)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	var vm *microVM
	switch {
	case req.FromSnapshot && s.pool != nil:
		vm, err = s.pool.get()
	case req.FromSnapshot:
		vm, err = restoreHealthy(s.vendorDir, tmpl)
	default:
		vm, err = spawnHealthy(s.vendorDir, tmpl)
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	s.mu.Lock()
	s.sandboxes[vm.id] = vm
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]string{"id": vm.id})
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

// destroyAll terminates every running VM -- the warm pool's idle VMs first, then the
// active ones in the registry. Called on shutdown so nothing leaks.
func (s *server) destroyAll() {
	if s.pool != nil {
		s.pool.drain()
	}
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
