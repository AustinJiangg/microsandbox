# Stage 6 design: templates (custom sandbox images)

> Status: **proposed design — pending review**. To be implemented in three
> sub-steps (6a → 6b → 6c). Read `docs/STAGE4_DESIGN.md` (the control-plane split)
> and `docs/STAGE5_DESIGN.md` (the warm pool) first — Stage 6 generalizes the
> *single* image those stages assume into *named* images, and reuses the warm pool
> per template.

## 1. Goal & non-goals

**Goal.** Let a user define and boot **named custom images**, exactly like E2B's
headline feature. A **template** is a named `(rootfs.ext4, snapshot/)` pair built
from its own Dockerfile; the SDK selects one with `Sandbox(template="ml-env")`:

```python
# build once:  scripts/build-template.sh ml-env   (docker build -> rootfs -> snapshot)
with Sandbox(template="ml-env") as sb:            # boots from ml-env's rootfs
    sb.run_code("import numpy; print(numpy.__version__)")   # numpy baked into the template
```

This is the feature that makes the project feel like E2B: today every sandbox is
the one stock image baked into `vendor/rootfs.ext4`; after Stage 6 a sandbox can be
*your* image with *your* dependencies and files pre-installed.

The design rests on a single observation: **the whole rootfs → snapshot → warm-pool
chain already exists — it is just hard-wired to one image.** Stage 6 turns "the one
image" into "an image picked by name", changing as little as possible. The default
template reproduces today's behavior byte-for-byte.

**Non-goals** (kept out on purpose, to bound the diff):

- **No remote template registry / versioning / upload.** E2B builds templates in
  its cloud and gives them content-addressed IDs. Here the "registry" is just the
  filesystem under `vendor/templates/`, and building stays a local script. A real
  registry (DB, versions, GC) is later productization.
- **No per-template resource limits (vCPU / mem) yet.** `machine-config` stays the
  global `vcpus`/`memMiB` constants. Per-template overrides are an easy later add,
  but `mem_size_mib` must match the snapshot's memory size, so it is scoped out of
  the core stage. Noted as a refinement in §3 Decision 7.
- **Do not touch `server.py` / `backend.py`.** The in-VM daemon is identical across
  templates — only the *rootfs contents* differ (different packages/files), not the
  daemon that serves them. So nothing inside the VM changes.
- **Keep the protocol change minimal and backward-compatible** — one optional field
  (§3 Decision 2). The transparent data-path proxy is untouched.
- **No auth, no multi-host scheduling.** Those remain later stages.

## 2. Target architecture

```
 recipes (source, in git)              built artifacts (regenerable, gitignored)
 templates/                            vendor/
   ml-env/Dockerfile  ── build-template.sh ─▶  rootfs.ext4   snapshot/        ← "default" (legacy paths, unchanged)
   web/Dockerfile     ──────────────────────▶  templates/ml-env/{rootfs.ext4, snapshot/}
                                               templates/web/{rootfs.ext4, snapshot/}

your program                ┌───────────────────── control-plane (Go) ──────────────────────┐
 POST /sandboxes ───────────┼─▶ handleCreate                                                  │
 {template, from_snapshot}  │     │ resolve template name ─▶ {rootfs, snapshotDir}  (template.go)
                            │     │      (invalid/missing name ─▶ 400/404)                    │
                            │     │ pick source of the VM:                                    │
                            │     │   pools[template] ready?  ─▶ pool.get()  (~ms)            │
                            │     │   else from_snapshot      ─▶ restore(template.snapshot)    │
                            │     │   else                    ─▶ spawn(template.rootfs)        │
                            │     ▼                                                            │
                            │   registry[id] ─▶ proxy ─vsock─▶ VM   (data path: UNCHANGED)     │
                            │                                                                  │
                            │   pools map[name]*pool : one warm pool per pre-warmed template    │
                            └──────────────────────────────────────────────────────────────────┘
```

Two parallel `templates/` trees, intentionally: `templates/<name>/Dockerfile` is the
**recipe** (source-controlled); `vendor/templates/<name>/` holds the **built
artifacts** (regenerable, gitignored like every other `vendor/` artifact). The
control plane only ever reads the latter.

