package catalog

import (
	"os"
	"testing"
)

// TestRedisRoundTrip exercises the Redis catalog against a live server. It self-skips unless
// REDIS_ADDR points at one (the e2e fixture / CI sets it via docker-compose), so
// `go test ./services/...` stays hermetic and dependency-free on a bare box -- the same
// discipline pkg/uffd's tests use. See docs/STAGE14_DESIGN.md, Decision 5.
func TestRedisRoundTrip(t *testing.T) {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		t.Skip("REDIS_ADDR not set; skipping live Redis catalog test")
	}
	c := NewRedis(addr)
	defer c.Close()

	const id = "sb_redis_test"
	_ = c.Delete(id) // clear any leftover from a prior aborted run

	// A missing key is a miss, not an error: ok=false, err=nil (client-proxy turns this into
	// a 404, never a 5xx).
	if route, ok, err := c.Get(id); err != nil || ok || route.Node != "" {
		t.Fatalf("Get(absent) = (%+v, %v, %v), want ({}, false, nil)", route, ok, err)
	}

	if err := c.Set(id, Route{Node: "127.0.0.1:5007", Token: "tok_a"}); err != nil {
		t.Fatalf("Set: %v", err)
	}
	route, ok, err := c.Get(id)
	if err != nil || !ok || route.Node != "127.0.0.1:5007" || route.Token != "tok_a" {
		t.Fatalf("Get after Set = (%+v, %v, %v), want ({127.0.0.1:5007 tok_a}, true, nil)", route, ok, err)
	}

	// Set overwrites (a sandbox could be re-registered on a different node / token).
	if err := c.Set(id, Route{Node: "127.0.0.1:6007", Token: "tok_b"}); err != nil {
		t.Fatalf("Set overwrite: %v", err)
	}
	if route, _, _ := c.Get(id); route.Node != "127.0.0.1:6007" || route.Token != "tok_b" {
		t.Fatalf("Get after overwrite = %+v, want {127.0.0.1:6007 tok_b}", route)
	}

	if err := c.Delete(id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok, _ := c.Get(id); ok {
		t.Fatal("Get after Delete returned ok=true")
	}
	// Delete of an absent id is a no-op (Redis DEL returns 0, not an error).
	if err := c.Delete(id); err != nil {
		t.Fatalf("Delete(absent): %v", err)
	}
}
