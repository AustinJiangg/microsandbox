package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"
)

// catalogClient writes sandbox routes to client-proxy's internal control port. It is the
// single-machine stand-in for "the api writes the catalog store" (E2B writes Redis); when
// the catalog swaps to Redis (Stage 12) this becomes a direct store write and the internal
// RPC disappears. See docs/STAGE9_DESIGN.md.
type catalogClient struct {
	base string // e.g. http://127.0.0.1:5008 (client-proxy's internal control port)
	http *http.Client
}

func newCatalogClient(internalAddr string) *catalogClient {
	return &catalogClient{
		base: "http://" + internalAddr,
		http: &http.Client{Timeout: 5 * time.Second},
	}
}

// Register records sandbox id -> node in the catalog (PUT /routes/{id}). The caller treats
// this as load-bearing: a sandbox with no route is unreachable on the data path, so a
// failure here rolls the just-built VM back rather than returning an unroutable sandbox.
func (c *catalogClient) Register(id, node string) error {
	body, _ := json.Marshal(map[string]string{"node": node})
	req, err := http.NewRequest(http.MethodPut, c.base+"/routes/"+id, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("client-proxy returned %s", resp.Status)
	}
	return nil
}

// Deregister drops a sandbox's route (DELETE /routes/{id}). Best-effort: a stale route
// pointing at a destroyed VM only yields a self-healing 404 on the data path, so a failure
// here is logged, not surfaced.
func (c *catalogClient) Deregister(id string) {
	req, err := http.NewRequest(http.MethodDelete, c.base+"/routes/"+id, nil)
	if err != nil {
		log.Printf("catalog: deregister %s: %v", id, err)
		return
	}
	resp, err := c.http.Do(req)
	if err != nil {
		log.Printf("catalog: deregister %s: %v", id, err)
		return
	}
	resp.Body.Close()
}
