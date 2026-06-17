package pool

// Unit tests for the warm pool (Stage 5b). The "make one VM" step is injected, so
// Get / refill / Drain / the size cap are all exercised with a fake restorer and no
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

// fakeVM is a stand-in handle: Destroy() removes a real temp workdir, so a test can
// observe that drain / late-warm cleanup actually ran -- no firecracker process.
type fakeVM struct{ workdir string }

func (v *fakeVM) Destroy() {
	if v.workdir != "" {
		os.RemoveAll(v.workdir)
	}
}

// fakeRestorer stands in for the orchestrator's restoreHealthy: it produces dummy VMs
// (a real temp workdir so Destroy() is observable) and can be made to fail or to block
// mid-restore -- enough to drive every pool path without a VM.
type fakeRestorer struct {
	mu    sync.Mutex
	calls int
	fail  bool
	block chan struct{} // if non-nil, each restore waits on it (to observe the in-flight cap)
	dirs  []string      // workdirs handed to produced VMs, so a test can check Destroy() ran
}

func (f *fakeRestorer) restore() (VM, error) {
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
	// No proc, no uds: Destroy() then only os.RemoveAll(workdir), which we observe.
	return &fakeVM{workdir: dir}, nil
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
	p := New(3, f.restore)
	p.Start()

	eventually(t, 2*time.Second, func() bool { return p.ReadyLen() == 3 })
	if got := f.callCount(); got != 3 {
		t.Fatalf("restore called %d times, want exactly 3 (filled to size, no over-warming)", got)
	}
	p.Drain()
}

func TestPoolGetHitsWarmThenRefills(t *testing.T) {
	f := &fakeRestorer{}
	p := New(2, f.restore)
	p.Start()
	eventually(t, 2*time.Second, func() bool { return p.ReadyLen() == 2 })

	vm, err := p.Get()
	if err != nil || vm == nil {
		t.Fatalf("Get() = (%v, %v); want a warm VM", vm, err)
	}
	// The handed-out slot is refilled in the background, back up to size.
	eventually(t, 2*time.Second, func() bool { return p.ReadyLen() == 2 })
	if got := f.callCount(); got != 3 {
		t.Fatalf("restore called %d times, want 3 (2 initial + 1 refill)", got)
	}
	vm.Destroy()
	p.Drain()
}

func TestPoolGetFallsBackWhenEmpty(t *testing.T) {
	f := &fakeRestorer{}
	p := New(1, f.restore) // not started: the ready queue is empty

	vm, err := p.Get()
	if err != nil || vm == nil {
		t.Fatalf("Get() on an empty pool = (%v, %v); want a synchronously restored VM", vm, err)
	}
	if f.callCount() < 1 {
		t.Fatalf("expected at least one (synchronous) restore, got %d", f.callCount())
	}
	vm.Destroy()
	p.Drain()
}

func TestPoolGetSurfacesRestoreError(t *testing.T) {
	f := &fakeRestorer{fail: true}
	p := New(1, f.restore)

	if _, err := p.Get(); err == nil {
		t.Fatal("Get() on an empty pool with a failing restore should surface the error")
	}
	// Background warms fail too, but are swallowed: the pool stays empty, no hot loop.
	if n := p.ReadyLen(); n != 0 {
		t.Fatalf("ReadyLen = %d, want 0 after failed warms", n)
	}
	p.Drain()
}

func TestPoolDrainDestroysAndStops(t *testing.T) {
	f := &fakeRestorer{}
	p := New(3, f.restore)
	p.Start()
	eventually(t, 2*time.Second, func() bool { return p.ReadyLen() == 3 })
	dirs := f.workdirs()

	p.Drain()

	if n := p.ReadyLen(); n != 0 {
		t.Fatalf("ReadyLen = %d after drain, want 0", n)
	}
	for _, d := range dirs {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("workdir %s still exists after drain; Destroy() did not run", d)
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
	p := New(2, f.restore)
	p.Start()

	// All `size` warms enter restore() and block there; the in-flight cap stops a 3rd.
	eventually(t, 2*time.Second, func() bool { return f.callCount() == 2 })
	time.Sleep(50 * time.Millisecond)
	if got := f.callCount(); got != 2 {
		t.Fatalf("restore called %d times while warms blocked, want exactly 2 (the size cap)", got)
	}

	close(f.block) // release; the warms complete and fill the pool
	eventually(t, 2*time.Second, func() bool { return p.ReadyLen() == 2 })
	p.Drain()
}
