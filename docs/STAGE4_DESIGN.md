# Stage 4 design: extract a Go control plane

> Status: in progress. This document is the agreed design; it is implemented in
> two sub-steps (4a then 4b). Read `docs/ARCHITECTURE.md` first for the three
> layers (client / protocol / daemon) this stage builds on.

## 1. Goal & non-goals

**Goal.** Move "start/stop a microVM" out of the SDK (`client.py`) into a
standalone **Go service — the control plane**. The SDK stops forking
`firecracker` itself; instead it asks the control plane over HTTP for a sandbox
and runs code through it. This introduces, for the first time, a real
**client ↔ cloud** network boundary — the thing that makes E2B a "cloud" rather
than a local library.

**Non-goals** (kept out on purpose, to bound the diff):

- **Do not touch `protocol.py`.** It stays the byte-stable contract — the most
  important invariant in the project.
- **Do not touch `server.py` / `backend.py`.** The in-VM daemon is baked into the
  rootfs exactly as today. In the E2B mapping these "belong with infra", but they
  stay physically in `src/` for now; relocating them is cosmetic work for a later
  stage, not needed here.
- **No warm pool, no auth, no multi-host scheduling.** Those are Stage 5+.

## 2. Target architecture

```
your program
   │ sandbox.run_code(...)
   ▼
┌──────────────┐ ① HTTP/TCP            ┌────────────────────────┐ ② vsock (CONNECT+HTTP/SSE) ┌──────────────┐
│ SDK client.py│ ────────────────────▶ │  control-plane (Go)     │ ─────────────────────────▶ │ daemon in VM │
│ (thin, pure  │  POST /sandboxes       │  • lifecycle: spawn /    │                            │ server.py    │
│  HTTP)       │  POST .../{id}/execute │    restore / destroy     │                            │ + backend.py │
│              │ ◀──────────────────── │  • transparent vsock     │ ◀───────────────────────── │ (jupyter)    │
└──────────────┘    SSE forwarded back  │    proxy (byte pipe)     │       SSE                   └──────────────┘
                                        └────────────────────────┘
                                           ▲ owns firecracker procs
                                           └ reads vendor/{firecracker,vmlinux,rootfs.ext4,snapshot}
```

① is the **new network boundary** (plain HTTP over TCP). ② is exactly what
`_VsockTransport` does today — it moves from Python into Go.

## 3. Key design decisions

### Decision 1 — the control plane is a thin proxy

Only the **lifecycle** (create/destroy) is real logic. The **data path** is a
generic reverse proxy: `ANY /sandboxes/{id}/<rest>` → strip the prefix → vsock
CONNECT to that VM → forward the `<rest>` request verbatim → copy the response
(SSE stream included) straight back.

Why: `protocol.py` stays the **single source of truth**; Go is a dumb pipe that
doesn't parse `/execute` vs `/files/*` vs `/commands`. Adding a new daemon
endpoint later needs zero Go changes. This preserves the project's core
principle — a byte-stable protocol boundary.

### Decision 2 — a minimal SDK ↔ control-plane protocol

This is the only new contract Stage 4 introduces:

| Method & path | Request body | Response | Replaces (in `client.py`) |
|---|---|---|---|
| `POST /sandboxes` | `{"from_snapshot": bool}` | `201 {"id":"sb_xxx"}` | `_spawn_microvm` / `_restore_microvm` |
| `DELETE /sandboxes/{id}` | — | `204` | `close()` |
| `ANY /sandboxes/{id}/<rest>` | verbatim | verbatim (transparent proxy) | `_VsockTransport.request` |

`timeout_seconds` is **not** in this protocol — it already rides inside each
`ExecuteRequest` (`protocol.py`) through path ②, so the control plane never sees
it. That keeps the new contract tiny.

### Decision 3 — "ready on delivery"

Today the SDK polls `_wait_until_healthy` itself. After the move, `POST
/sandboxes` does the `/health` probe **on the control plane** (through its own
vsock proxy) before returning `201`. The SDK gets back an id only once the VM is
ready — cleaner, and closer to E2B (the cloud hands you a ready sandbox).

### SDK → control-plane address resolution

`Sandbox(base_url=...)` parameter, defaulting to the `MICROSANDBOX_URL`
environment variable, defaulting to `http://127.0.0.1:8080`.

