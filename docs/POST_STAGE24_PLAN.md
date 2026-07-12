# Post–Stage 24 plan: finishing production fidelity (Stages 25–31)

> Status: **agreed direction.** This is the forward plan for the "production fidelity"
> remainder called out at the end of `docs/E2B_ALIGNMENT_ROADMAP.md` and the "Possible next"
> bullet in `CLAUDE.md`. Through Stage 24 the multi-host story has its **placement core**
> (`placement.BestOfK`, Stage 23) and **dynamic node discovery** (a Redis service registry behind
> the `Discovery` seam, Stage 24). This document maps the rest into independently shippable stages.
> Per-stage detail lands in its own `docs/STAGE<N>_DESIGN.md` as each is picked up; this file is
> the map.
>
> Grounded against the real code as of Stage 24: `services/pkg/placement` (`placement.go`,
> `registry.go`, `discovery.go`, `discovery_redis.go`, `registrar.go`, `bestofk.go`),
> `services/cmd/api` (`handlers.go`, `templates.go`, `nodes.go`, `main.go`), and
> `services/pkg/{storage,storage/header,store,catalog,uffd,nbd,block}`.

## 1. Scope (what's in, what's out)

| # | Item (from the roadmap remainder) | Decision | Stage |
|---|---|---|---|
| 1 | Rebalancing already-placed sandboxes | **do** | 26 |
| 2 | Graceful drain (a node saying "no new placements" vs "gone") | **do** | 25 |
| 3 | Per-node build placement (builds still route to one designated node) | **do** | 27 |
| 4 | A real Nomad/Consul `Discovery` impl | **do** | 28 |
| 5a | A TypeScript SDK | **never** (explicitly dropped) | — |
| 5b | A cross-node chunk cache | **do** | 30 |
| 5c | Auth depth (key-management API, token expiry/rotation, TLS) | **do** | 31 |
| 6 | memfile/rootfs **compression** (V4/V5 headers, zstd/lz4, 2 MiB frames) | **do, as an opt-in** (matching E2B, where it is optional; raw stays default) | 29 |

Everything here stays a **learning implementation, not security-audited** — the safety rule
(never safe to expose to untrusted input) is unchanged and, since Stage 12's networking,
matters more, not less.

## 2. Dependency graph (why this order)

```
Scheduling track (api-side for the most part):
  25 Drain ──► 26 Rebalance        (must be able to MARK a node "draining" before evacuating it)
  25 Drain ──► 27 Build placement  (a draining node shouldn't receive builds either)
  25 Drain ──► 28 Nomad/Consul     (a new Discovery impl must also carry the drain status)

Storage track (independent of the scheduling track; can run in parallel):
  29 Compression ──► 30 Chunk cache  (a cache of *compressed* frames is smaller, so compress first)

Auth track (fully independent; insertable anytime):
  31 key-management API / token lifecycle / TLS
```

The three tracks are mutually independent. Within the scheduling track, **Stage 25 is the
foundation** for 26/27/28. Suggested global order is 25→31 linear, but the storage and auth
tracks can be reordered ahead of the harder scheduling work if we want low-risk wins first
(31 and 29 are the most self-contained).

## 3. A recurring discipline for every stage below

Each stage carries a 🔎 **"resolve against `e2b-dev/infra` @ main at design time"** marker on the
fidelity decisions it cannot settle by looking at our own code. Per the project's standing rule
(*prefer the E2B-faithful branch when unsure, and decide "which is more E2B-like" by reading
`e2b-dev/infra`, never by guessing*), those markers are **not** pre-answered here — they are read
when that stage's design doc is written. This mirrors how Stages 20/22/23/24 were designed.

---

# Scheduling track

## Stage 25 — Graceful drain (node lifecycle: active / draining) — DONE

> Shipped: design + 25a/25b/25c. See `docs/STAGE25_DESIGN.md` and the CLAUDE.md "Done (Stage 25)"
> entry. Real-VM drain e2e 1/1 + discovery 2/2 under `--node-discovery redis`; static-default
> `test_microvm` 6/6 unregressed. The drain override is Redis-mediated (finalized D3), not E2B's
> gRPC wire.

**Problem.** In `pkg/placement` a node has only two operational dimensions today: `ready`
(does its `List` RPC answer — `Node.refresh` in `placement.go`) and `Load` (the BestOfK score).
There is no *"I'm alive, but stop placing new sandboxes on me"* state. E2B has one (Nomad drain /
an orchestrator node status).

