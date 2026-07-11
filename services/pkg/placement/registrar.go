package placement

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

// DefaultNodeTTL / DefaultNodeHeartbeat: an orchestrator re-writes its registry key every 1s and
// the key expires after 3s (~3x), so one missed heartbeat is tolerated but a crashed node drops
// out of discovery within ~3s. This mirrors a Consul TTL health check -- the TTL is the liveness
// signal, no separate health probe.
const (
	DefaultNodeTTL       = 3 * time.Second
	DefaultNodeHeartbeat = 1 * time.Second
)

// Registrar is the orchestrator side of the Redis service registry: it heartbeats this node's
// NodeInfo into msb:node:<id> with a TTL (so the api's RedisDiscovery sees it) and DELetes the key
// on graceful shutdown (so a clean stop leaves the fleet immediately; a crash relies on the TTL).
// This is the "register/deregister" half of Stage 24's dynamic discovery -- the single-box analogue
// of an orchestrator registering itself with Consul/Nomad. See docs/STAGE24_DESIGN.md §3.
type Registrar struct {
	rdb  *redis.Client
	self NodeInfo
	ttl  time.Duration // key expiry: a missed beat within ttl doesn't evict, a crash does
	beat time.Duration // heartbeat period (< ttl): how often the key is re-written
	stop chan struct{}
	done chan struct{}
}

// NewRegistrar builds a registrar that advertises self into the Redis at addr. ttl<=0 / beat<=0
// fall back to the defaults, and beat is clamped below ttl so a live node never lets its key lapse.
func NewRegistrar(addr string, self NodeInfo, ttl, beat time.Duration) *Registrar {
	if ttl <= 0 {
		ttl = DefaultNodeTTL
	}
	if beat <= 0 {
		beat = DefaultNodeHeartbeat
	}
	if beat >= ttl {
		beat = ttl / 2
	}
	return &Registrar{
		rdb:  redis.NewClient(&redis.Options{Addr: addr}),
		self: self,
		ttl:  ttl,
		beat: beat,
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
}

// Start writes the node's key once (so it is discoverable immediately, before the first tick) and
// then re-writes it every beat until Stop, in a goroutine. A write error is logged and retried on
// the next beat -- a transient Redis blip shouldn't take the orchestrator down.
func (r *Registrar) Start() {
	if err := r.register(); err != nil {
		log.Printf("registrar: initial register failed: %v", err)
	}
	go func() {
		defer close(r.done)
		t := time.NewTicker(r.beat)
		defer t.Stop()
		for {
			select {
			case <-r.stop:
				return
			case <-t.C:
				if err := r.register(); err != nil {
					log.Printf("registrar: heartbeat failed: %v", err)
				}
			}
		}
	}()
}

// Stop halts the heartbeat and deregisters the node (DEL) so a graceful shutdown leaves the fleet
// at once rather than lingering until the TTL. Best-effort on the DEL (the process is exiting).
func (r *Registrar) Stop() {
	close(r.stop)
	<-r.done
	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()
	if err := r.rdb.Del(ctx, nodeKey(r.self.ID)).Err(); err != nil {
		log.Printf("registrar: deregister failed: %v", err)
	}
	_ = r.rdb.Close()
}

// register writes the node's NodeInfo to its key with the TTL (SET … EX ttl), self-reporting its
// current drain status. It reads the api's drain command for this node first (Stage 25) so entering
// or leaving drain flows api -> Redis command -> our heartbeat -> the api's discovery/reconcile,
// which is what makes the orchestrator authoritative for its own status. A drain-read error leaves
// the last reported status unchanged (a transient blip must not flip drain state).
func (r *Registrar) register() error {
	ctx, cancel := context.WithTimeout(context.Background(), redisOpTimeout)
	defer cancel()
	if draining, err := isDraining(ctx, r.rdb, r.self.ID); err != nil {
		log.Printf("registrar: drain-status read failed: %v", err)
	} else if draining {
		r.self.Status = StatusDraining
	} else {
		r.self.Status = StatusActive
	}
	blob, err := json.Marshal(r.self)
	if err != nil {
		return err
	}
	return r.rdb.Set(ctx, nodeKey(r.self.ID), blob, r.ttl).Err()
}
