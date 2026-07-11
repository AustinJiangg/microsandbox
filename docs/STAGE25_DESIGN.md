# Stage 25 — Graceful drain (node lifecycle: active / draining)

> Part of the post–Stage 24 production-fidelity plan (`docs/POST_STAGE24_PLAN.md`). Gives a node
> a lifecycle beyond "reachable / unreachable": a node can be **draining** — alive and still
> serving its existing sandboxes, but excluded from **new** placements. This is the foundation
> Stage 26 (rebalancing) builds on. **api-side + orchestrator-startup only** — no data-path /
> `envd` / rootfs change.

## 1. What E2B actually does (verified against `e2b-dev/infra` @ main)

Read `packages/api/internal/orchestrator/{nodemanager/status.go,nodemanager/node.go,nodemanager/sync.go,placement/placement_best_of_K.go,admin.go}`.

- **Node status is an enum, self-reported by the orchestrator.** `api.NodeStatus` ∈
  {`Ready`, `Draining`, `Unhealthy`, `Standby`} (+ `Connecting`, *derived* — see below). The api
  learns each node's status by polling the orchestrator's `ServiceInfo` RPC
  (`nodemanager/sync.go` → `nodeInfo.GetServiceStatus()`), i.e. status originates **at the node**,
  not from the discovery/membership layer. Discovery (Nomad) only answers "which nodes exist";
  per-node status + metrics come from the `ServiceInfo` poll.

- **`Connecting`/`Unhealthy` are folded in from live connectivity.** `Node.StatusInfo()`
  (`nodemanager/status.go`) returns the stored status, but if it is `Ready` and the gRPC conn is
  `Shutdown` → `Unhealthy`, or `TransientFailure`/`Connecting` → `Connecting`. So the effective
  status combines *self-reported intent* (Ready/Draining/Standby) with *live reachability*.

- **Placement picks only `Ready` nodes.** The single load-bearing line, in
  `placement/placement_best_of_K.go` `sample()`:
  ```go
  // If the node is not ready, skip it
  if n.Status() != api.NodeStatusReady { continue }
  ```
  A `Draining` node therefore keeps running its sandboxes (Delete/List still route to it) but is
  never chosen for a new one. This is *exactly* the drain semantics we want.

- **Drain is api-initiated as an override.** `nodemanager/status.go` `SendStatusChange` →
  `client.Info.ServiceStatusOverride(ctx, &ServiceStatusChangeRequest{ServiceStatus: …})`: the api
  pushes a status override to the orchestrator, which then self-reports the overridden status back
  through the normal `ServiceInfo` poll. An operator drains a node **through the api**, and the
  orchestrator is the authoritative holder (so it survives an api restart and is seen by every api).

## 2. Our adaptation (the same model, mapped onto our stack)

Our stack differs from E2B in one relevant way: we have **no `orchestrator-info` / `ServiceInfo`
service**. Our two node signals are (a) the `SandboxService.List` RPC — reachability + sandbox
count — polled by `Node.refresh()` (`placement.go`), and (b) the Stage-24 **Redis registrar**: the
orchestrator heartbeats its `NodeInfo` JSON into `msb:node:<id>`, which `RedisDiscovery` reads.

The registrar heartbeat **is** our faithful analogue of E2B's `ServiceInfo` self-report: it is the
orchestrator advertising its own state on a timer. So:

- **Decision D1 — status is a self-reported field carried in `NodeInfo`.** Add `Status`
  (`"active"`/`"draining"`, empty = active) to `placement.NodeInfo`. `Registrar` already marshals
  `NodeInfo` to JSON, so this is a **backward-compatible add** (an old key deserializes to empty =
  active). `RedisDiscovery.ListNodes` decodes it for free; `reconcile` syncs it onto the live node.
  This mirrors E2B (status originates at the node, flows to the api), differing only in the channel
  (Redis heartbeat vs a gRPC `ServiceInfo` poll) — consistent with how Stage 24 already models
  discovery over Redis rather than Nomad.

