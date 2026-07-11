package placement

import (
	"context"

	"github.com/redis/go-redis/v9"
)

// drainKeyPrefix namespaces the api's per-node drain commands in the shared Redis (beside the
// msb:node:<id> registry keys the Registrar writes and the sandbox: routing catalog). A key
// present means an operator has drained that node through the api; the orchestrator's Registrar
// reads it each heartbeat and reflects it in the status it self-reports, so the api's discovery +
// reconcile stop BestOfK from picking the node. See docs/STAGE25_DESIGN.md D3.
const drainKeyPrefix = "msb:drain:"

func drainKey(id string) string { return drainKeyPrefix + id }

// DrainCommands is the api side of the Redis-mediated drain channel. It is the low-churn analogue
// of E2B's api->orchestrator ServiceStatusOverride RPC: api-initiated, but orchestrator-authoritative
// (the orchestrator reflects the command in its heartbeat, so drain survives an api restart and is
// seen by every api), and it reuses the Redis both sides already share rather than a new gRPC method
// (protoc is absent here) or an endpoint on the public data port. Only meaningful under
// --node-discovery redis; static mode has no heartbeat to carry the status (Decision D5).
type DrainCommands struct {
	rdb *redis.Client
}

// NewDrainCommands returns a DrainCommands writing to the shared Redis at addr (the same instance
// holding the routing catalog + service registry).
func NewDrainCommands(addr string) *DrainCommands {
	return &DrainCommands{rdb: redis.NewClient(&redis.Options{Addr: addr})}
}

// Close releases the underlying Redis client.
func (d *DrainCommands) Close() error { return d.rdb.Close() }

// Drain records that node id should stop taking new placements. The command persists (no TTL) so it
// outlives an api restart -- it is a durable instruction the orchestrator honors on every heartbeat
// until Resume, mirroring E2B keeping drain authoritative on the node rather than in the api.
func (d *DrainCommands) Drain(ctx context.Context, id string) error {
	cctx, cancel := context.WithTimeout(ctx, redisOpTimeout)
	defer cancel()
	return d.rdb.Set(cctx, drainKey(id), "1", 0).Err()
}

// Resume clears node id's drain command so it takes placements again on the orchestrator's next
// heartbeat (which then self-reports StatusActive).
func (d *DrainCommands) Resume(ctx context.Context, id string) error {
	cctx, cancel := context.WithTimeout(ctx, redisOpTimeout)
	defer cancel()
	return d.rdb.Del(cctx, drainKey(id)).Err()
}

// isDraining reports whether node id currently has a drain command. The orchestrator's Registrar
// calls it each heartbeat to decide the status it self-reports; a missing key means not draining.
func isDraining(ctx context.Context, rdb *redis.Client, id string) (bool, error) {
	cctx, cancel := context.WithTimeout(ctx, redisOpTimeout)
	defer cancel()
	n, err := rdb.Exists(cctx, drainKey(id)).Result()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
