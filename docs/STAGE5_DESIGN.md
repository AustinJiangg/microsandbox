# Stage 5 design: the warm pool

> Status: agreed design; implemented in two sub-steps (5a then 5b). Read
> `docs/STAGE4_DESIGN.md` first — Stage 5 lives entirely inside the Go control
> plane that Stage 4 built, and deliberately leaves the byte-stable protocol and
> the SDK untouched.

## 1. Goal & non-goals

**Goal.** Make `POST /sandboxes {"from_snapshot": true}` return a ready sandbox in
**~milliseconds** by handing out a VM that was already restored in the background.
Two mechanisms, in order:

- **5a — lift the single-instance limit.** Today only *one* VM can be restored
  from the snapshot at a time, because the vsock host socket path is baked into
  the snapshot (`vendor/snapshot/fc.vsock`). Give each restored VM its own socket
  so N can run at once. This is the prerequisite for a pool of any size.
- **5b — the pool itself.** Keep K restored-and-healthy VMs waiting in the control
  plane; serve a request by popping one and refill in the background.

**Non-goals** (kept out on purpose, to bound the diff):

- **Do not touch `protocol.py`.** The warm pool is invisible to it — it stays the
  byte-stable contract, the project's most important invariant.
- **Do not touch `server.py` / `backend.py` and do not rebuild the snapshot.** 5a
  *overrides* the baked-in socket path at load time, so the existing
  `vendor/snapshot/*` works unchanged (see Decision 1). The in-VM daemon is
  untouched.
- **Do not change the SDK.** It already sends `{"from_snapshot": true}`; the pool
  is a server-side latency optimization. No new SDK ↔ control-plane contract.
- **No multi-host scheduling, no auth, no idle eviction / autoscaling.** Those are
  later productization; this stage is one host, fixed K.

## 2. Target architecture

```
                  ┌───────────────────────── control-plane (Go) ──────────────────────────┐
your program      │                                                                        │
 POST /sandboxes ─┼─▶ handleCreate                                                          │
 {from_snapshot}  │     │ from_snapshot && pool on?                                         │
                  │     │      ├─ yes ─▶ pool.get() ─ ready VM (~ms) ─┐                      │
                  │     │      └─ no  ─▶ spawnMicroVM (cold ~0.94s) ──┤                      │
                  │     │                                            ▼                       │
                  │     │                                   registry[id] ─▶ proxy ─vsock─▶ VM│
                  │     │                                                                    │
                  │  pool (background): hold K restored+healthy VMs; refill after each get   │
                  │     restoreMicroVM ×K  ─ each gets its OWN workdir/fc.vsock (5a)          │
                  └────────────────────────────────────────────────────────────────────────┘
```

5a changes only how `restoreMicroVM` wires the socket. 5b adds the pool box and a
one-line branch in `handleCreate`. The data path (proxy over vsock) is unchanged —
it already keys off each VM's own `udsPath`.

## 3. Key design decisions

### Decision 1 — per-VM uds via Firecracker's `vsock_override` (5a)

Firecracker **v1.16.0** — the exact version in `vendor/` — added a
`vsock_override` field to `PUT /snapshot/load` (CHANGELOG: *"Add support for Vsock
Unix domain socket path overriding on snapshot restore."*). The load body becomes:

```jsonc
{ "snapshot_path": ".../vmstate",
  "mem_backend": { "backend_type": "File", "backend_path": ".../memfile" },
  "vsock_override": { "uds_path": "<this VM's workdir>/fc.vsock" },  // ← new
  "resume_vm": true }
```

So each restored VM listens on a socket inside *its own* `MkdirTemp` workdir, and
the path baked into the snapshot is simply ignored. Two consequences worth stating:

- **No snapshot rebuild.** The override wins over the baked path, so
  `vendor/snapshot/*` stays exactly as built in Stage 3c.
- We **drop the `os.Remove(uds)` hack** in `restoreMicroVM`: it existed only to
  clear the *shared* baked socket before re-listening on it. A fresh per-VM
  workdir has nothing to clear.

Why not the alternative (bake a *relative* `uds_path` and run each Firecracker in
its own `cwd`)? That works on older Firecracker, but `vsock_override` is explicit,
needs no `chdir` games, and we already run the right version. Use the native field.

Note on the guest CID: it stays `3` for every VM, and that is correct. The CID
addresses the *guest* within one VMM; each VM is a separate Firecracker process
with its own host socket, so the host reaches a specific VM by its `udsPath`, not
by CID. (Only a shared host vsock device would need distinct CIDs.) The read-only
`rootfs.ext4` is likewise safe to share across VMs — all writes go to the in-VM
tmpfs.

### Decision 2 — the pool is control-plane-internal (5b)

The SDK keeps doing `POST /sandboxes {"from_snapshot": true}`; only *where the VM
comes from* changes (a pre-warmed one instead of a fresh restore). No protocol
change, no SDK change — this is the whole point of having put the lifecycle behind
an HTTP boundary in Stage 4.

### Decision 3 — eager fixed-size pool, refill-on-get, synchronous fallback

- `--pool-size K` flag, **default 0 = today's behavior** (restore on the request
  path). So every existing test runs unchanged unless it opts in.