## 4. Code "from → to" map (against the current functions)

| Now (`client.py`) | Destination |
|---|---|
| `_MICROVM_*` constants, `_vendor_dir()` | → Go (vendor path via `--vendor-dir`) |
| `_spawn_microvm` (write config.json + start firecracker) | → Go: cold create |
| `_restore_microvm` + `_firecracker_api` (UDS `PUT /snapshot/load`) | → Go: snapshot create (`http.Client` over a unix socket) |
| `_check_microvm_available`, `_microvm_log` | → Go: preflight checks + console.log tail |
| `_VsockTransport` + `_Response` (CONNECT handshake + hand-written HTTP/1.1) | → Go: transparent vsock proxy |
| `_wait_until_healthy` | → Go: self-probe before delivery |
| `close()` (kill proc + rm workdir) | → Go: `DELETE` handler + process table |
| **stays in Python**: `run_code` / `_stream` / `_post_json` / `_Files` / `_Commands` / `Execution` assembly | unchanged; only the underlying transport switches to "plain HTTP to the control plane" |

After the move `client.py` shrinks sharply, and because ② no longer needs a
hand-written vsock handshake the SDK can use `urllib`/`http.client` (SSE read
line by line) — the hand-rolled socket code disappears. This is also what paves
the way for a TypeScript SDK in Stage 6: with no vsock left in the SDK, a
re-implementation is just a plain HTTP client.

## 5. Go service layout (minimal)

A new top-level directory in the **same repo** (no git-repo split yet):

```
control-plane/
  go.mod
  main.go        # flags, HTTP routing, graceful shutdown
  server.go      # the server struct (VM registry) + handlers
  microvm.go     # spawn / restore / destroy firecracker (ported from _spawn/_restore/close)
  proxy.go       # vsock CONNECT handshake + HTTP/SSE byte bridge (ported from _VsockTransport)  [4b]
  *_test.go
```

The built binary lands at `vendor/control-plane` (a regenerable artifact;
`vendor/` is gitignored, like the firecracker binary), built by
`scripts/build-control-plane.sh`.

## 6. Two independently verifiable sub-steps

### Stage 4a — lifecycle to Go (smallest diff, co-located)

- Go does create/destroy only. `POST /sandboxes` returns `{id, uds_path}`.
- The SDK still uses today's `_VsockTransport(uds_path, port)` to connect to the
  UDS directly (**same host**, shared filesystem).
- So `_spawn/_restore/_check/_microvm_log/_firecracker_api` move to Go;
  `_VsockTransport` and `test_transport.py` stay put and stay green.
- Limitation: SDK and control plane must be co-located (to share the UDS path).
  This is the cost of 4a as an intermediate; 4b removes it.

### Stage 4b — data path + health probe to Go (the real cloud shape)

- `_VsockTransport` + `_wait_until_healthy` move into Go (transparent proxy +
  self-probe before delivery). `POST /sandboxes` stops returning `uds_path`.
- The SDK becomes pure HTTP to `/sandboxes/{id}/...` and deletes all vsock code.
- From here the SDK and the control plane can live on **different machines**.

## 7. Keeping the tests green (honest trade-offs)

- **VM end-to-end tests** (`test_microvm*.py` / `test_stateful.py` /
  `test_files.py` / `test_sandbox.py`): a `conftest.py` fixture `go build`s the
  control-plane binary, starts it, probes it healthy, points `Sandbox(base_url=)`
  at it, then tears it down. Assertions barely change (still `s.run_code(...)`).
  Auto-skip gains a "no Go toolchain / build failed" reason on top of the
  existing "no firecracker/kvm/artifacts".
- **`test_transport.py`** (today's "runs anywhere, no VM" vsock unit test):
  unaffected in 4a; **in 4b the logic it covers has moved into Go, so it is
  ported to a Go test** (`proxy_test.go`). This is the necessary cost of moving
  vsock into Go.
- New `scripts/build-control-plane.sh`, plus a line in CLAUDE.md's common
  commands. The Go toolchain becomes a new dev dependency.
- The snapshot **single-instance** limitation (the uds path is baked into the
  snapshot) carries over unchanged — Stage 4 does not fix it; the warm pool
  (Stage 5) will, since it needs a per-VM uds override anyway.
