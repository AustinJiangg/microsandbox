# Stage 26R — Real per-sandbox live pause / resume (completing Stage 26 D4)

> **Naming.** The roadmap's Stages 27–31 are already assigned (per-node build placement, a real
> Nomad/Consul `Discovery`, compression, chunk cache, auth depth — `docs/POST_STAGE24_PLAN.md`).
> The *real per-sandbox live pause* is described there as "the deferred follow-on that would make
> the relocation move real VMs, not just its scheduling." So this stage is the **completion of
> Stage 26's Decision D4**, not a new roadmap number — hence **26R** (Rebalancing, real). It does
> not disturb 27–31.

## 1. What Stage 26 left, and what 26R finishes

Stage 26 delivered the **relocation control plane** — api `POST /sandboxes/{id}/pause` + `/resume`,
drain-aware resume affinity (`placement.PickPreferred`), origin tracking in `pkg/store`, and the
catalog route rewrite — and proved it **in process** against fake orchestrators
(`cmd/api/placement_integration_test.go`). The **real orchestrator's** `Pause`/`Resume` return
`codes.Unimplemented` (`cmd/orchestrator/grpc.go:64`), because a real per-sandbox live snapshot is
the Stage 20/22 re-snapshot path — the single most Firecracker-fragile path in the project, and at
Stage 26's writing "no FC-saga risk" was the chosen scope.

That fragility is now **resolved**: Stage 22 pinned `vendor/firecracker` to upstream **v1.10.1**,
which does not have the v1.16.0 writable-virtio re-snapshot regression, and the build-time layered
snapshot producer (`server.go:LayeredSnapshot`) runs end to end on this box (e2e 45/45, `--nbd` s3).
26R reuses that exact producer, **per running sandbox** instead of per build.

**Result of 26R:** the real orchestrator actually checkpoints a live user VM to object storage on
`Pause` and restores it on `Resume` (same sandbox id, possibly a different node), and the Python SDK
gains `Sandbox.pause()` / `.resume()`.

## 2. What E2B actually does (verified against the local `e2b-dev/infra` checkout)

Read `packages/orchestrator/internal/sandbox/sandbox.go` (`Pause`, l.875) and
`packages/api/internal/orchestrator/pause_instance.go`.

