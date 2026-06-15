package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// server owns the microVM fleet: it creates VMs (cold start or snapshot restore,
// optionally handed out from a warm pool), transparently proxies the data path to
// them over vsock, and destroys them. See docs/STAGE4_DESIGN.md (the control-plane
// split) and docs/STAGE5_DESIGN.md (the warm pool).
type server struct {
	vendorDir string
	pools     map[string]*pool // template name -> its warm pool; empty when no --pool/--pool-size

	mu        sync.Mutex          // guards sandboxes
	sandboxes map[string]*microVM // sandbox id -> running VM
}

// newServer builds the server and one warm pool per entry in poolSpecs (template name
// -> K): each pre-restores K snapshot VMs in the background, already health-probed, so
// a from_snapshot create for that template is served in ~ms. An empty poolSpecs keeps
// the original behavior of restoring on the request path. The pool's "make one VM"
// step is restoreHealthy -- the same one the unpooled from_snapshot path uses.
func newServer(vendorDir string, poolSpecs map[string]int) *server {
	s := &server{vendorDir: vendorDir, sandboxes: map[string]*microVM{}, pools: map[string]*pool{}}
	for name, k := range poolSpecs {
		// name was validated by parsePoolSpecs, so resolve cannot fail. tmpl is a
		// fresh per-iteration variable, so each closure captures its own template.
		tmpl, _ := resolveTemplate(vendorDir, name)
		p := newPool(k, func() (*microVM, error) { return restoreHealthy(vendorDir, tmpl) })
		s.pools[name] = p
		p.start()
	}
	return s
}

// poolFor returns the warm pool for a template, or nil if that template isn't pooled.
func (s *server) poolFor(tmpl template) *pool { return s.pools[tmpl.name] }

// repeatedFlag collects a flag passed more than once (--pool a=1 --pool b=2).
type repeatedFlag []string

func (r *repeatedFlag) String() string     { return strings.Join(*r, ",") }
func (r *repeatedFlag) Set(v string) error { *r = append(*r, v); return nil }

// parsePoolSpecs turns the CLI pool flags into a {template name -> warm count} map.
// --pool-size K is shorthand for the default template; --pool name=K (repeatable) sets
// a named one. A bad format, a non-positive K, an invalid template name, or the same
// template named twice (including default given via both flags) is a startup error.
func parsePoolSpecs(poolFlags []string, poolSize int) (map[string]int, error) {
	out := map[string]int{}
	add := func(name string, k int) error {
		if _, dup := out[name]; dup {
			return fmt.Errorf("template %q given more than one pool size", name)
		}
		if k <= 0 {
			return fmt.Errorf("pool size for %q must be > 0, got %d", name, k)
		}
		if _, err := resolveTemplate("", name); err != nil { // validate the name (path-independent)
			return err
		}
		out[name] = k
		return nil
	}
	if poolSize > 0 {
		if err := add(defaultTemplate, poolSize); err != nil {
			return nil, err
		}
	}
	for _, spec := range poolFlags {
		name, val, ok := strings.Cut(spec, "=")
		if !ok {
			return nil, fmt.Errorf("invalid --pool %q: want name=K", spec)
		}
		k, err := strconv.Atoi(val)
		if err != nil {
			return nil, fmt.Errorf("invalid --pool %q: K must be an integer", spec)
		}
		if err := add(name, k); err != nil {
			return nil, err
		}
	}
	return out, nil
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
		FromSnapshot bool   `json:"from_snapshot"`
		Template     string `json:"template"`
	}
	// A missing/empty/invalid body just means the defaults (cold start, default
	// template); the SDK is the only caller, so we stay lenient rather than 400 on
	// decode errors.
	_ = json.NewDecoder(r.Body).Decode(&req)

	// 6b: the sandbox's image is picked by the request's "template" field (empty = the
	// default image). An unknown/invalid name is the caller's error -> 400. 6c will
	// serve a matching template from a per-template warm pool.
	tmpl, err := resolveTemplate(s.vendorDir, req.Template)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	// Serve from a warm pool only if this template has one; otherwise restore/spawn
	// its own image inline. A pooled VM is always the right image -- each pool restores
	// from its template's own snapshot (newServer).
	p := s.poolFor(tmpl)
	var vm *microVM
	switch {
	case req.FromSnapshot && p != nil:
		vm, err = p.get()
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

// destroyAll terminates every running VM -- the warm pools' idle VMs first, then the
// active ones in the registry. Called on shutdown so nothing leaks.
func (s *server) destroyAll() {
	for _, p := range s.pools {
		p.drain()
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
