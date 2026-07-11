# Stage 26 — Rebalancing (relocate a sandbox off a draining node)

> Part of the post–Stage 24 production-fidelity plan (`docs/POST_STAGE24_PLAN.md`). Stage 25
> gave a node a `draining` state (alive, still serving its sandboxes, excluded from **new**
> placements). Stage 26 answers the follow-on: what happens to the sandboxes **already on** a
> draining node? This stage is **api-side + placement-layer + proto only**, verified **in
> process** — no data-path / `envd` / rootfs change, and (deliberately) **no real-VM live
> snapshot**, which would re-enter the Stage 20/22 Firecracker-consistency saga.

## 1. What E2B actually does (verified against `e2b-dev/infra` @ main)

The plan flagged the core question as *"does E2B migrate running sandboxes, and if so how?"* —
to be resolved against the source **before any code**. It is now resolved. Read
`packages/orchestrator/main.go` (drain phase), `packages/api/internal/orchestrator/nodemanager/sync.go`,
`packages/api/internal/handlers/{sandbox_resume.go,sandbox.go,sandbox_pause.go}`,
`packages/api/internal/orchestrator/create_instance.go`, and
`packages/api/internal/orchestrator/placement/placement.go`.

**Finding: E2B has no server-driven live-migration / rebalancing loop.**

- `grep -rniE "rebalanc|evacuat|migrat"` across `packages/` finds only DB/event migrations —
  **no** sandbox-migration code.
- The api's per-node sync loop (`nodemanager/sync.go`) only **reads** each node's status,
  metrics, and instance list into the store. It never moves a sandbox off a draining node.
- The orchestrator's drain phase (`orchestrator/main.go:617`) marks itself `Draining`, sleeps
  ~15 s so the api stops placing on it, then closes. It does **not** hand its sandboxes off to a
  peer.

**So how does a sandbox leave a node? The pause → resume lifecycle.**

- `Sandbox.Pause` (`orchestrator/internal/sandbox/sandbox.go`) snapshots the live VM's memfile
  diff + rootfs diff to object storage and records the sandbox's `OriginNodeID`. The VM leaves
  the node; its state persists in the bucket.
- On `Resume`, the api **prefers the origin node but drops that affinity when the origin is not
  `Ready`** — the single load-bearing block, in `create_instance.go`:
  ```go
  if isResume && nodeID != nil {
      node = o.GetNode(clusterID, *nodeID)
      if node != nil && node.Status() != api.NodeStatusReady {
          node = nil   // origin draining/unhealthy/gone -> drop the pin, re-place
      }
  }
  node, err = placement.PlaceSandbox(ctx, o.placementAlgorithm, clusterNodes, node /*preferred*/, ...)
  ```
  `PlaceSandbox` (`placement/placement.go`) uses `preferredNode` when non-nil, else runs BestOfK
  over the fleet — which already skips draining nodes (Stage 25). So a paused sandbox whose origin
  node is draining **resumes on a different node**.

**Consequence for Stage 26's shape.** The plan sketched two candidates: **A — orchestrator-to-
orchestrator live hand-off** and **B — no live migration**. The source rules out A (E2B does *more*
than nothing but *less* than a live hand-off) and shows the faithful answer is a **third** thing:
**relocation via pause→resume with drain-aware resume affinity.** There is no evacuation loop to
copy; the fidelity target is the **resume-time placement**.

## 2. Our adaptation (the same mechanism, mapped onto our stack)

Our stack reaches this mechanism through the seams earlier stages already built:

- **The relocation decision is the resume-time placement** — an api-side scheduling choice,
  exactly the class of logic Stages 23–25 built in `pkg/placement` and verified with the
  in-process fake-orchestrator harness (`cmd/api/placement_integration_test.go`). Running two real
  orchestrators on one box is **not an E2B concept** (Stage 23/24 rationale: each E2B orchestrator
  is a separate machine), so — consistent with those stages — the **multi-node** relocation is
  verified in process, not on two real VMs.

