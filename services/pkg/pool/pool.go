// Package pool keeps a small set of snapshot-restored, already-healthy VMs warm, so a
// `POST /sandboxes {"from_snapshot": true}` can be served by handing one out (~ms)
// instead of restoring on the request path. It is a control-plane-internal latency
// optimization: the wire protocol and the SDK are unaware it exists. See
// docs/STAGE5_DESIGN.md.
//
// Ported from control-plane/pool.go (Stage 8a). The one change made during the move:
// the pool operates on a VM interface{ Destroy() } rather than the concrete *microVM,
// so it stays decoupled from the fc package and unit-testable with a fake VM and no
// firecracker / KVM (pool_test.go) -- the same "inject the make-one-VM step" idea the
// original already used for `restore`.
package pool

import "sync"

// VM is the minimal handle the pool needs: it parks VMs, hands them out, and -- on
// drain or a late warm after Drain -- destroys them. The orchestrator's *fc.MicroVM
// satisfies this; the tests use a fake.
type VM interface {
	Destroy()
}

// Pool maintains up to size warm VMs. The "make one VM" step is injected (restore),
// so get/refill/drain are unit-testable with a fake restorer, mirroring how
// proxy_test.go covers the vsock bridge.
type Pool struct {
	size    int                // target number of warm VMs (K)
	restore func() (VM, error) // restore + health-probe one VM (off the request path)

	mu       sync.Mutex
	ready    []VM           // warm, healthy, waiting to be handed out
	inflight int            // warms in progress; the len+inflight<size cap lives on this
	closed   bool           // set by Drain(): stops refills and discards late warms
	warming  sync.WaitGroup // tracks in-flight warms so Drain() can wait them out
}

// New builds a pool of target size whose warm VMs are produced by restore.
func New(size int, restore func() (VM, error)) *Pool {
	return &Pool{size: size, restore: restore}
}

// Start kicks the initial background fill toward size. Call once after construction.
func (p *Pool) Start() { p.refill() }

// Get hands out a warm VM if one is ready (~ms); otherwise it restores one
// synchronously -- never worse than restoring on the request path with no pool at
// all. Either way it tops the pool back up toward size in the background.
func (p *Pool) Get() (VM, error) {
	p.mu.Lock()
	var vm VM
	if n := len(p.ready); n > 0 {
		vm = p.ready[n-1]
		p.ready = p.ready[:n-1]
	}
	p.mu.Unlock()

	defer p.refill() // refill from the post-take state, on hit and miss alike

	if vm != nil {
		return vm, nil
	}
	return p.restore()
}

// refill launches background warms until ready+inflight reaches size. Safe to call
// concurrently: the lock serializes the accounting, so warms never exceed the cap.
func (p *Pool) refill() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for !p.closed && len(p.ready)+p.inflight < p.size {
		p.inflight++
		p.warming.Add(1)
		go p.warmOne()
	}
}

// warmOne restores one VM and parks it in the ready queue. A failed warm is
// intentionally swallowed: the slot is reclaimed (inflight--) and the next Get()
// re-triggers a refill, while a persistent failure surfaces synchronously through
// Get()'s fallback restore. So the pool stays a best-effort accelerator and never
// hot-loops on a broken environment.
func (p *Pool) warmOne() {
	defer p.warming.Done()
	vm, err := p.restore()

	p.mu.Lock()
	p.inflight--
	keep := err == nil && vm != nil && !p.closed
	if keep {
		p.ready = append(p.ready, vm)
	}
	p.mu.Unlock()

	if !keep && vm != nil {
		vm.Destroy() // restore succeeded but we're draining: don't leak it
	}
}

// Drain destroys all warm VMs and stops further warming, so idle pooled VMs don't
// leak on shutdown. It then waits out in-flight warms (each self-destroys once it
// sees closed) -- bounded by the restore timeouts; at idle there are none, so it
// returns at once.
func (p *Pool) Drain() {
	p.mu.Lock()
	p.closed = true
	ready := p.ready
	p.ready = nil
	p.mu.Unlock()

	for _, vm := range ready {
		vm.Destroy()
	}
	p.warming.Wait()
}

// ReadyLen reports how many warm VMs are currently parked (test / diagnostic helper).
func (p *Pool) ReadyLen() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.ready)
}