**Per-sandbox `Pause` (orchestrator side), `sandbox.go:875`:**
1. Stop the health check; **pause** the FC VM (`s.process.Pause`).
2. `CreateSnapshot` → the vmstate (snapfile). (E2B's custom FC drains+flushes the disk here.)
3. Fetch the sandbox's **original template** memfile + rootfs **headers**
   (`s.Template.Memfile().Header()`, `originalRootfs.Header()`) — the diff base.
4. `pauseProcessMemory` → a **memfile diff** of the current guest RAM against the original memfile
   header (dirty pages), keyed under a **fresh build id**.
5. `pauseProcessRootfs` → a **rootfs diff** from the writable overlay's dirtied blocks
   (`RootfsDiffCreator` over `s.rootfs`) against the original rootfs header.
6. Upload snapfile + metafile + memfile-diff + rootfs-diff under that build id.

**Per-sandbox `Pause` (api side), `pause_instance.go`:**
- The api **mints the snapshot's identity** (`UpsertSnapshot` → a new `TemplateID` + `BuildID`) and
  passes it in: `SandboxPauseRequest{SandboxId, TemplateId, BuildId}`. The orchestrator writes the
  diffs under that `BuildId`.
- The snapshot row records `OriginNodeID`. On **resume**, `create_instance.go` prefers the origin
  node but drops the pin when the origin's `Status() != Ready` → `PlaceSandbox` re-places via BestOfK
  (Stage 26 already built this half).

**Key facts this pins down (no guessing):**
- The diff base is the sandbox's **original template**, not the previous state. ✔
- The rootfs diff comes from the VM's **writable overlay**. ✔
- The **api owns the snapshot build id** and passes it to the orchestrator; resume restores *from
  that build id*. ✔
- A paused snapshot is just **another build id in object storage** with the normal artifact shape
  (`{buildID}/{snapfile,memfile,memfile.header,rootfs.ext4,rootfs.ext4.header,rootfs.path}`) — so
  **resume is a restore from an explicit build id**, nothing new in the read path.

## 3. Our adaptation — the mechanic maps onto machinery we already have

Every hard piece exists and is exercised by `LayeredSnapshot` today; 26R is the same producer minus
the "resume a base + run a command" preamble, because the VM to snapshot **is already the running
sandbox**.

| Step | 26R uses | Already built for |
|---|---|---|
| pause + Full snapshot a live VM | `fc.MicroVM.Snapshot(vmstate, memfile)` (`fc.go:505`) | Stage 20/22 |
| memfile COW diff over the template | `storage.PublishMemfileDiff(baseBuildID, memfile, snapBuildID)` (`cow.go:357`) | Stage 20 |
| rootfs COW diff from the overlay | `overlay.ExportToDiff()` + `storage.PublishRootfsDiffBlocks(baseBuildID, snapBuildID, …)` | Stage 22 |
| vmstate + baked rootfs path | `storage.PublishSnapfile` / `PublishRootfsBakedPath` | Stage 15/20 |
| restore (layered read) | `prepareRestore` + `buildRootfsBacking` (owner-mapped UFFD + NBD) | Stage 17/20/21 |
| restore under a chosen id | `fc.Restore(id, …)` takes an explicit id | Stage 4/8 |

**The one architectural change in the read path:** `prepareRestore` and `buildRootfsBacking` resolve
the build id internally via `ResolveAlias(tmpl.Name)`. A resume must target the **explicit snapshot
build id**, not the template alias. So both learn an optional build-id override (the template still
supplies `SnapshotDir`, vendor paths, and network; only the *which build's artifacts* changes).

**The one bookkeeping change on the write path:** in `--nbd` mode **every** sandbox already gets a
writable `block.Overlay` (`buildRootfsBacking`), but `restoreHealthy`/`spawnHealthy` discard the
reference (`_`). To pause a sandbox we must **reach** its overlay (and its base build id). So the
orchestrator retains `{overlay, baseBuildID}` per running sandbox — additive; the overlay already
lives until `Destroy` (held by the backing's `Close` closure), we just keep a handle.

### Base build id, and the pause→resume→pause chain

The sandbox's **base build id** is the template's alias build it was created from
(`ResolveAlias(tmpl.Name)` at create time). Pause diffs against that. On **resume**, the restored VM's
base becomes the **snapshot build id** it restored from, so a *second* pause diffs against the first
snapshot — a COW chain. This is safe because `PublishMemfileDiff` **flattens** the header
(`MergeMappings`, Stage 20): each snapshot's header already names, per block, the build that owns it,
back to the root — so the chain never deepens the read cost, and `layeredMemSource` / `openRootfsBase`
(multi-owner) serve it unchanged.

### The snapshot's baked rootfs path

The re-snapshotted vmstate references the rootfs path the sandbox VM booted with — the base's baked
path (`OpenRootfsBakedPath(baseBuildID)`, or `tmpl.Rootfs` for the non-layered default). 26R records
that under the snapshot build id (`PublishRootfsBakedPath`), exactly as `LayeredSnapshot` records the
base's path. On resume, `buildRootfsBacking` binds the snapshot's own NBD device over that path in a
per-VM mount namespace (Stage 21c), so the shared template rootfs is never clobbered.

## 4. Decisions

- **D1 — Reuse the Stage 20/22 producer, per sandbox.** No new snapshot mechanism. `Pause` = look up
  the live VM + its overlay + base build id → `fc.MicroVM.Snapshot` → the same three `Publish*` calls
  `LayeredSnapshot` makes → `Destroy`. Faithful to E2B (§2) and to our own build-time path.

- **D2 — The api owns the snapshot build id (E2B-faithful).** Proto `SandboxPauseRequest` gains
  `build_id`; the api mints it (a fresh build id), passes it in, and persists it. `Pause` still
  returns `Empty`. `SandboxResumeRequest` gains `snapshot_build_id`; the api reads it back from the
  store and passes it to the target node. This keeps id authority with the api (which owns catalog +
  store), matching E2B's `UpsertSnapshot` → `SandboxPauseRequest{BuildId}`.

- **D3 — `--nbd` + s3 only; honest refusal otherwise.** The overlay (rootfs diff) and object storage
  (diff upload/serve) are preconditions, identical to `LayeredSnapshot`. In local-fs / non-NBD mode
  `Pause` returns `codes.FailedPrecondition` with a clear message (not `Unimplemented` — the RPC *is*
  implemented, the mode just can't satisfy it). The e2e runs in `--nbd` s3 mode, like
  `test_layered_snapshot_via_api`.

- **D4 — Resume keeps the sandbox id stable.** `fc.Restore(req.SandboxId, …)` restores under the
  same id, so the catalog route rewrite and the SDK's reconnect (same id, fresh token) work exactly
  as Stage 26's in-process path already assumes. The restored VM re-registers with its retained
  `{overlay, baseBuildID = snapshot_build_id}` so it can be paused again.

- **D5 — Store persists the snapshot build id.** `pkg/store` adds a `snapshot_build` column (idempotent
  ALTER, like Stage 16's `team_id` / Stage 26's `origin_node`). `PauseSandbox(id, originNode,
  snapshotBuild)` and `PausedSandbox(id) → (origin, template, snapshotBuild, paused, err)`. Both
  SQLite + Postgres.

- **D6 — SDK surface (`pause()` / `resume()`).** `Sandbox.pause()` → `POST …/pause` (marks the local
  object paused; the data path is gone). `Sandbox.resume()` → `POST …/resume`, which returns
  `data_url` + a fresh `token`; the SDK adopts them and can run code again under the same id. No new
  wire concepts — reuses the create/reconnect shape.

- **D7 — Honest scope: single-box fidelity, not speed; single-node real VM.** The real-VM pause/resume
  is proven on **one** orchestrator (create → pause → resume → state carried). The **multi-node**
  relocation (pause on A, resume on B when A drains) stays verified **in process** (Stage 26) —
  running two real orchestrators on one box is not an E2B concept (Stage 23/24 rationale). So 26R makes
  the *mechanic* real; the *cross-node scheduling* was already real (in process) in Stage 26.

## 5. Sub-steps (each independently verifiable, committed on your go-ahead)

- **26R-a — orchestrator retains `{overlay, baseBuildID}` per sandbox (bookkeeping, no behavior
  change).** Introduce a small `liveSandbox{vm, overlay, baseBuildID}`; `s.sandboxes` maps id → it.
  Thread the overlay + base build id out of `restoreHealthy`/`spawnHealthy` (and the warm pool's
  factory) instead of discarding them. `destroy`/`lookup`/`list`/the data proxy unchanged in
  behavior. **Verify:** `go test ./services/...` green; a unit test that create registers a non-nil
  overlay in `--nbd` mode (and nil in local-fs).

- **26R-b — proto + store + api plumbing for the snapshot build id (in-process still green).** Proto:
  `SandboxPauseRequest{sandbox_id, build_id}`, `SandboxResumeRequest{sandbox_id, config,
  snapshot_build_id}`; `scripts/gen-proto.sh`. Store: `snapshot_build` column + the extended
  `PauseSandbox`/`PausedSandbox`. api: `handleSandboxPause` mints + passes + persists the build id;
  `handleSandboxResume` reads + passes it. Fake orchestrator (`placement_integration_test.go`) accepts
  the new fields (still a list move). **Verify:** the Stage 26 relocation integration test green with
  the id threaded; store round-trip on SQLite + (skip-guarded) Postgres.

- **26R-c — real orchestrator `Pause`.** Reuse the `LayeredSnapshot` tail: look up the `liveSandbox`
  → `vm.Snapshot` → `PublishMemfileDiff` + `PublishSnapfile` + `PublishRootfsBakedPath` +
  `overlay.ExportToDiff` → `PublishRootfsDiffBlocks` under `req.BuildId` → `destroy(id)`. Guard
  `--nbd` + s3 (else `FailedPrecondition`, D3). **Verify:** covered by the 26R-f e2e (pause alone is
  observable via a bucket probe, but the natural check is the round-trip).

- **26R-d — real orchestrator `Resume`.** Add an explicit-build-id restore path (`prepareRestore` /
  `buildRootfsBacking` learn a build-id override) → `fc.Restore(req.SandboxId, …)` → register the
  restored `liveSandbox` (base = `snapshot_build_id`). **Verify:** the 26R-f e2e.

- **26R-e — SDK `pause()` / `resume()`.** Thin HTTP calls (D6). **Verify:** exercised by the e2e; no
  new unit surface.

- **26R-f — gated real-VM e2e + docs.** `tests/test_pause_resume.py` (gated like
  `test_layered_snapshot_via_api`: `--nbd` s3 + KVM/root/v1.10.1): create → `run_code("x=41")` (RAM) +
  write a file (disk) → `pause()` → `resume()` → `run_code("print(x+1)")` == `42` (RAM carried) + read
  the file back (disk carried). Update `CLAUDE.md` (a "Done (Stage 26R)" bullet) +
  `docs/POST_STAGE24_PLAN.md` (mark the deferred follow-on done).

## 6. Honest scope

26R makes the **real-VM per-sandbox pause/resume** work — the deferred half of Stage 26 (D4) — by
reusing the Stage 20/22 producer per running sandbox, and exposes it in the SDK. It is proven on a
real VM **single-node** (create → pause → resume → state carried). It does **not** add cross-node live
migration on real VMs (not a single-box E2B concept; the cross-node *scheduling* is already
in-process-verified from Stage 26). On one box this is **fidelity, not speed** — pause/resume on one
machine buys a feature (checkpoint/restore a sandbox) and closes the roadmap's last "not real on VMs"
gap, not latency. It inherits the Stage 22 Firecracker constraints: **`v1.10.1` + `--nbd` + s3**.
Still a learning implementation, **not security-audited** — the safety rule is unchanged.

## 7. Tests

- Go units (`go test ./services/...`, KVM-free): 26R-a overlay-retention; 26R-b store round-trip
  (`snapshot_build`) on SQLite + skip-guarded Postgres; the existing `placement_integration_test.go`
  relocation scenario stays green with the snapshot id threaded through the fake orchestrator.
- Real-VM e2e (gated, `--nbd` s3 + KVM/root/v1.10.1): `tests/test_pause_resume.py` — the round-trip
  above. Auto-skips where the privilege/artifacts/module are missing, like the other real-VM tests.
- No regression: `test_microvm` / `test_layered_snapshot_via_api` unchanged.
