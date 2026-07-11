package placement

import (
	"context"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"
)

// nodeKeyPrefix namespaces the orchestrator service-registry keys in the shared Redis (the same
// Redis that holds the Stage-14a routing catalog under "sandbox:"). Each live orchestrator owns
// one key msb:node:<id> holding its NodeInfo JSON, written with a TTL and refreshed by a heartbeat
// (see Registrar); a crashed node's key simply expires and it drops out of discovery. This is the
// register/discover contract both RedisDiscovery (reader) and Registrar (writer) share.
const nodeKeyPrefix = "msb:node:"

func nodeKey(id string) string { return nodeKeyPrefix + id }

// redisOpTimeout bounds each registry round-trip so a wedged Redis can't stall the reconcile loop
// (discovery side) or a heartbeat (registrar side).
const redisOpTimeout = 3 * time.Second

// RedisDiscovery is a Discovery backed by the shared Redis service registry: ListNodes returns
// every orchestrator that currently has a live msb:node:<id> key. This is the single-box analogue
// of Consul/Nomad service discovery -- a key present means that node heartbeated within its TTL,
// so the list is self-cleaning (a dead node's key expires with no bookkeeping). It is one of the
// two Discovery implementations behind the same interface; StaticDiscovery (the --nodes flag) is
// the other. See docs/STAGE24_DESIGN.md §3.
type RedisDiscovery struct {
	rdb *redis.Client
}

// NewRedisDiscovery returns a RedisDiscovery reading the registry at addr (host:port). The client
// is lazy (connects on first use) and plaintext, matching pkg/catalog's Redis (NOT safe to expose).
func NewRedisDiscovery(addr string) *RedisDiscovery {
	return &RedisDiscovery{rdb: redis.NewClient(&redis.Options{Addr: addr})}
}

// Close releases the underlying Redis client.
func (d *RedisDiscovery) Close() error { return d.rdb.Close() }

// ListNodes scans the registry keyspace and decodes each live node's NodeInfo. It uses SCAN (not
// KEYS, which blocks Redis) over msb:node:*, then MGETs the values. A key that vanished between
// SCAN and MGET (its TTL raced) reads as nil and is skipped; a value that doesn't decode is
// skipped rather than failing the whole list. A transport error surfaces so the registry keeps its
// current fleet instead of wrongly emptying it on a blip.
func (d *RedisDiscovery) ListNodes(ctx context.Context) ([]NodeInfo, error) {
	cctx, cancel := context.WithTimeout(ctx, redisOpTimeout)
	defer cancel()

	var keys []string
	iter := d.rdb.Scan(cctx, 0, nodeKeyPrefix+"*", 100).Iterator()
	for iter.Next(cctx) {
		keys = append(keys, iter.Val())
	}
	if err := iter.Err(); err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, nil
	}
	vals, err := d.rdb.MGet(cctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	out := make([]NodeInfo, 0, len(vals))
	for _, v := range vals {
		s, ok := v.(string)
		if !ok { // nil: the key expired between SCAN and MGET; skip
			continue
		}
		var info NodeInfo
		if err := json.Unmarshal([]byte(s), &info); err != nil || info.ID == "" {
			continue // an unparseable / id-less value isn't a usable node
		}
		out = append(out, info)
	}
	return out, nil
}