- **A real per-sandbox live pause is the fragile part, and it is out of scope.** The only way to
  snapshot a *running* VM is `fc.MicroVM.Snapshot` (`pkg/fc/fc.go:505`) — the Stage 20/22
  live-VM re-snapshot path, consistent only under `v1.10.1` + `--nbd` + a writable overlay, and
  used so far **only for build-time template snapshots**, never per running sandbox. Wiring it
  per-sandbox is a genuine re-entry into that saga. Stage 26 therefore delivers the **relocation
  control plane** and proves it **in process**; the real-VM per-sandbox snapshot mechanic reuses
  the already-built Stage 20/22 producer and is **deferred** (Decision D4).

### Decisions

- **D1 — Relocation = pause→resume, not an evacuation loop.** Faithful to E2B (which has no
  rebalance loop). Stage 26 adds the *resume-time relocation*, not a background evacuator. Drain
  (Stage 25) already stops new placements; Stage 26 makes a paused sandbox whose origin node is
  draining come back **elsewhere**.

- **D2 — Resume affinity with drain-aware fallback (the load-bearing bit).** A new placement
  primitive `Registry.PickPreferred(preferred *Node, excluded)`: honor `preferred` iff it is
  **eligible** (`Ready() && !Draining()`), else fall back to `BestOfK.Choose` (which already
  excludes draining/not-ready nodes). This is the exact reduction of E2B's `create_instance.go`
  affinity drop (`node = nil` when `Status() != Ready`) composed with `PlaceSandbox(preferredNode)`.
  Pure, unit-testable, no VM.

- **D3 — A paused sandbox's relocation state lives in the store + catalog.** A paused sandbox is
  on **no** node, so the api must persist enough to resume it: its **origin node** (the data-proxy
  address it last held, so resume can prefer it) and a `paused` status. `pkg/store` gains an
  `origin_node` column (idempotent ALTER, like the Stage-16 `team_id` migration) and `paused`/
  `running` statuses. The **catalog route is dropped on pause** (the sandbox is unreachable while
  paused) and **rewritten to the new node on resume** (`catalog.Set(id, Route{Node: target, …})` —
  the same call create makes; `deleteOnHoldingNode` already routes by `Route.Node`, so the whole
  data path follows the rewrite for free).