**Changes (real symbols).**
- `placement.NodeInfo` (`discovery.go`) gains a `Status` field (`active`/`draining`). Because
  `Registrar.register` (`registrar.go`) marshals the whole `NodeInfo` to JSON into `msb:node:<id>`,
  **adding a field is backward-compatible** — an old key deserializes to an empty status = active.
- `placement.Node` gains a drain flag; `BestOfK.Choose` (`bestofk.go`) skips draining nodes when
  sampling — a peer of the existing `!Ready()` skip. `Registry.reconcile` (`registry.go`) syncs the
  discovered `Status` onto the live node each poll.
- Orchestrator side: a way to enter drain (a local signal / small endpoint) that flips what
  `Registrar` heartbeats to `Status:"draining"`.
- api side: `handleNodes` (`handlers.go`, already exists) reports `status` on `GET /nodes`; add
  `POST /nodes/{id}/drain` (auth-gated, not team-scoped, like `handleNodes`).

**🔎 Resolve against `e2b-dev/infra`.** E2B's node status machine (how many states —
`draining`/`unhealthy`/`terminating`?), and whether drain is api-initiated or self-reported. Read
`packages/api/internal/orchestrator`.

**Verify.** `placement` unit test — a draining node isn't chosen by `Choose` but is still reachable
for `deleteOnHoldingNode`. e2e — mark a node draining, a new sandbox lands elsewhere, existing
sandboxes keep running.

**Honest scope.** Drain only means "no new placements"; existing sandboxes run to natural
end — **actively evacuating them is Stage 26.** Single-box: fidelity, not speed.

## Stage 26 — Rebalancing (relocate a sandbox off a draining node) — DONE

> Shipped: design + 26a/26b/26c. See `docs/STAGE26_DESIGN.md` and the CLAUDE.md "Done (Stage 26)"
> entry. **The source read overturned the plan's "leaning A":** E2B has no server-driven migration
> loop; a sandbox relocates only through **pause → resume with drain-aware resume affinity**
> (`create_instance.go` drops the origin pin when the origin is not `Ready`). So Stage 26 built that
> — `placement.PickPreferred` + api `POST /sandboxes/{id}/pause`+`/resume` + a catalog route rewrite
> — verified **in process** (the fake-orchestrator harness): a sandbox created on A, paused, then
> resumed after A drains comes back on B. The real per-sandbox live pause reuses the Stage 20/22
> producer and is **deferred** (the real orchestrator's `Pause`/`Resume` return `Unimplemented`);
> "no FC-saga risk, fidelity not speed."

The hardest stage here, and the one that **most needs the source read first**, because what moves is
a *live microVM*.

**🔎 Core open question — resolve against `e2b-dev/infra` before any code.** Does E2B actually
**migrate running sandboxes**, and if so how? Two candidate approaches, very different to build:

- **A — snapshot-migrate (high fidelity, reuses existing machinery).** On the source node
  `Pause + Snapshot` (`fc.MicroVM.Snapshot` already exists, used by Stages 20/22) → the artifacts
  already live in object storage under `{buildID}/…` → `Restore` on the target node (via the
  existing `MaterializeLayered` / NBD / UFFD paths) → rewrite the catalog's `Route{Node}`
  (`deleteOnHoldingNode` already routes by `Route.Node`→`NodeByProxy`, so the pattern is in place).
  **Every cross-node building block already exists**; rebalance is mostly wiring them plus an
  orchestrator-to-orchestrator hand-off.
- **B — no live migration.** Drain only stops new placements; running sandboxes either expire
  naturally or are terminated for the client to recreate. Much simpler, lower fidelity.

Leaning **A** (it is nearly free reuse of the Stage 20/22 + object-storage + catalog groundwork),
**but E2B's source decides** — if E2B does not migrate live sandboxes, neither do we.

**Changes (approach A).** An orchestrator "export snapshot to peer" capability; an api-side
rebalance decision (which sandboxes, to where — reuse `BestOfK`) that rewrites
`catalog.Set(id, Route{Node: target, …})`.

**Verify.** Multi-node integration test (reuse the in-process fake-orchestrator harness from
`cmd/api/placement_integration_test.go`) — mark a node draining, assert its sandboxes move, the
catalog route updates, the data path still resolves.

**Honest scope.** Single-box: fidelity, not speed. How far "live-sandbox migration" is consistent
depends on the Stage 20/22 producer's snapshot-consistency edges (the Firecracker-version saga in
`STAGE22_DESIGN.md`).

## Stage 27 — Per-node build placement