- **Decision D2 — eligibility = `Ready() && !Draining()`.** We keep the existing `!n.Ready()`
  reachability skip in `BestOfK.sample` (our analogue of E2B's `Unhealthy`/`Connecting` derivation
  from the gRPC state) and add a `Draining()` skip beside it. Net: a node is picked iff it is
  reachable **and** not draining — the exact reduction of E2B's `Status() == NodeStatusReady`.

- **Decision D3 — drain is api-initiated, orchestrator-authoritative.** `POST /nodes/{id}/drain`
  on the api → the api tells the orchestrator to enter drain (mirroring `ServiceStatusOverride`) →
  the orchestrator flips its self-reported status → the next registrar heartbeat carries
  `draining` → `reconcile` propagates it → `BestOfK` skips it. The orchestrator holds the truth, so
  the drain survives an api restart and is seen by every api — same property as E2B.

- **Decision D4 — `Standby` is out of scope.** E2B's `Standby` (a pre-warmed node not yet taking
  load) has no analogue in our fleet model; we model only `active`/`draining`. `Unhealthy` is
  already covered by our `ready=false` reachability path. Recording this so the omission is a
  decision, not a gap.

- **Decision D5 — static-discovery mode: drain is a no-op channel.** In `--node-discovery static`
  (the `--nodes` flag, the Stage-23 fixed fleet) there is no registrar heartbeat to carry a status
  change, so a node's status stays `active`. This is acceptable and honest: drain is inherently a
  *dynamic-fleet* operation, and static mode is explicitly the "fixed fleet, behaves like Stage 23"
  path. `POST /nodes/{id}/drain` in static mode returns a clear error rather than silently
  no-op'ing. The drain e2e runs under `--node-discovery redis`.

## 3. Sub-steps

- **25a — placement-layer status (pure, unit-tested, no orchestrator/proto change).**
  - `placement.NodeInfo` gains `Status string` (backward-compatible).
  - `placement.Node` gains a `draining atomic.Bool` + `Draining()`/`setDraining()`; `NewNode`
    starts active. `BestOfK.sample` skips `n.Draining()` beside the existing `!n.Ready()`.
  - `Registry.reconcile` syncs each discovered `NodeInfo.Status` onto its live node (a status
    flip on an already-present node updates it; today reconcile only adds/removes membership).
  - Unit tests: a `draining` node is never returned by `Choose`; flipping it back to active makes
    it eligible again; a reconcile that changes only status (same membership) updates the live node.

- **25b — the drain channel (orchestrator self-reports + api initiates).**
  - Orchestrator: hold a self-reported status; the `Registrar` heartbeats it as `NodeInfo.Status`.
    Enter drain via the api-driven override (D3). Finalize the api→orchestrator channel here
    (candidate: a small gRPC override on the orchestrator, hand-editing the stub as Stage 18 did,
    since `protoc` is absent; or a minimal orchestrator control endpoint) — pick the least-churn
    faithful option at implementation time.
  - api: `POST /nodes/{id}/drain` (auth-gated, not team-scoped, like `handleNodes`); `GET /nodes`
    reports `status`. Static mode returns the D5 error.

- **25c — e2e + docs finalize.** A gated real-VM/dynamic-discovery e2e: bring up two nodes under
  `--node-discovery redis`, drain one, assert (a) new sandboxes avoid it, (b) a sandbox already on
  it keeps working (List/Delete still route there). Finalize this doc's "measured" notes.

## 4. Honest scope

Drain here means **"stop placing new sandboxes here"**, nothing more — existing sandboxes run to
their natural end; **actively evacuating them is Stage 26**. On one box this is **fidelity, not
speed**. `Standby` and a real gRPC `ServiceInfo` service are out of scope (Decisions D4/D5); the
status channel is the Redis registrar, not a separate info RPC — the same single-box substitution
Stage 24 made for discovery.

## 5. Tests

- Go units (`go test ./services/...`, KVM-free): the 25a placement/reconcile cases above, plus the
  existing `placement_test.go` / `discovery_test.go` stay green (the `Status` field defaults keep
  Stage 23/24 behavior identical).
- e2e (gated, `MSB_TEST_DISCOVERY=1`, like Stage 24): the 25c drain scenario.