## 3. Key design decisions

### Decision 1 — a template is a named directory; the registry is the filesystem

The control plane resolves a template *name* to a pair of artifact paths:

| template name | rootfs | snapshot dir |
|---|---|---|
| `default` (or absent) | `vendor/rootfs.ext4` | `vendor/snapshot/` |
| `<name>` | `vendor/templates/<name>/rootfs.ext4` | `vendor/templates/<name>/snapshot/` |

`default` maps to **today's legacy paths**, so no files move, the snapshot is **not
rebuilt**, and every existing test, fixture and the quickstart keep working
untouched. Resolution is **lazy**: `POST /sandboxes` resolves the requested name and
runs the existing `checkAvailable` preflight against *that* template's artifacts; a
name with no `rootfs.ext4` on disk is a clean `404`, a `from_snapshot` request for a
template with no `snapshot/` is a clear error.

Why filesystem-as-registry: it matches how `vendor/` already works — *artifact
present on disk = capability available* (exactly what `checkAvailable` already
encodes for firecracker/kernel/rootfs). A DB-backed registry with IDs and versions
is what a product needs; a learning implementation does not.

### Decision 2 — the one protocol change: an optional `template` field

`POST /sandboxes` gains an optional field; everything else in the contract is
unchanged:

| Method & path | Request body | Response |
|---|---|---|
| `POST /sandboxes` | `{"from_snapshot": bool, "template"?: string}` | `201 {"id"}` |

- **Backward-compatible additive change.** An omitted/empty `template` means
  `"default"` = today's behavior, so an old SDK keeps working verbatim. This is the
  same discipline as Stage 2c, which *added* the file/command endpoints without
  disturbing the three `/execute` types — additions are allowed, the existing shape
  is never broken.
- **`template` is orthogonal to `from_snapshot`.** One picks *which image*, the
  other picks *cold vs restore*:

  | | `from_snapshot=false` | `from_snapshot=true` |
  |---|---|---|
  | `template=default` | cold start, stock rootfs (today) | restore stock snapshot (today) |
  | `template=X` | cold start from X's rootfs | restore from X's snapshot |

- **The data path is completely untouched.** `ANY /sandboxes/{id}/<rest>` still
  pipes bytes to the in-VM daemon; `protocol.py` (the client↔daemon contract) does
  not change at all. The only contract that grows is the tiny SDK↔control-plane one
  from Stage 4, by one optional field.

### Decision 3 — validate template names (path-traversal safety)

A name becomes a filesystem path under `vendor/templates/`, so it must be validated
before use: accept `^[a-z0-9][a-z0-9_-]*$`, reject everything else (notably `..`,
`/`, empty). This is small but real — without it `template=../../etc` would let a
request point the VM at an arbitrary path. The check lives in `template.go` and is
unit-tested without a VM.

### Decision 4 — one warm pool **per template**

Stage 5's `server.pool` is a single `*pool` restoring from the one snapshot. Stage 6
makes it `pools map[string]*pool`, one entry per template that opts into pre-warming;
each pool's restore closure targets *that template's* snapshot.

- `--pool-size K` keeps its Stage 5 meaning: **K warm VMs of the `default`
  template** (so Stage 5 behavior and its tests are unchanged).
- A new repeatable `--pool <name>=<K>` flag pre-warms named templates, e.g.
  `--pool ml-env=2 --pool web=1`.
- `handleCreate` looks up `pools[template]`; hit → `pool.get()` (~ms), miss → restore
  inline (never worse than no pool). `destroyAll` drains *every* pool.
- **Memory is the cap, now summed across templates**: warm memory ≈ Σ_t (K_t × 512
  MiB). On a laptop that bounds how many templates you can keep warm at once — a
  documented limit (Stage 5 Decision 3, multiplied), opt-in per template, default
  none. The `pool` struct itself is reused unchanged; only the server holds a map of
  them.

### Decision 5 — building a template = a recipe + the two existing scripts, parameterized

