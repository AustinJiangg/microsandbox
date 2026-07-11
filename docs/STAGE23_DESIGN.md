# Stage 23 design — multi-host scheduling: a node registry + `placement.BestOfK`

> Status: **design.** This stage takes the first "production fidelity across hosts" item off the deferred
> list in `docs/E2B_ALIGNMENT_ROADMAP.md` §5 / §Still-deferred: the api stops assuming **one** orchestrator
> and instead holds a **set** of nodes and picks one per create with E2B's **power-of-K-choices** placement
> (`placement.BestOfK`). Everything the multi-host data path needs was already built — the routing catalog
> has been **per-sandbox `Route{Node}`** since Stage 14a, so client-proxy already routes each sandbox to
> whichever node holds it. This stage is therefore **api-side only**: node registry + placement + failover.
> No proto change, no data-path change, no `envd`/rootfs change.

## 1. The problem (what "one orchestrator" is baked into)

Today the api is hard-wired to a single orchestrator, in exactly three places:

- `cmd/api/main.go:47-48` — two flags, `--orchestrator-grpc` (one gRPC address) and `--orchestrator-proxy`
  (one data-proxy address = the `Node` value written to the catalog).
- `cmd/api/main.go:76-82` — the `api` struct holds **one** `pb.SandboxServiceClient` (`a.client`) and **one**
  `a.nodeAddr` string.
- `cmd/api/handlers.go:35,65` — `handleCreate` calls `a.client.Create` (that one node) and writes
  `catalog.Route{Node: a.nodeAddr}` (that one node). `handleDestroy` calls `a.client.Delete` (that one node).

Everything downstream is already multi-host-shaped:

- `pkg/catalog` stores a **per-sandbox** `Route{Node, Token}` (Redis, Stage 14a). client-proxy reads it on
  every data request and reverse-proxies to `Route.Node`. Two sandboxes on two different nodes already route
  correctly today — the api just never writes two different `Node` values.
- The metadata store, the access token, the rollback-on-failure — all per-sandbox, node-agnostic.

So the whole change is: give the api a **list** of nodes, choose one per create, call **that** node's gRPC and
write **that** node's proxy address to the catalog.

## 2. What E2B actually does (verified against `e2b-dev/infra` @ main)

`packages/api/internal/orchestrator/placement/placement_best_of_K.go`. The algorithm is **power of K
choices**: don't scan the whole fleet, sample a few and take the best — cheap, and it avoids the "thundering
herd onto the single least-loaded node" that a global argmin causes under concurrency.

```go
// DefaultBestOfKConfig: R=4 (over-commit ratio), K=3 (candidates sampled), Alpha=0.5 (live-usage weight)
func (b *BestOfK) Score(node, resources, config) float64 {
    reserved   := node.Metrics().CpuAllocated + node.PlacementMetrics.InProgress()  // allocated + in-flight
    usageAvg   := node.Metrics().CpuPercent / 100
    totalCap   := config.R * node.Metrics().CpuCount
    return (resources.CPUs + reserved + config.Alpha*usageAvg) / totalCap           // lower = emptier = better
}
// chooseNode: sample() up to K nodes uniformly at random, skipping excluded / not-Ready / CPU-incompatible;
//             Score each; return the min. nil candidates -> FailedToPlaceSandboxError.
```

