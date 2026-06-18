// Package catalog is the sandbox routing table: which node holds each sandbox, so the
// edge data proxy (client-proxy) can route a data request to the right orchestrator. It
// mirrors E2B's Redis "sandbox-catalog": the api writes a row when a sandbox is created
// (sandbox id -> node), and client-proxy reads it on every data request to route.
//
// On one machine we host this in-memory table inside client-proxy (the process that reads
// it on the hot path) and the api writes it over an internal RPC; the Catalog interface is
// the seam, so swapping the in-memory map for Redis later (Stage 12) is a change behind it,
// not above it. See docs/STAGE9_DESIGN.md.
package catalog

import "sync"

// Catalog maps a sandbox id to its node. "node" is the orchestrator data-proxy address
// (e.g. "127.0.0.1:5007") that client-proxy reverse-proxies that sandbox's data path to;
// in E2B it is the orchestrator's IP and client-proxy hits :5007 on it -- same shape.
type Catalog interface {
	Set(id, node string)
	Get(id string) (node string, ok bool)
	Delete(id string)
}

// InMemory is a concurrency-safe in-memory Catalog. client-proxy embeds one; the api
// mutates it remotely through client-proxy's internal control endpoints. The map is tiny
// (one entry per live sandbox) and read on every data request, so an RWMutex fits.
type InMemory struct {
	mu    sync.RWMutex
	nodes map[string]string
}

// NewInMemory returns an empty in-memory catalog.
func NewInMemory() *InMemory {
	return &InMemory{nodes: map[string]string{}}
}

// Set records (or overwrites) the node for a sandbox id.
func (c *InMemory) Set(id, node string) {
	c.mu.Lock()
	c.nodes[id] = node
	c.mu.Unlock()
}

// Get returns the node for a sandbox id, and whether it is known.
func (c *InMemory) Get(id string) (string, bool) {
	c.mu.RLock()
	node, ok := c.nodes[id]
	c.mu.RUnlock()
	return node, ok
}

// Delete drops a sandbox's route. Idempotent: deleting an absent id is a no-op.
func (c *InMemory) Delete(id string) {
	c.mu.Lock()
	delete(c.nodes, id)
	c.mu.Unlock()
}