`scripts/build-rootfs.sh` **already takes `[image] [output_path]`** (lines 19-20), so
it needs no change. `scripts/build-snapshot.sh` is hard-wired to `vendor/rootfs.ext4`
+ `vendor/snapshot/` (lines 13-14, 68) and gets parameterized to take a rootfs path +
an output snapshot dir. A thin new `scripts/build-template.sh <name> [dockerfile]`
orchestrates the three steps:

```
docker build -f templates/<name>/Dockerfile -t microsandbox-tmpl-<name> .
scripts/build-rootfs.sh   microsandbox-tmpl-<name>  vendor/templates/<name>/rootfs.ext4
scripts/build-snapshot.sh vendor/templates/<name>/rootfs.ext4  vendor/templates/<name>/snapshot   # optional (only if you want ms restore)
```

This mirrors E2B's `e2b template build`, but local and script-shaped. The default
template still builds with the bare `build-rootfs.sh` / `build-snapshot.sh` (no name),
so its commands are unchanged.

### Decision 6 — repair `build-snapshot.sh` (it broke in Stage 4b)

`build-snapshot.sh` still imports `from microsandbox.client import _VsockTransport`
(lines 42-43), but Stage 4b removed all vsock code from `client.py`. So the script
**currently fails with `ImportError`** if run — it is only masked because the
committed `vendor/snapshot/` predates 4b and `conftest.py:ensure_snapshot` rebuilds
only when the artifact is *absent*. Templates need working per-template snapshot
builds, so 6a repairs it.

Fix: inline a tiny vsock client in the script's warm-up heredoc — the CONNECT
handshake (`CONNECT 1024\n` → `OK …`) plus one HTTP/1.1 request, ~20 lines, no
dependency on the SDK. Snapshot *creation* must stay an offline script (not routed
through the control plane) because it needs the base VM's raw Firecracker API socket
to `PATCH /vm Paused` + `PUT /snapshot/create`, and the control plane owns that
socket privately and does not proxy it.

### Decision 7 — machine-config stays global (scoping note)

