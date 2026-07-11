# Stage 24 design — real node discovery: a `Discovery` source + a reconciling registry

> Status: **proposed.** This stage takes the next "production fidelity" item off the deferred list in
> `docs/E2B_ALIGNMENT_ROADMAP.md` §Still-deferred ("real node discovery (Nomad/Consul, or a dynamic
> register/deregister API)"). Stage 23 gave the api a **fleet** and E2B's `placement.BestOfK`, but the fleet
> was **static** — a `--nodes grpc@proxy,…` flag parsed once at startup into a fixed slice that never changes.
> This stage makes the fleet **dynamic**: orchestrators **register themselves** into a shared service registry
> (Redis, heartbeat + TTL), and the api's registry runs a **reconcile loop** that adds newly-appeared nodes and
> removes vanished ones — E2B's `discovery.Discovery` interface + `keepInSync`, specialized to one box.
>
> It builds directly on Stage 23 and changes **only the api's registry and the orchestrator's startup**: no
> proto change, no data-path change, no `envd`/rootfs change. Backward compatible — the default discovery
> backend is `static` (the Stage-23 `--nodes` flag), so dev-up and the e2e fixture are unchanged.

## 1. The problem (what "static fleet" bakes in)

Stage 23 dissolved the api's single-orchestrator assumption into a `placement.Registry` holding a set of
nodes, but the set is fixed at startup and immutable thereafter, in exactly two places:

- `cmd/api/main.go:62-84` — `parseNodeSpecs(--nodes)` → `[]*placement.Node` → `NewRegistry(nodes, k)`, once.
- `pkg/placement/registry.go:12` — `nodes []*Node` is a **fixed-length slice**. The `Poll` loop refreshes each
  node's cached `count` + `ready` (via `List`), but there is **no method to add or remove a node**.

Consequences on a real fleet:

- **A crashed orchestrator never leaves.** Its `refresh()` flips `ready=false` so BestOfK stops *sampling* it,
  but the `*Node` (and its idle gRPC conn) sit in the slice forever. There is no eviction.
- **Adding a machine means restarting the api** with a new `--nodes` flag. There is no runtime join.

That is the definition of "static." E2B does not restart its api to grow the fleet; it discovers nodes.

## 2. What E2B actually does (verified against `e2b-dev/infra` @ main)

`packages/api/internal/orchestrator/`. The node set is behind a **pluggable discovery interface**, and a
background loop **reconciles** the live pool against it.

```go
// orchestrator/discovery/discovery.go
type Node struct {
    ShortID             string
    IPAddress           string
    OrchestratorAddress string   // precomputed "ip:grpcPort"
}
type Discovery interface {
    // ListNodes returns every orchestrator the discovery backend knows about.
    ListNodes(ctx context.Context) ([]Node, error)
}
```

```go
// orchestrator/cache.go — keepInSync, the reconcile loop
const cacheSyncTime = 20 * time.Second
ticker := time.NewTicker(cacheSyncTime)
// each tick:
discovered := o.listNomadNodes(ctx)                 // = discovery.ListNodes
for _, n := range discovered:
    if o.GetNodeByNomadShortID(n.ShortID) == nil:   // discovered but not in the pool
        o.connectToNode(ctx, n)                      //   -> dial gRPC, add to `nodes`
for _, n := range o.nodes:
    if !found(n, discovered):                        // in the pool but no longer discovered
        o.deregisterNode(n)                          //   -> close conn, remove from `nodes`
```

The pool itself is `nodes *smap.Map[*nodemanager.Node]` (a concurrent map, not a slice). Discovery is
**multi-implementation**: `clusters/discovery/{static,local,kubernetes,remote}.go` plus the legacy
`listNomadNodes`. E2B's production source of truth is Nomad's node list; `static.go` (a fixed list from
config) is "used for local development." **The lesson is the seam — a `Discovery` interface + a `keepInSync`
reconcile — not Nomad specifically.**

## 3. The single-machine specialization (a Redis service registry, faithful to the seam)

We reproduce E2B's *two* pieces — a `Discovery` interface and a reconcile loop — and pick a discovery backend
that is a real dynamic registry on one box, not a flag:

**A Redis service registry with heartbeat + TTL** (user pick). This is the canonical single-node analogue of
Consul/Nomad service registration: a service writes its address to the registry under a TTL and heartbeats to
keep it alive; if the service dies, the entry expires and it drops out. We already run a shared Redis (the
Stage-14a catalog), so this adds no infrastructure.

