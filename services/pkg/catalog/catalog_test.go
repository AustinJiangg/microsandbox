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

	c.Set("sb_1", Route{Node: "127.0.0.1:5007", Token: "tok_a"})
	route, ok, _ := c.Get("sb_1")
	if !ok || route.Node != "127.0.0.1:5007" || route.Token != "tok_a" {
		t.Fatalf("Get after Set = (%+v, %v), want ({127.0.0.1:5007 tok_a}, true)", route, ok)
	}

	// Set overwrites (a sandbox could be re-registered on a different node / token).
	c.Set("sb_1", Route{Node: "127.0.0.1:6007", Token: "tok_b"})
	if route, _, _ := c.Get("sb_1"); route.Node != "127.0.0.1:6007" || route.Token != "tok_b" {
		t.Fatalf("Get after overwrite = %+v, want {127.0.0.1:6007 tok_b}", route)
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
		go func() { defer wg.Done(); c.Set(id, Route{Node: "127.0.0.1:5007", Token: "tok"}) }()
		go func() { defer wg.Done(); c.Get(id); c.Delete(id) }()
	}
	wg.Wait()
}
