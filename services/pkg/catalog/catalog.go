// Package catalog is the sandbox routing table: which node holds each sandbox, so the
// edge data proxy (client-proxy) can route a data request to the right orchestrator. It
// mirrors E2B's Redis "sandbox-catalog": the api writes a row when a sandbox is created
// (sandbox id -> node), and client-proxy reads it on every data request to route.
//
// As of Stage 14a this is a shared Redis (redis.go) that the api (writer) and client-proxy
// (reader) reach directly -- replacing the Stage 9 design where the table lived in-process
// inside client-proxy and the api mutated it over an internal control RPC. With a shared
// store both processes touch it directly, so that shim is gone. The Catalog interface is the
// seam: InMemory now survives only as a unit-test double. See docs/STAGE14_DESIGN.md.
package catalog

import "sync"

// Route is what a sandbox id maps to: the node that holds it plus, since Stage 16, the
// per-sandbox access token. Node is the orchestrator data-proxy address (e.g. "127.0.0.1:5007")
// that client-proxy reverse-proxies that sandbox's data path to; in E2B it is the orchestrator's
// IP and client-proxy hits :5007 on it -- same shape. Token is the secret minted by the api at
// create time; client-proxy requires it (an X-Access-Token header) before routing to the in-VM
// *control* services, leaving user-exposed ports public. See docs/STAGE16_DESIGN.md.
type Route struct {
	Node  string `json:"node"`
	Token string `json:"token,omitempty"`
}

// Catalog maps a sandbox id to its Route (node + access token).
//
// Every method returns an error because the real backend (Redis) is a network store that
// can fail -- and that failure is load-bearing: the api's create rolls the VM back if Set
// fails, and client-proxy must tell "Redis is down" (5xx) apart from "no such sandbox" (404)
// on Get. The in-process map can't fail, so InMemory always returns a nil error.
type Catalog interface {
	Set(id string, route Route) error
	Get(id string) (route Route, ok bool, err error)
	Delete(id string) error
}

// InMemory is a concurrency-safe in-memory Catalog. Since Stage 14a it is a unit-test
// double only (it backs the catalog/client-proxy unit tests through the interface); it can
// no longer back the running system, because the api and client-proxy are separate processes
// and only a shared store (Redis) is reachable by both. The map is tiny (one entry per live
// sandbox), so an RWMutex fits.
type InMemory struct {
	mu     sync.RWMutex
	routes map[string]Route
}

// NewInMemory returns an empty in-memory catalog.
func NewInMemory() *InMemory {
	return &InMemory{routes: map[string]Route{}}
}

// Set records (or overwrites) the route for a sandbox id. The in-memory map never fails.
func (c *InMemory) Set(id string, route Route) error {
	c.mu.Lock()
	c.routes[id] = route
	c.mu.Unlock()
	return nil
}

// Get returns the route for a sandbox id, and whether it is known. The error is always nil
// (it exists to satisfy the interface the Redis backend needs).
func (c *InMemory) Get(id string) (Route, bool, error) {
	c.mu.RLock()
	route, ok := c.routes[id]
	c.mu.RUnlock()
	return route, ok, nil
}

// Delete drops a sandbox's route. Idempotent: deleting an absent id is a no-op.
func (c *InMemory) Delete(id string) error {
	c.mu.Lock()
	delete(c.routes, id)
	c.mu.Unlock()
	return nil
}