- **Register (orchestrator side).** On startup the orchestrator writes `msb:node:<id> → {"grpc","proxy"}`
  with `SET … EX ttl`, and re-writes it every `heartbeat` (< ttl) so a live node stays present. On graceful
  shutdown it `DEL`s the key. `id` = its gRPC address (its unique advertised identity). `heartbeat = 1s`,
  `ttl = 3s` (≈3× heartbeat: one missed beat doesn't evict, a crash evicts within ~3s). TTL **is** the health
  signal — no separate health RPC, mirroring Consul TTL checks.
- **Discover (api side).** `RedisDiscovery.ListNodes` does a `SCAN MATCH msb:node:*` → `MGET` → decode. A key
  present ⇒ that orchestrator has heartbeated within the TTL ⇒ it is alive. A crashed node's key is simply
  gone.

This maps E2B's `discovery.Node{ShortID, OrchestratorAddress}` onto our `NodeInfo{ID, GRPC, Proxy}` (we carry
`Proxy` too, because the api writes it to the catalog as `Route.Node` — E2B's proxy address is derived
elsewhere).

## 4. Target shape

```
  orchestrator A (--register)          orchestrator B (--register)
    heartbeat: SET msb:node:9090 EX3     heartbeat: SET msb:node:9091 EX3
    shutdown:  DEL msb:node:9090         crash:     (key TTL-expires in ~3s)
            │                                    │
            ▼   Redis service registry  (msb:node:*)   ◄── shared with the Stage-14a catalog
            └──────────────────┬─────────────────┘
                               │  RedisDiscovery.ListNodes()  (SCAN + MGET)
                 ┌─────────────▼──────────────────── api ───────────────────────┐
                 │  placement.Registry                                          │
                 │    discovery  Discovery       ← static (--nodes) | redis     │
                 │    factory    NodeFactory     ← dial gRPC, build *Node       │
                 │    nodes      map[id]*Node    (guarded; was a fixed slice)   │
                 │    ~1s poll:  reconcile() ── add discovered-not-present,      │
                 │                              remove present-not-discovered    │
                 │               refresh()   ── each node's count + ready (List) │
                 │    Pick(excluded) ── BestOfK over the live map  (unchanged)   │
                 └──────────────────────────────────────────────────────────────┘
```

The BestOfK algorithm, `Reserve`/`Release`, failover, `handleCreate`/`handleDestroy`, the catalog, the data
path — **all unchanged from Stage 23**. This stage swaps *where the node set comes from* (a flag → a
`Discovery` source) and makes the set *mutable* (a slice → a reconciled map). Everything that consumes the set
is untouched.

## 5. The pieces (what changes, precisely)

**`pkg/placement` — the discovery seam + a reconciling registry:**

```go
// discovery.go
type NodeInfo struct { ID, GRPC, Proxy string }        // one discovered orchestrator
type Discovery interface { ListNodes(ctx context.Context) ([]NodeInfo, error) }

type StaticDiscovery struct{ nodes []NodeInfo }         // E2B's static.go: a fixed list, forever
func NewStaticDiscovery(nodes []NodeInfo) *StaticDiscovery

// discovery_redis.go
type RedisDiscovery struct{ rdb *redis.Client }         // SCAN msb:node:* -> MGET -> decode
func NewRedisDiscovery(addr string) *RedisDiscovery

// registrar.go — the orchestrator's self-registration (shares the key format with RedisDiscovery)
type Registrar struct{ … }
func NewRegistrar(addr string, self NodeInfo, ttl, beat time.Duration) *Registrar
func (r *Registrar) Start(); func (r *Registrar) Stop()  // heartbeat SET EX; DEL on Stop
```

`Registry` changes:
- `nodes []*Node` → `nodes map[string]*Node` + a `sync.RWMutex`; `byProxy` maintained under the same lock.
- new fields `discovery Discovery`, `factory NodeFactory` (`func(NodeInfo) (*Node, error)` — injected by the
  api because *it* knows how to dial gRPC; keeps `pkg/placement` dial-free and unit-testable with a fake).
- `NewRegistry(discovery, factory, k)` replaces `NewRegistry(nodes, k)`.
- the poll loop calls `reconcile(ctx)` (add discovered-absent via `factory`, remove present-absent and
  `Close()` its conn) **before** `refresh()` (the existing per-node count/ready `List`). `reconcile` is
  reconcile-only on `ListNodes` **success**; a discovery error is logged and the current set kept (a transient
  Redis blip must not wipe the fleet).
- `Node` gains an optional `Close()` (set by the factory to close the gRPC conn) so eviction releases the conn.

> **Divergence noted:** E2B reconciles every `cacheSyncTime = 20s` and polls metrics separately. Our fleet is
> tiny and a `SCAN` over a handful of keys is sub-millisecond, so we fold reconcile into the existing 1s poll
> (one loop, reconcile then refresh). Same shape, one timer.