`vcpus` / `memMiB` remain global constants shared by all templates. Per-template
CPU/mem is a natural follow-up (E2B exposes it), but it interacts with the snapshot
(`mem_size_mib` must equal the snapshot's memory size) and with the pool, so it is
deliberately out of this stage. When added, it belongs as per-template metadata
(e.g. a `vendor/templates/<name>/template.json`) resolved alongside the paths.

## 4. Code "from → to" map

| Now | Stage 6 |
|---|---|
| `microvm.go` `spawnMicroVM(id, vendorDir)` uses `filepath.Join(vendorDir,"rootfs.ext4")` | take a resolved `template` (rootfs path); cold-start from it |
| `microvm.go` `restoreMicroVM(id, vendorDir)` uses `filepath.Join(vendorDir,"snapshot")` | take the template's snapshot dir; restore from it |
| `microvm.go` `checkAvailable(vendorDir)` checks the one rootfs | per-template preflight (rootfs for cold, snapshot for restore) |
| **6a new** `template.go` | name validation + `resolve(name) -> {rootfs, snapshotDir}` (default → legacy paths) |
| `server.go` `pool *pool`; `newServer(vendorDir, poolSize)` | **6c**: `pools map[string]*pool`; build pools from `--pool name=K` (+ `--pool-size` ⇒ `default`) |
| `server.go` `handleCreate` reads only `from_snapshot` | **6b**: also read `template`; resolve it; **6c**: pick `pools[template]` |
| `server.go` `destroyAll` drains the one pool | **6c**: drain every pool in the map |
| `main.go` flags `--addr --vendor-dir --pool-size` | **6c**: add repeatable `--pool name=K` |
| `client.py` `Sandbox(timeout_seconds, from_snapshot, base_url)` | **6b**: add `template=`; send it in `_create`'s POST body |
| `scripts/build-rootfs.sh` (already `[image] [out]`) | unchanged |
| `scripts/build-snapshot.sh` (hard-wired paths; broken `_VsockTransport` import) | **6a**: parameterize `[rootfs] [out_dir]`; repair with an inline vsock client |
| — | **6a new**: `scripts/build-template.sh <name>`; `templates/<name>/Dockerfile` convention; a tiny example template for the e2e test |

## 5. Go service layout (additions only)

```
control-plane/
  template.go      # 6a: name validation + resolve(name) -> {rootfs, snapshotDir}; per-template preflight
  template_test.go # 6a: KVM-free unit tests (valid/invalid names, default vs named resolution)
  microvm.go       # 6a: spawn/restore/checkAvailable take a resolved template (small edits)
  server.go        # 6b: handleCreate reads `template`; 6c: pools map[string]*pool
  main.go          # 6c: repeatable --pool name=K flag, build the pools map
  pool.go          # reused unchanged (the per-template pool is the same struct)
```

## 6. Three independently verifiable sub-steps

### Stage 6a — multi-image plumbing (build side + registry)

The whole "more than one image" machinery, with **no protocol/SDK change yet**: the
control plane can boot a named template, selected internally (e.g. via a temporary
`--default-template` or a direct unit test) — proving the registry + per-template
spawn/restore before the wire contract grows.

- `template.go` (+ `template_test.go`): name validation and resolution, KVM-free.
- `microvm.go`: `spawn/restore/checkAvailable` take a resolved template.
- `build-snapshot.sh` repaired + parameterized; new `build-template.sh`; a tiny
  example template under `templates/`.
- **e2e (auto-skips like the other VM tests)**: build a minimal example template
  whose rootfs carries a marker the default lacks — cheapest is a **marker file**
  injected by its Dockerfile (no pip, so the `docker export + mkfs` stays fast) —
  cold-start it through the control plane and assert the marker is present (and
  absent in a default sandbox). Proves a second image really boots and is isolated
  from the default.

### Stage 6b — the `template` field (protocol + SDK)

- `client.py`: `Sandbox(template="…")`, sent in the create body.
- `server.go` `handleCreate`: read `template`, resolve, spawn/restore from it.
- **Backward-compat is the key assertion**: an omitted `template` still resolves to
  `default`, so every existing test (which passes no template) is unchanged.
- **e2e**: `Sandbox(template="…")` end-to-end through the public SDK surface.

### Stage 6c — per-template warm pools

- `server.go`: `pool` → `pools map[string]*pool`; `handleCreate` pops from
  `pools[template]`; `destroyAll` drains all. `main.go`: repeatable `--pool name=K`.
- **Verifiable without KVM**: extend `pool_test.go` (or add `pools_test.go`) to cover
  the map — independent per-template pools, default via `--pool-size`, miss falls
  back to inline restore. The single-`pool` semantics from Stage 5 are reused, so the
  existing pool unit test stays valid.
- **No regression**: with no `--pool` flags the map is empty and behavior equals
  Stage 5 at `--pool-size 0`; `--pool-size K` still warms exactly the default
  template.

## 7. Keeping tests green (honest trade-offs)

- **The default template = legacy paths**, so the entire existing Python suite, the
  `conftest.py` fixtures, and the quickstart run **unchanged**, and the snapshot is
  **not** rebuilt (same discipline as Stage 5 Decision 1).
- **New Go unit tests are KVM-free** (`template_test.go` name/resolution,
  `pools_test.go` map accounting via the injected fake restore), so
  `go test ./control-plane` stays green on any machine — mirroring how `proxy_test.go`
  / `pool_test.go` already cover their logic without a VM.
- **The named-template e2e costs a second rootfs build** (`docker export` + `mkfs.ext4
  -d`) in any environment that actually has firecracker/kvm. Trade-off: keep the
  example template *tiny* — a marker file injected by its Dockerfile rather than a pip
  install — so the extra build is cheap, and gate it behind the same auto-skip as the
  other VM tests. A pool-enabled e2e is again **not** added (Stage 5's reasoning
  holds): pool *semantics* are proven in Go, pool *plumbing* by 6a/6b's real-VM boots.
- **`build-snapshot.sh` is repaired** as part of 6a; until then it is silently broken
  post-4b (§3 Decision 6). Worth a line in the commit so the fix is not mistaken for
  unrelated churn.
- **Per-template pool memory multiplies** (Decision 4) — document the cap; it is a
  resource limit, not a correctness issue.
- After 6c lands: update CLAUDE.md ("current state" + the common-commands block:
  `build-template.sh`, `--pool name=K`), add a "custom templates" example to README,
  and add this doc to the README docs index.