- **D4 — Proto gains `Pause`/`Resume`; the real-VM mechanic is deferred.** `SandboxService` gains
  `Pause(SandboxPauseRequest{sandbox_id})` and `Resume(SandboxResumeRequest{sandbox_id, config})`
  (clean `scripts/gen-proto.sh` regen — the plugins are present in `$(go env GOPATH)/bin`; the
  proto already anticipates *"E2B's real service also has Pause / Resume … those need runtime
  checkpointing of a running VM, so they arrive in a later stage"*). The **fake orchestrator**
  implements them as sandbox-list moves — all the in-process relocation test needs. The **real
  orchestrator** implements them as **honest `Unimplemented`** stubs whose message points at the
  Stage 20/22 producer as the reuse path: a real per-sandbox live snapshot is that fragile
  machinery, and the chosen scope for this stage is *"no FC-saga risk, verify in process."* Wiring
  the real snapshot per-sandbox is a clean follow-on that reuses `fc.MicroVM.Snapshot` +
  `storage.PublishMemfileDiff`/`PublishRootfsDiff` behind these same RPCs — recorded here so the
  omission is a decision, not a gap. (Alternative considered: wire the real snapshot now. Rejected
  because it re-enters the exact Firecracker-version edges Stage 22 spent the most effort on, with
  no single-box way to verify it beyond what Stage 20/22 already cover.)

- **D5 — Verification is the in-process relocation scenario (the plan's stated Verify).** Extend
  `placement_integration_test.go`: create a sandbox on node A → `pause` (state leaves A, route
  dropped, origin recorded) → **drain A** → `resume` → BestOfK lands it on node B (A ineligible) →
  the catalog route is rewritten to B → a `catalog.Get` resolves the data path to B. Plus
  `pkg/placement` unit tests for `PickPreferred` (honored when active; dropped when draining/not
  ready; falls through to BestOfK).

- **D6 — Honest scope.** Single-box: **fidelity, not speed.** No server-driven evacuation loop
  (E2B has none). No real cross-node live move (not a single-box E2B concept). The real per-sandbox
  live snapshot inherits the Stage 20/22 Firecracker constraints and is deferred (D4). What Stage
  26 *does* deliver, faithfully and tested: the **resume-time drain-aware relocation** — the actual
  mechanism by which E2B moves a sandbox off a draining node.

## 3. Sub-steps

- **26a — the placement affinity primitive (pure, unit-tested, no proto/orchestrator change).**
  - `BestOfK` / `Registry` gain `PickPreferred(preferred *Node, excluded map[string]struct{})`:
    return `preferred` iff `preferred != nil && preferred.Ready() && !preferred.Draining()`, else
    `Choose(snapshot, excluded)`. (Mirrors `PlaceSandbox`'s preferred-node branch.)
  - Unit tests in `pkg/placement`: preferred honored when active; preferred **dropped** when it is
    draining or not-ready, landing on an eligible node; nil preferred behaves like `Pick`.

- **26b — the relocation control plane (proto + api handlers + store + catalog) + the in-process
  relocation test.**
  - Proto: add `Pause`/`Resume` to `SandboxService`; `scripts/gen-proto.sh`.
  - Orchestrator server (`cmd/orchestrator/server.go`): `Pause`/`Resume` — real orchestrator returns
    `Unimplemented` (D4, message → Stage 20/22 reuse path). Fake orchestrator
    (`placement_integration_test.go`): list moves.
  - `pkg/store`: `origin_node` column (idempotent migration) + `PauseSandbox(id, originNode)` /
    `ResumeSandbox(id, node)` / a `PausedSandbox(id) (origin, ok, err)` lookup; `status` becomes
    `running`/`paused`.
  - api: `POST /sandboxes/{id}/pause` (team-scoped ownership check like destroy → route to holding
    node's `Pause` → record origin + `paused` in store → drop catalog route → 202) and
    `POST /sandboxes/{id}/resume` (ownership check → read origin → `PickPreferred(originNode)` →
    target node `Resume` → rewrite catalog route + `running` → 200 with `data_url`+`token`).
  - Integration test: the D5 scenario end to end on fake orchestrators.

- **26c — docs finalize + honest self-review.** Update `CLAUDE.md` (a "Done (Stage 26)" bullet) and
  `docs/POST_STAGE24_PLAN.md` (mark Stage 26 done). No SDK pause/resume surface this stage: a
  user-facing pause button would imply the real-VM mechanic works, which D4 defers — so we do not
  advertise it. The Python e2e suite is unchanged (no real-VM path added); `go test ./services/...`
  carries the new coverage.

## 4. Honest scope

Stage 26 delivers the **api-side relocation scheduling** that is E2B's actual mechanism for moving
a sandbox off a draining node — resume-time affinity with a drain-aware fallback — and proves it in
process (the plan's stated Verify). It does **not** add a background evacuation loop (E2B has none),
a real cross-node live migration (not a single-box concept), or a real per-sandbox live snapshot
(that is the Stage 20/22 fragile path, deferred behind the new `Pause`/`Resume` RPCs). On one box
this is **fidelity, not speed**. Still a learning implementation, **not security-audited** — the
safety rule is unchanged.

## 5. Tests

- Go units (`go test ./services/...`, KVM-free): the 26a `PickPreferred` cases; the existing
  `placement_test.go` / `bestofk` / `discovery` / `drain` tests stay green (the primitive is
  additive). Store: an `origin_node` round-trip on both SQLite and Postgres (the PG variant
  self-skips without `MSB_TEST_PG_DSN`, like the rest).
- In-process integration (`cmd/api`, real gRPC over localhost, no KVM): the D5 relocation scenario
  — create → pause → drain origin → resume-elsewhere → catalog rewritten → data path resolves to
  the new node.
- No new Python e2e: no real-VM path is added this stage (D4/D6). The static-default `test_microvm`
  and the discovery/drain e2e stay as they are.
</content>
</invoke>