**`cmd/orchestrator`** — new `--redis-addr` + `--register`: when `--register` is set, build
`NodeInfo{ID: grpcAddr, GRPC: grpcAddr, Proxy: proxyAddr}`, start a `Registrar`, `defer Stop()` (which `DEL`s
the key so a graceful shutdown leaves immediately, not after a TTL).

**`cmd/api`** — new `--node-discovery static|redis` (default `static`). `static` builds a `StaticDiscovery`
from the Stage-23 `--nodes`/legacy flags (**behavior identical to Stage 23**); `redis` builds a
`RedisDiscovery(--redis-addr)`. The node factory dials gRPC (today's `grpc.NewClient` + `pb.NewSandboxServiceClient`)
and returns a `*Node` whose `Close()` closes the conn.

**Optional (recommended) — `GET /nodes`** (auth-gated, admin-ish): return the registry's live node set
(`id, proxy, ready, load`). It is the observable that makes "discovery" demonstrable end-to-end (E2B has an
`admin.go` node listing), and it lets the gated e2e assert a killed node drops out without scraping logs.

## 6. Sub-steps (each independently verifiable, tests green at every step)

- **24a — the `Discovery` seam + a reconciling `Registry` + `StaticDiscovery` (pure, KVM-free).** Add the
  interface, `NodeInfo`, `StaticDiscovery`, the node factory, and refactor `Registry` from a fixed slice to a
  reconciled `map` (reconcile add/remove/close; `Pick`/`NodeByProxy`/`Nodes` read the map under the lock).
  Wire the api to build a `StaticDiscovery` from `--nodes` — the fleet is still the flag's, so the create/destroy
  path is byte-identical to Stage 23. **Unit tests** (mirroring E2B's reconcile): a newly-discovered node is
  added, a vanished node is removed and its conn closed, reconcile is idempotent, a discovery error keeps the
  current set, `StaticDiscovery` returns its fixed list. **Go units green; single-node lifecycle e2e unchanged.**
- **24b — `RedisDiscovery` + orchestrator self-registration + `--node-discovery` (real dynamic path).**
  `RedisDiscovery` (SCAN+MGET+decode), `Registrar` (heartbeat SET EX; DEL on stop), the orchestrator
  `--register`/`--redis-addr` wiring, the api `--node-discovery redis`, and the optional `GET /nodes`. **Unit
  test** the registrar↔discovery round-trip against a live Redis (self-skips without `REDIS_ADDR`, like the
  other Redis-backed tests): register two, `ListNodes` sees two; stop one, after TTL `ListNodes` sees one.
- **24c — gated real-VM dynamic-discovery e2e + docs.** `conftest.py` honors `MSB_TEST_DISCOVERY=1`: start the
  orchestrator with `--register --redis-addr …` and the api with `--node-discovery redis` (no `--nodes`), so
  the api learns the orchestrator **only through Redis**. A `tests/test_discovery.py` case: create a sandbox +
  run code (proves discovery-driven placement boots a real VM), then kill the orchestrator and poll `GET
  /nodes` until it drops out (proves TTL eviction + reconcile). Finalize this doc, `CLAUDE.md`, the roadmap,
  and memory.

## 7. Honest scope (what this is and is not)

- **This is real node discovery on one box — and it is genuinely dynamic, unlike Stage 23's fidelity-only
  framing.** An orchestrator joins by registering, leaves by deregistering (or by dying and TTL-expiring), and
  the api's fleet changes at runtime with **no restart**. That behavior is fully observable on one machine (a
  registrar heartbeating into Redis, a reconcile loop adding/removing), so the gated e2e is a *real* end-to-end
  proof, not a stand-in — the strongest verification a multi-host feature has gotten in this repo.
- **The registry backend is Redis with TTL, not Nomad/Consul.** We reproduce the *service-registry* pattern
  (register, heartbeat, TTL eviction, reconcile) that Consul/Nomad implement, behind E2B's `Discovery`
  interface — not their scheduler. Swapping in a real Nomad/Consul discovery would be one more `Discovery`
  implementation, no registry/api change (that is the point of the seam).
- **Still deferred (noted, not done here):** rebalancing already-placed sandboxes off a newly-joined or
  draining node; per-node build placement (template builds still route to one node); graceful drain (a node
  announcing "no new placements" distinct from "gone"). These are load-management concerns *on top of*
  discovery, not discovery itself.
- **Unchanged, still true:** learning implementation, not security-audited, never safe for untrusted input. The
  service registry is plaintext on loopback like every other seam here; it is the *mechanism*, not a hardened
  control plane.
