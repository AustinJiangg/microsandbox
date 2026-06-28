package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// keyPrefix namespaces catalog keys in Redis. E2B uses "sandbox:catalog:<id>"; we use the
// shorter "sandbox:<id>" -> node. A prefix keeps the routing table greppable and lets other
// state share the same Redis later without colliding.
const keyPrefix = "sandbox:"

// opTimeout bounds each catalog round-trip. The lookup is per data *request* (not per byte),
// and on loopback Redis answers in well under a millisecond, so a short ceiling just stops a
// dead Redis from hanging the api's create or a client-proxy data request indefinitely.
const opTimeout = 5 * time.Second

// Redis is a Catalog backed by a shared Redis -- the cross-process routing table the api
// (writer, on create/destroy) and client-proxy (reader, on every data request) both reach
// directly. It is the Stage 14a replacement for the in-memory map + the api->client-proxy
// control RPC: a shared store both processes can dial makes that shim unnecessary, so the
// topology gets one step closer to E2B (which routes off a Redis "sandbox-catalog"). See
// docs/STAGE14_DESIGN.md.
type Redis struct {
	rdb *redis.Client
}

// Redis must satisfy the Catalog interface (the seam the in-memory map used to fill).
var _ Catalog = (*Redis)(nil)

// NewRedis returns a Catalog backed by the Redis at addr (host:port). The client is lazy --
// it connects on first use, not here -- and unauthenticated/plaintext, matching every other
// host service in this single-box learning setup (NOT safe to expose).
func NewRedis(addr string) *Redis {
	return &Redis{rdb: redis.NewClient(&redis.Options{Addr: addr})}
}

// Set records (or overwrites) the route for a sandbox id, stored as a small JSON blob at one
// key (as the bare node string was before Stage 16). No TTL (0): the api explicitly Deletes on
// destroy, so we don't rely on expiry for correctness. A non-nil error is load-bearing -- the
// api rolls the just-built VM back rather than leave an unroutable zombie.
func (c *Redis) Set(id string, route Route) error {
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	blob, err := json.Marshal(route)
	if err != nil {
		return err
	}
	return c.rdb.Set(ctx, keyPrefix+id, blob, 0).Err()
}

// Get returns the route for a sandbox id. A missing key is (Route{}, false, nil) -- a genuine
// "no such sandbox", which client-proxy turns into a 404. A transport/server failure is
// (Route{}, false, err) -- distinct from a miss, so client-proxy can answer 5xx instead of
// wrongly claiming the sandbox doesn't exist. A value that doesn't parse as JSON is treated as
// a legacy bare-node string (a pre-Stage-16 entry), so a Redis surviving the upgrade still routes.
func (c *Redis) Get(id string) (Route, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	val, err := c.rdb.Get(ctx, keyPrefix+id).Result()
	if errors.Is(err, redis.Nil) {
		return Route{}, false, nil
	}
	if err != nil {
		return Route{}, false, err
	}
	var route Route
	if jerr := json.Unmarshal([]byte(val), &route); jerr != nil {
		return Route{Node: val}, true, nil // legacy bare-node value (no token)
	}
	return route, true, nil
}

// Delete drops a sandbox's route. Idempotent: Redis DEL on an absent key returns 0, not an
// error, so deregistering an unknown id is a no-op (the api treats Delete as best-effort).
func (c *Redis) Delete(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	return c.rdb.Del(ctx, keyPrefix+id).Err()
}

// Close releases the Redis connection pool. main() defers it, mirroring the gRPC conn and
// the metadata store.
func (c *Redis) Close() error { return c.rdb.Close() }
