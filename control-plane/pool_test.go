package main

// Unit tests for the warm pool (Stage 5b). The "make one VM" step is injected, so
// get / refill / drain / the size cap are all exercised with a fake restorer and no
// firecracker / KVM -- the same approach proxy_test.go takes for the vsock bridge.
// These run anywhere Go is installed.

import (
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

// eventually polls cond until it holds or the timeout elapses -- the pool refills in
// the background, so the assertions are on an eventual state, not an instant one.
func eventually(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

// fakeRestorer stands in for restoreHealthy: it produces dummy microVMs (a real temp
// workdir so destroy() is observable, but no firecracker process) and can be made to
// fail or to block mid-restore -- enough to drive every pool path without a VM.
type fakeRestorer struct {
	mu    sync.Mutex
	calls int
	fail  bool
	block chan struct{} // if non-nil, each restore waits on it (to observe the in-flight cap)
	dirs  []string      // workdirs handed to produced VMs, so a test can check destroy() ran
}

func (f *fakeRestorer) restore() (*microVM, error) {
	f.mu.Lock()
	f.calls++
	block, fail := f.block, f.fail
	f.mu.Unlock()

	if block != nil {
		<-block
	}
	if fail {
		return nil, fmt.Errorf("restore failed")
	}
	dir, err := os.MkdirTemp("", "pool-fake-vm-")
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.dirs = append(f.dirs, dir)
	f.mu.Unlock()
	// No proc, no udsPath: destroy() then only os.RemoveAll(workdir), which we observe.
	return &microVM{id: newID(), workdir: dir}, nil
}

func (f *fakeRestorer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeRestorer) workdirs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.dirs...)
}

func TestPoolFillsToSize(t *testing.T) {
	f := &fakeRestorer{}
	p := newPool(3, f.restore)
	p.start()

	eventually(t, 2*time.Second, func() bool { return p.readyLen() == 3 })
	if got := f.callCount(); got != 3 {
		t.Fatalf("restore called %d times, want exactly 3 (filled to size, no over-warming)", got)
	}
	p.drain()
}

func TestPoolGetHitsWarmThenRefills(t *testing.T) {
	f := &fakeRestorer{}
	p := newPool(2, f.restore)
	p.start()
	eventually(t, 2*time.Second, func() bool { return p.readyLen() == 2 })

	vm, err := p.get()
	if err != nil || vm == nil {
		t.Fatalf("get() = (%v, %v); want a warm VM", vm, err)
	}
	// The handed-out slot is refilled in the background, back up to size.
	eventually(t, 2*time.Second, func() bool { return p.readyLen() == 2 })
	if got := f.callCount(); got != 3 {
		t.Fatalf("restore called %d times, want 3 (2 initial + 1 refill)", got)
	}
	vm.destroy()
	p.drain()
}

func TestPoolGetFallsBackWhenEmpty(t *testing.T) {
	f := &fakeRestorer{}
	p := newPool(1, f.restore) // not started: the ready queue is empty

	vm, err := p.get()
	if err != nil || vm == nil {
		t.Fatalf("get() on an empty pool = (%v, %v); want a synchronously restored VM", vm, err)
	}
	if f.callCount() < 1 {
		t.Fatalf("expected at least one (synchronous) restore, got %d", f.callCount())
	}
	vm.destroy()
	p.drain()
}

func TestPoolGetSurfacesRestoreError(t *testing.T) {
	f := &fakeRestorer{fail: true}
	p := newPool(1, f.restore)

	if _, err := p.get(); err == nil {
		t.Fatal("get() on an empty pool with a failing restore should surface the error")
	}
	// Background warms fail too, but are swallowed: the pool stays empty, no hot loop.
	if n := p.readyLen(); n != 0 {
		t.Fatalf("readyLen = %d, want 0 after failed warms", n)
	}
	p.drain()
}

func TestPoolDrainDestroysAndStops(t *testing.T) {
	f := &fakeRestorer{}
	p := newPool(3, f.restore)
	p.start()
	eventually(t, 2*time.Second, func() bool { return p.readyLen() == 3 })
	dirs := f.workdirs()

	p.drain()

	if n := p.readyLen(); n != 0 {
		t.Fatalf("readyLen = %d after drain, want 0", n)
	}
	for _, d := range dirs {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("workdir %s still exists after drain; destroy() did not run", d)
		}
	}
	// Closed pool: refill is a no-op (no new restores).
	before := f.callCount()
	p.refill()
	if got := f.callCount(); got != before {
		t.Fatalf("refill after drain launched %d new restores, want 0", got-before)
	}
}

func TestPoolNeverExceedsSize(t *testing.T) {
	f := &fakeRestorer{block: make(chan struct{})}
	p := newPool(2, f.restore)
	p.start()

	// All `size` warms enter restore() and block there; the in-flight cap stops a 3rd.
	eventually(t, 2*time.Second, func() bool { return f.callCount() == 2 })
	time.Sleep(50 * time.Millisecond)
	if got := f.callCount(); got != 2 {
		t.Fatalf("restore called %d times while warms blocked, want exactly 2 (the size cap)", got)
	}

	close(f.block) // release; the warms complete and fill the pool
	eventually(t, 2*time.Second, func() bool { return p.readyLen() == 2 })
	p.drain()
}
