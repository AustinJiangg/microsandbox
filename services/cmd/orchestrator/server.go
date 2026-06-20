package main

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"microsandbox/services/pkg/fc"
	"microsandbox/services/pkg/network"
	"microsandbox/services/pkg/pool"
	"microsandbox/services/pkg/proxy"
	"microsandbox/services/pkg/template"
)

// server owns the microVM fleet: it creates VMs (cold start or snapshot restore,
// optionally handed out from a warm pool), tracks them in a registry, and destroys
// them. In Stage 8b it is fronted by two listeners -- a gRPC SandboxService (grpc.go)
// for the lifecycle and an HTTP data proxy (dataproxy.go) for the vsock data path --
// but the fleet logic here is unchanged from the Stage 8a HTTP control plane: the
// handlers got thinner, the VM management did not move. See docs/STAGE8_DESIGN.md.
type server struct {
	vendorDir string
	pools     map[string]*pool.Pool // template name -> its warm pool; empty when no --pool/--pool-size
	net       *network.Manager      // Stage 12: per-sandbox netns/TAP/veth/DNAT slots (cold-start + restore paths)

	mu        sync.Mutex             // guards sandboxes
	sandboxes map[string]*fc.MicroVM // sandbox id -> running VM
}

// networkSlots caps concurrent per-sandbox network slots (Stage 12a). 256 is the third-octet
// limit in pkg/network's 10.0.<i>.0/30 scheme -- ample for one machine.
const networkSlots = 256

// newServer builds the server and one warm pool per entry in poolSpecs (template name
// -> K): each pre-restores K snapshot VMs in the background, already health-probed, so
// a from_snapshot create for that template is served in ~ms. An empty poolSpecs keeps
// the original behavior of restoring on the request path. The pool's "make one VM"
// step is restoreHealthy -- the same one the unpooled from_snapshot path uses.
func newServer(vendorDir string, poolSpecs map[string]int) *server {
	s := &server{
		vendorDir: vendorDir,
		sandboxes: map[string]*fc.MicroVM{},
		pools:     map[string]*pool.Pool{},
		net:       network.NewManager(networkSlots),
	}
	for name, k := range poolSpecs {
		// name was validated by parsePoolSpecs, so resolve cannot fail. tmpl is a
		// fresh per-iteration variable, so each closure captures its own template.
		tmpl, _ := template.Resolve(vendorDir, name)
		p := pool.New(k, func() (pool.VM, error) {
			vm, err := restoreHealthy(vendorDir, tmpl, s.net)
			if err != nil {
				return nil, err
			}
			return vm, nil
		})
		s.pools[name] = p
		p.Start()
	}
	return s
}

// poolFor returns the warm pool for a template, or nil if that template isn't pooled.
func (s *server) poolFor(tmpl template.Template) *pool.Pool { return s.pools[tmpl.Name] }

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
		if _, err := template.Resolve("", name); err != nil { // validate the name (path-independent)
			return err
		}
		out[name] = k
		return nil
	}
	if poolSize > 0 {
		if err := add(template.DefaultTemplate, poolSize); err != nil {
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

// --- VM fleet management: the verbs the gRPC SandboxService (grpc.go) calls ---

// create boots a sandbox of the given template (cold start, or snapshot restore which
// is warm-pool eligible), registers it, and returns it already health-probed. This is
// the old handleCreate switch, minus the HTTP plumbing.
func (s *server) create(fromSnapshot bool, tmpl template.Template) (*fc.MicroVM, error) {
	// Serve from a warm pool only if this template has one; otherwise restore/spawn
	// its own image inline. A pooled VM is always the right image -- each pool restores
	// from its template's own snapshot (newServer).
	p := s.poolFor(tmpl)
	var vm *fc.MicroVM
	var err error
	switch {
	case fromSnapshot && p != nil:
		var v pool.VM
		v, err = p.Get()
		if err == nil {
			vm = v.(*fc.MicroVM) // the pool only ever holds *fc.MicroVM (newServer's restore)
		}
	case fromSnapshot:
		vm, err = restoreHealthy(s.vendorDir, tmpl, s.net)
	default:
		vm, err = spawnHealthy(s.vendorDir, tmpl, s.net)
	}
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.sandboxes[vm.ID] = vm
	s.mu.Unlock()
	// Stage 12b: every VM (cold-started or restored) now has a network slot -- observe
	// (non-fatally) that it is reachable over its NIC via TCP, the path the data plane is moving
	// to. Logged only; the vsock path stays authoritative this sub-step, so a probe failure here
	// does not fail create.
	if vm.Slot != nil {
		go func(id, addr string) {
			if proxy.TCPHealthy(addr) {
				log.Printf("sandbox %s: TCP health OK at %s (NIC path live)", id, addr)
			} else {
				log.Printf("sandbox %s: TCP health probe failed at %s (vsock authoritative)", id, addr)
			}
		}(vm.ID, vm.Slot.Addr(fc.EnvdTCPPort))
	}
	return vm, nil
}

// destroy kills a sandbox by id and drops it from the registry; it reports whether the
// id existed (a false lets the gRPC layer answer NotFound).
func (s *server) destroy(id string) bool {
	s.mu.Lock()
	vm, ok := s.sandboxes[id]
	if ok {
		delete(s.sandboxes, id)
	}
	s.mu.Unlock()
	if !ok {
		return false
	}
	vm.Destroy()
	return true
}

// list returns the ids of all running sandboxes.
func (s *server) list() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.sandboxes))
	for id := range s.sandboxes {
		ids = append(ids, id)
	}
	return ids
}

// lookup returns the VM for an id, if known (used by the data proxy to find the vsock).
func (s *server) lookup(id string) (*fc.MicroVM, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	vm, ok := s.sandboxes[id]
	return vm, ok
}

// healthyOrDestroy probes a freshly created VM and returns it once its in-VM daemon
// answers /health; on failure it destroys the VM (so nothing leaks) and returns an
// error carrying the guest serial tail for diagnostics. Shared by the cold-start, the
// unpooled restore, and the warm pool's pre-warm paths -- "ready on delivery" in one
// place.
func healthyOrDestroy(vm *fc.MicroVM, err error) (*fc.MicroVM, error) {
	if err != nil {
		return nil, err
	}
	if err := proxy.WaitHealthy(vm.UDSPath, fc.VsockPort, 10*time.Second); err != nil {
		tail := vm.ConsoleTail()
		vm.Destroy()
		return nil, fmt.Errorf("sandbox %v; %s", err, tail)
	}
	return vm, nil
}

// restoreHealthy / spawnHealthy mint an id, create a VM (restored from the snapshot /
// cold-started), and block until it is healthy.
func restoreHealthy(vendorDir string, tmpl template.Template, netMgr *network.Manager) (*fc.MicroVM, error) {
	return healthyOrDestroy(fc.Restore(fc.NewID(), vendorDir, tmpl, netMgr))
}

func spawnHealthy(vendorDir string, tmpl template.Template, netMgr *network.Manager) (*fc.MicroVM, error) {
	return healthyOrDestroy(fc.Spawn(fc.NewID(), vendorDir, tmpl, netMgr))
}

// destroyAll terminates every running VM -- the warm pools' idle VMs first, then the
// active ones in the registry. Called on shutdown so nothing leaks.
func (s *server) destroyAll() {
	for _, p := range s.pools {
		p.Drain()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, vm := range s.sandboxes {
		vm.Destroy()
		delete(s.sandboxes, id)
	}
}