Node fields it reads: `Metrics()` (`CpuAllocated`, `CpuPercent`, `CpuCount` — cached, refreshed by a
background poll of the orchestrator), and `PlacementMetrics.InProgress()` (CPUs of placements that have been
chosen but whose metrics the node hasn't reported back yet), and `Status()` (skip non-`Ready`). E2B discovers
the node set from **Nomad**; a background loop keeps each node's cached metrics fresh.

## 3. The single-machine specialization (faithful reduction, no proto change)

Two properties of this repo collapse E2B's CPU-weighted score into something we can compute with the RPC we
**already have**:

1. **Homogeneous sandboxes.** Every sandbox is 1 vCPU / 512 MiB (`grpc.go:35-36` ignores `cfg.Vcpu/MemMb`).
   So "CPU allocated on a node" ≡ "number of sandboxes on that node". E2B's `CpuAllocated` **is** the sandbox
   count here.
2. **`SandboxService.List` already reports it.** `grpc.go:51` / `server.go:186` returns the ids of the
   sandboxes a node currently holds. `len(List())` is the node's load — no metrics RPC, no proto edit
   (`protoc-gen-go` isn't installed; proto stubs are hand-maintained — avoiding a proto change is worth real
   points).

So E2B's `Score` specializes to:

```
score(node) = (inProgress + cachedCount) / capacity
```

- `cachedCount` = `len(node.List())`, refreshed by a background poll (~1 s) — this is `node.Metrics()`.
- `inProgress` = a per-node counter, +1 when this node is chosen for a create, −1 once that create finishes
  (catalog write done, or rolled back) — this is `PlacementMetrics.InProgress()`. It closes the window
  between "chosen" and "the next poll sees it", so a burst of concurrent creates spreads instead of all
  landing on the node that looked emptiest at t=0.
- `capacity` = a per-node bound (default `networkSlots` = 256, `--node-capacity`), standing in for E2B's
  `R * CpuCount`. We keep no live-CPU% telemetry (that *would* need a new RPC), so the `Alpha*usage` term is
  dropped (equivalently Alpha = 0). This is a faithful reduction, not a different algorithm: same sampling,
  same "lowest normalized load wins", same in-progress correction.

**Decision D1 (load metric): cached `List` poll + in-progress counter**, mirroring BestOfK's two `Score`
inputs (`Metrics()` + `InProgress()`), rather than a live `List` per placement. Chosen for mechanism fidelity
(user pick). The background poller also doubles as the **readiness probe**: a node whose `List` errors is
marked not-Ready and dropped from sampling until it answers again.

## 4. Node discovery: a static `--nodes` list (Nomad stays deferred)

E2B discovers nodes from Nomad's node list; the roadmap deferred Nomad/Consul from the start ("single
machine; api holds one orchestrator address by flag"). The honest single-box analogue of "the fleet the api
knows about" is a **static list**, supplied by flag:

```
--nodes '127.0.0.1:9090@127.0.0.1:5007,127.0.0.1:9091@127.0.0.1:5017'   # grpc@proxy , repeatable/CSV
```

Each entry is `grpcAddr@proxyAddr`: the gRPC address the api calls `Create/Delete` on, and the data-proxy
address it writes to the catalog as `Route.Node`. **Backward compatibility:** if `--nodes` is empty, the api
synthesizes a one-node list from the existing `--orchestrator-grpc` / `--orchestrator-proxy` flags — so every
current invocation (and the e2e fixture) keeps working unchanged, as a one-node cluster.

## 5. Target shape

```
                 ┌─────────────────────────── api ───────────────────────────┐
  POST /sandboxes│  placement.Registry                                        │
  ──────────────▶│    nodes: [ Node{grpc, proxy, cachedCount, inProgress,     │
                 │             ready}, … ]   ← background poll List() ~1s      │
                 │    Pick() ──BestOfK(sample K, score, min)──▶ Node          │
                 │  handleCreate:                                             │
                 │    node := reg.Pick(excluded)      // choose               │
                 │    node.Reserve()                  // inProgress++         │
                 │    node.GRPC.Create(...)           // that node's fleet    │
                 │      └─ err? reg.Exclude(node); retry Pick (E2B excludedNodes)
                 │    catalog.Set(id, Route{Node: node.Proxy, Token})         │
                 │    node.Release()                  // inProgress--         │
                 └────────────────────────────────────────────────────────────┘
                        │ gRPC Create/Delete                    ▲ Redis catalog per-sandbox Route{Node}
                        ▼                                        │
                 orchestrator A (:9090 / :5007)   orchestrator B (:9091 / :5017)   … (each a node)
                                                            ▲
  data path:  SDK ─▶ client-proxy ─(catalog Route.Node)─────┘  (already multi-host since Stage 14a)
```

`handleDestroy` must reach the node that **holds** the sandbox. The catalog already records it as
`Route.Node` (the proxy address); the registry keeps a `proxy → Node` index, so destroy resolves
`catalog.Get(id).Node → registry.NodeByProxy → node.GRPC.Delete`. No new store column.

Template builds (`TemplateService`) route to **one designated node** (node[0]) for now — artifacts land in
shared object storage keyed by build id (Stage 15), so any node restores from any build regardless of who
built it. Per-node build placement is a noted, deliberate non-goal for this stage.

## 6. Sub-steps (each independently verifiable, tests green at every step)

- **23a — `pkg/placement` (pure, KVM-free).** `Node` (gRPC client + proxy addr + capacity + cached count +
  in-progress counter + ready flag; `Score`, `Reserve`/`Release`), `BestOfK` (`Config{K, Capacity}`,
  `sample`, `Choose(excluded) (*Node, error)`), `Registry` (holds the nodes, `Pick`/`Exclude`/`NodeByProxy`,
  a `Poll` loop refreshing count+readiness via `List`, `Start`/`Stop`). Unit tests mirror E2B's
  `placement_best_of_K_test.go`: min-score selection, K-sampling bound, excluded/not-ready skipped,
  in-progress shifts the choice. **No api wiring yet.**
- **23b — wire the api to the registry (`--nodes`, backward-compatible at 1 node).** Replace `a.client` /
  `a.nodeAddr` with `a.registry`; `handleCreate`/`handleDestroy` route through it (single node = a Pick that
  always returns node[0], catalog gets node[0].Proxy). **Verify: the Python e2e is green unchanged** (the
  fixture runs one orchestrator → a one-node cluster; behavior is byte-identical to today).
- **23c — multi-node behavior: failover + in-progress + readiness, with an in-process integration test.**
  `handleCreate` excludes a node whose `Create` errors and retries another; `Reserve`/`Release` wrap the
  placement; the poller marks unreachable nodes not-Ready. **Go integration test** stands up N in-process
  fake `SandboxService` gRPC servers (real localhost listeners, a counter each, no VMs), builds a registry
  over them, fires M concurrent creates through the placement path, and asserts (a) creates spread across
  nodes within tolerance, (b) an erroring node is excluded and the create lands elsewhere. Deterministic,
  no KVM.
- **23d — docs + honest self-review.** Finalize this doc, update `CLAUDE.md` + the roadmap + memory.

## 7. Honest scope (what this is and is not)

- **This is the placement *seam* and E2B's *algorithm*, on one box.** The api genuinely holds a fleet and
  spreads load with real power-of-K-choices; failover on node error is real. On one physical machine it is
  **fidelity, not speed** — the same honest framing as Stages 14/15/17 (a cross-process catalog, a real
  store, compaction: mechanism, not a single-box win).
- **Node discovery is a static flag, not Nomad/Consul.** Registering/deregistering a node at runtime, health
  scoring beyond reachable/not, and per-node build placement are out of scope (noted).
- **Two *real* orchestrators on one box is deliberately not the verification** (user pick). Running two real
  VM fleets on one machine fights single-box shared resources (`pkg/network` derives netns/veth/IP names from
  a global slot index; the NBD device pool; the baked rootfs/vmstate paths) — contention that E2B never has
  because each orchestrator is a separate machine. Emulating that with slot/device partitioning would test
  *our* single-box plumbing, not E2B's scheduling. The in-process fake-orchestrator integration test
  exercises the placement algorithm — the actual E2B lesson — identically and deterministically. A real
  two-fleet e2e is a possible later, gated extra.
- **Unchanged, still true:** learning implementation, not security-audited, never safe for untrusted input.
