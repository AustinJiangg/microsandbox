package catalog

import (
	"fmt"
	"sync"
	"testing"
)

// InMemory must satisfy the Catalog interface (the seam swapped for Redis later).
var _ Catalog = (*InMemory)(nil)

func TestSetGetDelete(t *testing.T) {
	c := NewInMemory()

	if _, ok, _ := c.Get("sb_1"); ok {
		t.Fatal("Get on an empty catalog returned ok=true")
	}

	c.Set("sb_1", "127.0.0.1:5007")
	node, ok, _ := c.Get("sb_1")
	if !ok || node != "127.0.0.1:5007" {
		t.Fatalf("Get after Set = (%q, %v), want (127.0.0.1:5007, true)", node, ok)
	}

	// Set overwrites (a sandbox could be re-registered on a different node).
	c.Set("sb_1", "127.0.0.1:6007")
	if node, _, _ := c.Get("sb_1"); node != "127.0.0.1:6007" {
		t.Fatalf("Get after overwrite = %q, want 127.0.0.1:6007", node)
	}

	c.Delete("sb_1")
	if _, ok, _ := c.Get("sb_1"); ok {
		t.Fatal("Get after Delete returned ok=true")
	}

	// Delete of an absent id is a no-op, not a panic.
	c.Delete("sb_absent")
}

// Concurrent access must be race-free (run with -race). Mirrors the data-request load:
// many readers (Get) against occasional writers (Set/Delete).
func TestConcurrentAccess(t *testing.T) {
	c := NewInMemory()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		id := fmt.Sprintf("sb_%d", i)
		wg.Add(2)
		go func() { defer wg.Done(); c.Set(id, "127.0.0.1:5007") }()
		go func() { defer wg.Done(); c.Get(id); c.Delete(id) }()
	}
	wg.Wait()
}