- Fill to K at startup (in the background; startup does not block on it).
- `get()`: pop a ready VM if one exists; **otherwise restore synchronously** right
  there (never worse than today). Either way, kick one async refill, bounded so
  `len(ready) + inflight ≤ K`. The pool is a best-effort accelerator, never a
  bottleneck and never an unbounded VM factory.
- **Memory is the real cap on K**: each VM maps the 512 MiB memfile, so K is small
  on a laptop (2–4). This is a documented limit, not a correctness issue.

### Decision 4 — health is probed at warm time; idle death is a known gap

A pooled VM is `waitHealthy`-probed when it is restored (off the request path), so
`get()` hands out an already-healthy VM. A VM that dies *while idle in the pool* is
not re-probed — acceptable for a learning implementation; a note will say a
production pool would re-probe on handout and evict dead/stale VMs.

### Decision 5 — testable without KVM (injected restore fn)

The pool takes its "make one VM" function as a dependency
(`restore func(id string) (*microVM, error)`). A Go unit test injects a fake that
returns dummy handles, exercising **get / refill / drain / fallback / the
`len+inflight ≤ K` accounting** with no VM or `/dev/kvm`. This mirrors how
`proxy_test.go` covers the vsock bridge without a VM, and keeps
`go test ./control-plane` green everywhere.

### Decision 6 — shutdown drains the pool

`destroyAll` currently kills only VMs in the active registry. It must also destroy
the **idle** VMs sitting in the ready queue, or they leak on Ctrl-C.

## 4. Code "from → to" map

| Now | Stage 5 |
|---|---|
| `restoreMicroVM`: `uds := filepath.Join(snap,"fc.vsock")`, `os.Remove(uds)`, load body without override | **5a**: `uds := filepath.Join(workdir,"fc.vsock")`, drop `os.Remove`, add `vsock_override` to the load body |
| `server.handleCreate`: always restore/spawn inline | **5b**: `if from_snapshot && pool != nil { vm = pool.get() } else { … }` |
| `server.destroyAll`: drains the active registry only | **5b**: also drain the ready pool |
| `main.go`: `--addr`, `--vendor-dir` | **5b**: add `--pool-size` (default 0); construct the pool and pass it to the server |
| — | **5b new**: `pool.go` (ready queue + background refill + drain), `pool_test.go` (injected restore fn, KVM-free) |

## 5. Go service layout (additions only)

```
control-plane/
  microvm.go     # 5a: vsock_override in restoreMicroVM (small edit)
  pool.go        # 5b: warm pool — ready queue, background refill, drain
  pool_test.go   # 5b: KVM-free unit test via an injected restore fn
  server.go      # 5b: handleCreate pops from the pool; destroyAll drains it
  main.go        # 5b: --pool-size flag, wire the pool in
```

## 6. Two independently verifiable sub-steps

### Stage 5a — per-VM uds (smallest diff, unblocks everything)

- The `restoreMicroVM` edit above. Nothing else in Go changes — `server.go` /
  `proxy.go` already use each VM's own `udsPath`.
- **New e2e test** (real VM, auto-skips without go/firecracker/`/dev/kvm`/vendor
  artifacts, same gate as the other VM tests): restore several
  `from_snapshot=True` sandboxes *concurrently* from the one snapshot, set distinct
  state in each (e.g. `x=1`, `x=2`, …), read it back, and assert no cross-talk —
  proving both concurrency and isolation. The existing single-restore snapshot
  test keeps passing (override works for N=1 too).

### Stage 5b — the warm pool

- `pool.go` + `pool_test.go` + the `handleCreate` / `destroyAll` / `main.go`
  wiring.
- **Verifiable without KVM**: `pool_test.go` asserts get/refill/drain/fallback and
  the `len+inflight ≤ K` accounting against an injected fake restore.
- **No regression**: the whole existing suite runs with the default `--pool-size 0`
  and is unaffected. (The pool's *real-VM* behavior is the same `restoreMicroVM`
  that 5a already proved end-to-end, just moved off the request path — so 5b leans
  on the deterministic Go unit test rather than a timing-sensitive e2e.)

## 7. Keeping tests green (honest trade-offs)

- **5a**: one new real-VM e2e case; auto-skip reasons unchanged. No snapshot
  rebuild (Decision 1), so the test fixture is untouched.
- **5b**: `pool_test.go` is new and KVM-free, so `go test ./control-plane` stays
  green on any machine. The Python suite stays at `--pool-size 0` and is unchanged;
  a pool-enabled e2e is intentionally **not** added — it would mean threading a
  pool-size flag through `conftest.py` and asserting on timing/refill races, which
  buys little over the deterministic Go test. This is the deliberate trade-off:
  pool *semantics* are proven in Go, pool *plumbing* (restore correctness) is
  proven by 5a's e2e.
- After 5b lands, update CLAUDE.md's "current state" and the common-commands block
  (`--pool-size`), and note the K-vs-memory limit.