**Current state (verified).** The api routes template builds to a **single**
`a.templates pbt.TemplateServiceClient` (`cmd/api/main.go`, used in `templates.go`) — this is the
"builds still route to one designated node" note in `CLAUDE.md`. Build-status polling hits that same
client.

**Changes.**
- Turn `a.templates` from one client into "pick a build node from the fleet" —
  `handleTemplateCreate` selects via the registry (possibly a build-capacity signal distinct from
  sandbox load) and routes `TemplateCreate` there.
- `handleTemplateBuildStatus` must route to **the node that ran the build** — record build→node in
  `pkg/store` (like a sandbox's `Route`), or broadcast-query (as `deleteOnHoldingNode` falls back to).

**🔎 Resolve against `e2b-dev/infra`.** Whether builds are Nomad jobs / run on a dedicated builder
pool, and what signal picks the build node.

**Verify.** On a multi-node fake, assert builds spread and status polling routes back to the builder.

## Stage 28 — A real Nomad/Consul `Discovery`

The cheapest stage — Stage 24 already carved the `Discovery interface { ListNodes }` seam
(`discovery.go`); Redis and static are just two impls. Adding Consul/Nomad is **one more impl**.

**Changes.** New `placement/discovery_consul.go` (implement `ListNodes` over the Consul service
catalog API); the orchestrator registers into Consul (or Nomad registers it) instead of/alongside
`Registrar`. The api's `--node-discovery` gains a `consul` value; compose gains a consul/nomad
service. If Stage 25 added `Status` to `NodeInfo`, this impl carries it too.

**Honest scope.** Needs a real Consul/Nomad running; the interface swap was designed for in Stage 24,
so the work is the new impl + compose wiring, not a refactor.

---

# Storage track (independent of the scheduling track)

## Stage 29 — Optional compression (V4/V5 header, zstd/lz4, 2 MiB frames)

Compression is **optional** in E2B (V4/V5 header compression, raw V3 still supported); we make it
optional too. It is **orthogonal to COW** — we currently store raw.

**Changes.**
- `pkg/storage/header` (formats v1/v2 today) gains a format with a **compressed-frame table** (per
  2 MiB frame: codec + compressed length).
- `pkg/storage` compresses on publish behind a flag (`--compress zstd|lz4|none`, default `none` for
  backward compatibility); the `uffd` / `nbd` read paths decompress per-frame — the chunked source
  becomes frame-aware.

**Honest scope.** **Opt-in**; raw stays default. Single-box: smaller storage, more CPU — not faster.

## Stage 30 — Cross-node chunk cache

A chunk a node fetched from object storage shouldn't be re-fetched by a peer (E2B has a local/peer
cache).

**Changes.** Wrap the chunked bucket source in a cache layer (local disk cache keyed by
`{buildID}/offset`; then peer fetch). After Stage 29 the cache holds **compressed frames** — smaller.

**Honest scope.** Single-box shows only intra-node cache hits; **cross-node peer fetch** is the
fidelity target.

---

# Auth track (fully independent, insertable anytime)

## Stage 31 — Auth depth (sub-steps a/b/c)

Today (Stage 16): keys are seeded by the `--seed-api-keys` flag, the access token is bearer-only (no
expiry/rotation/signing), everything is plaintext on loopback.

- **31a — key-management API.** `POST/DELETE /api-keys` + team CRUD, replacing flag-seeding. Built on
  the existing `pkg/store` `teams`/`api_keys` tables (keys already stored hashed).
- **31b — token lifecycle.** Per-sandbox access token gains an expiry + a rotation endpoint;
  client-proxy's gate (currently a constant-time compare) also checks expiry.
- **31c — TLS.** TLS on api + client-proxy (self-signed, for learning); optional mTLS on the internal
  gRPC.

**Honest scope.** Still a learning gateway; does **not** change the not-safe-for-untrusted-input rule.

---

# 4. Suggested order & cadence

**Recommended order:** 25 → 26 → 27 → 28 (scheduling track, hard-dependent); the storage track
(29 → 30) and the auth track (31) slot in anywhere. For low-risk-first, do **Stage 31 (auth)** and
**Stage 29 (compression)** early (both self-contained, with clear E2B references) and leave the
hardest — **Stage 26 rebalance** — until after its source read settles the migration semantics.

**Cadence (unchanged from the roadmap):** split each stage into independently verifiable sub-steps,
keep `go test ./services/...` + the Python e2e green at every step, give an honest 🔴/🟡/🟢
self-review before committing, commit only on the user's explicit go-ahead (one single-line
Conventional Commit per stage), and push to `origin/main` immediately after.
