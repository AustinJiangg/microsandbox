package main

import "sync"

// pool keeps a small set of snapshot-restored, already-healthy microVMs warm, so a
// `POST /sandboxes {"from_snapshot": true}` can be served by handing one out (~ms)
// instead of restoring on the request path. It is a control-plane-internal latency
// optimization: the wire protocol and the SDK are unaware it exists. See
// docs/STAGE5_DESIGN.md.
//
// The "make one VM" step is injected (restore), so the get/refill/drain logic is
// unit-testable with a fake restorer and no VM/KVM (pool_test.go), mirroring how
// proxy_test.go covers the vsock bridge.
type pool struct {
	size    int                      // target number of warm VMs (K)
	restore func() (*microVM, error) // restore + health-probe one VM (off the request path)

	mu       sync.Mutex
	ready    []*microVM     // warm, healthy, waiting to be handed out
	inflight int            // warms in progress; the len+inflight<size cap lives on this
	closed   bool           // set by drain(): stops refills and discards late warms
	warming  sync.WaitGroup // tracks in-flight warms so drain() can wait them out
}

func newPool(size int, restore func() (*microVM, error)) *pool {
	return &pool{size: size, restore: restore}
}

// start kicks the initial background fill toward size. Call once after construction.
func (p *pool) start() { p.refill() }

// get hands out a warm VM if one is ready (~ms); otherwise it restores one
// synchronously -- never worse than restoring on the request path with no pool at
// all. Either way it tops the pool back up toward size in the background.
func (p *pool) get() (*microVM, error) {
	p.mu.Lock()
	var vm *microVM
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
func (p *pool) refill() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for !p.closed && len(p.ready)+p.inflight < p.size {
		p.inflight++
		p.warming.Add(1)
		go p.warmOne()
	}
}

// warmOne restores one VM and parks it in the ready queue. A failed warm is
// intentionally swallowed: the slot is reclaimed (inflight--) and the next get()
// re-triggers a refill, while a persistent failure surfaces synchronously through
// get()'s fallback restore. So the pool stays a best-effort accelerator and never
// hot-loops on a broken environment.
func (p *pool) warmOne() {
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
		vm.destroy() // restore succeeded but we're draining: don't leak it
	}
}

// drain destroys all warm VMs and stops further warming, so idle pooled VMs don't
// leak on shutdown. It then waits out in-flight warms (each self-destroys once it
// sees closed) -- bounded by the restore timeouts; at idle there are none, so it
// returns at once.
func (p *pool) drain() {
	p.mu.Lock()
	p.closed = true
	ready := p.ready
	p.ready = nil
	p.mu.Unlock()

	for _, vm := range ready {
		vm.destroy()
	}
	p.warming.Wait()
}

// readyLen reports how many warm VMs are currently parked (test / diagnostic helper).
func (p *pool) readyLen() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.ready)
}
