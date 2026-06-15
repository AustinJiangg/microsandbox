# Stage 7 design: a Go in-VM daemon (envd)

> Status: **agreed direction**; first stage of the broader "E2B alignment". D3 (how
> Go drives the kernel) is decided: **Jupyter Server over WebSocket**, the E2B-faithful
> path. To be implemented in three sub-steps (7a → 7b → 7c). Read
> `docs/ARCHITECTURE.md` (the layers) and `docs/STAGE4_DESIGN.md` (the control-plane
> split) first — Stage 7 rewrites the *daemon* layer and deliberately leaves the
> byte-stable protocol, the SDK, and the control plane untouched.

## 1. Goal & non-goals

**Goal.** Rewrite the in-VM daemon — today `src/microsandbox/server.py` +
`backend.py` (Python) — as a **Go binary**, matching E2B's `envd` (Go). The daemon
is the resident agent inside the microVM: it listens on vsock, serves
`/health` · `/execute` · `/files/*` · `/commands`, and drives the stateful Python
kernel. After Stage 7 that process is Go; the rootfs bakes in a static Go binary as
PID-1's payload instead of a Python interpreter running our package.

Why this is the first E2B-alignment step: the daemon is the one core-architecture
layer still in Python where E2B is Go (see the mapping table in
`docs/ARCHITECTURE.md`: `server.py` ↔ `envd`). It is also self-contained — the
protocol boundary means we can swap the daemon's language with zero change above it.

**Non-goals** (kept out on purpose, to bound the diff):

- **Do not touch `protocol.py`.** The Go daemon must emit the *exact* HTTP/SSE bytes
  the Python daemon did — same `data: {...}` SSE events, same JSON shapes. The
  protocol is the contract that has stayed byte-stable since Stage 0; this stage is
  another "swap the implementation under a fixed protocol" move, the largest yet.
- **Do not touch the SDK (`client.py`) or the control plane (`control-plane/`).**
  They are already transport-agnostic (pure HTTP; the control plane pipes bytes over
  vsock). They cannot tell a Go daemon from a Python one.
- **Do not reimplement the Python interpreter.** The *interpreter* is intrinsically
  Python — a stateful IPython/Jupyter kernel (variables persist across `run_code`).
  E2B's envd is Go but the kernel is still a separate Python process it manages. So
  Stage 7 rewrites the *daemon* (the serving/management process), and the Go daemon
  *manages and drives* a Python kernel — it does not run Python itself.
- **No new endpoints, no PTY/process-streaming, no auth.** Strict parity with today's
  daemon; new capabilities are later stages.

## 2. Target architecture

```
                         Firecracker microVM (rootfs)
   ┌───────────────────────────────────────────────────────────────────┐
   │  /init (PID 1) ── execs ──▶ microsandbox-daemon  (Go, static)        │
   │                                 │  listens on vsock:1024 (HTTP/SSE)  │
   │   control plane ──vsock──▶ ─────┤  /health /files/* /commands  → Go   │
   │   (unchanged)                   │  /execute ──┐                       │
   │                                 │             ▼                       │
   │                          drives a stateful Python kernel              │
   │                          (ipykernel) over the Jupyter protocol        │
   │                                 │  iopub stream/result/error/status   │
   │                                 ▼                                     │
   │                          OutputEvents → SSE (identical bytes)         │
   └───────────────────────────────────────────────────────────────────┘
```

Everything above the vsock line (control plane, SDK, protocol) is unchanged. Inside
the VM, a Go binary replaces the Python daemon; the Python *kernel* remains, now a
child the Go daemon launches and talks to.

## 3. Key design decisions

### Decision 1 — byte-stable protocol parity (the whole discipline)

The Go daemon reproduces the current responses exactly. The `/execute` SSE contract
(from `backend.py`) the Go daemon must match:

| kernel signal (iopub) | OutputEvent |
|---|---|
| `stream` stdout/stderr | `STDOUT` / `STDERR` with the text |
| `execute_result` / `display_data` `text/plain` | `STDOUT` with the value + `\n` |
| `error` (traceback) | `STDERR` with the ANSI-stripped traceback + `\n`; marks failure |
| `status` → `idle` | end of this execution |
| per-cell timeout | **interrupt** the kernel (SIGINT, keeps state), drain to idle, `ERROR` "timed out" |
| terminal | `END` with exit_code: `0` ok / `1` error / `-1` timeout |

Plus: lazy kernel start on first execute (cold start paid once), one execution at a
time (serialized — stateful REPL semantics), non-python language → `ERROR` + `END(1)`.
These are ported verbatim from `backend.py`; the bytes on vsock do not change.

### Decision 2 — the kernel stays a Python process; Go drives it

Go launches a stateful Python kernel as a child and speaks the Jupyter messaging
protocol to it (execute_request → iopub messages → our OutputEvents; interrupt for
timeout). This is exactly what `jupyter_client` does for `backend.py` today and what
envd does in E2B — the daemon manages the kernel, it does not *be* the kernel.

### Decision 3 — how Go drives the kernel: WebSocket to a Jupyter Server (chosen — this is what E2B does)

**Chosen: option 1.** E2B's code interpreter runs a **Jupyter Server** inside the
sandbox and drives kernels over its **HTTP + WebSocket** kernels API
(`POST /api/kernels`, WS `/api/kernels/{id}/channels`); envd (Go) handles the
filesystem/process side. So the kernel-driving path closest to E2B is exactly this —
a Go daemon speaking the Jupyter HTTP/WS API to a Jupyter Server. (An earlier draft
called raw ZMQ the "most faithful"; that is wrong about E2B — ZMQ is `jupyter_client`'s
*internal* transport, not how E2B drives kernels.)

1. **(Chosen) Jupyter Server / Kernel Gateway over HTTP/WebSocket.** The Go daemon
   launches a headless Jupyter Server (`jupyter kernelgateway`, 127.0.0.1), creates a
   kernel via `POST /api/kernels`, opens the `/api/kernels/{id}/channels` WebSocket,
   and exchanges **Jupyter messages as JSON** (execute_request on the shell channel;
   stream / execute_result / error / status on iopub); timeout → `POST .../interrupt`.
   - Why: it mirrors E2B's architecture, *and* Go needs only HTTP + WebSocket + JSON
     (no ZMQ, no HMAC signing). The code that matters — translating Jupyter message
     *semantics* into our OutputEvents — is identical to `backend.py._translate`.
   - Cost: one extra Python dep (`jupyter-kernel-gateway`, which pulls in
     ipykernel/jupyter_client) and one extra child process (a small Tornado server).
2. **Raw ZMQ to `ipykernel`.** Closest to the *raw kernel wire protocol* (what
   `jupyter_client` does internally) — but **not** what E2B does, and the heaviest: Go
   would reimplement the kernel protocol (5 ZMQ sockets, multipart frames,
   HMAC-SHA256 signing) plus a pure-Go ZMQ dep. Rejected for this stage.
3. **Thin Python kernel-host sidecar.** Lowest risk, least faithful: Go launches a
   ~50-line `kernelhost.py` reusing today's `backend.py` over JSON-lines. Leaves
   Python on the execute path — not envd-shaped. Rejected.

### Decision 4 — flip the rootfs to the Go binary (7c)

`scripts/build-rootfs.sh` changes: build the daemon (`go build`, `CGO_ENABLED=0`,
`GOOS=linux GOARCH=amd64` → a static binary that runs in the minimal guest), inject
it (e.g. `/usr/local/bin/microsandbox-daemon`), and rewrite `/init` to `exec` it
instead of `python -m microsandbox.server`. The Python *daemon* package
(`server.py`/`backend.py`) is **no longer injected** into the rootfs — but Python +
the kernel deps stay (the kernel is still Python). The `Dockerfile` swaps
`jupyter_client` for `jupyter-kernel-gateway` (option 1). `server.py`/`backend.py`
remain in `src/` and git history as the reference the Go port was built from; the SDK
keeps `client.py` + `protocol.py`.

### Decision 5 — bring loopback up, in Go

The Python daemon raises `lo` via the `SIOCSIFFLAGS` ioctl (the kernel's ZMQ talks
127.0.0.1, and a microVM's `lo` defaults to down). The Go daemon must do the same
before launching the kernel/gateway — ported via `golang.org/x/sys/unix` (or a raw
syscall), guarded to run only on the in-VM (vsock) path, exactly like today.

### Decision 6 — the daemon introduces the project's first external Go deps

`control-plane/` is stdlib-only. The daemon needs what stdlib lacks: a **vsock
listener** (`github.com/mdlayher/vsock` — pure Go; or a ~30-line hand-rolled
`AF_VSOCK` listener) and, for option 1, a **WebSocket client**
(`github.com/coder/websocket`). HTTP serving itself is stdlib `net/http` over the
vsock listener — a real simplification over `server.py`'s hand-rolled HTTP parser.
Each dep is small, pure-Go, and justified by "not in stdlib"; listed here so the
move off stdlib-only is a conscious decision, not drift.

### Decision 7 — testable without KVM, parity proven by the existing suite

- 7a/7b run the daemon's `net/http` mux over an `httptest` server (TCP) — no vsock,
  no VM — so `/health` · `/files/*` · `/commands` and the message-translation logic
  are Go-unit-tested anywhere. The kernel integration (7b) is tested against a real
  kernel/gateway on the **host** (needs Python deps, *not* KVM), gated like a slow
  test.
- 7c's real proof is the **existing Python e2e suite** (`test_microvm` / `test_stateful`
  / `test_files` / `test_sandbox` / `test_microvm_snapshot` / `test_template`): once
  the rootfs runs the Go daemon, those byte-for-byte assertions passing *is* the
  parity proof. They auto-skip without KVM as today.

## 4. Code "from → to" map

| Now (Python, in-VM) | Stage 7 (Go, in-VM) |
|---|---|
| `server.py` vsock listener + hand-rolled HTTP/1.1 | `daemon/` `net/http` over a vsock listener |
| `server.py` `/health` `/files/*` `/commands` handlers | Go handlers (os / os/exec / filesystem) — **7a** |
| `server.py` `_ensure_loopback_up` (ioctl) | Go `lo`-up via x/sys/unix — **7a** |
| `backend.py` `JupyterKernelBackend` (jupyter_client over ZMQ) | Go kernel client (Decision 3) + `_translate` → OutputEvents — **7b** |
| `backend.py` timeout → interrupt → drain; exit codes | ported verbatim — **7b** |
| `scripts/build-rootfs.sh` injects `src/`, `/init` execs `python -m microsandbox.server` | builds + injects the Go binary; `/init` execs it; stops injecting the Python daemon — **7c** |
| `Dockerfile`: `ipykernel + jupyter_client` | `ipykernel + jupyter-kernel-gateway` (option 1) — **7c** |
| `src/microsandbox/server.py`, `backend.py` | retired from the rootfs; kept in `src/` + git history as the reference |

## 5. Go layout (new module)

```
daemon/                # the in-VM daemon (Go), E2B's envd; own go.mod, static binary
  main.go              # flags, vsock listen (or --addr tcp for tests), http.Serve(mux)
  server.go            # mux + handlers: /health /files/* /commands  [7a]
  loopback.go          # bring lo up (x/sys/unix), in-VM only            [7a]
  kernel.go            # launch + drive the Python kernel (Decision 3)   [7b]
  translate.go         # jupyter message -> OutputEvent (port of _translate) [7b]
  *_test.go            # httptest handlers (7a); translate + kernel integ (7b)
```

Built by a new `scripts/build-daemon.sh` (or inline in `build-rootfs.sh`). It is its
own module, separate from `control-plane/` (host side) — they share no code, only the
wire protocol.

## 6. Three independently verifiable sub-steps

### Stage 7a — Go daemon: server + non-execute endpoints

The daemon, everything except `/execute`: vsock/HTTP server, `/health`, `/files/read|write|list`,
`/commands`, and `lo` bring-up. **Not in the rootfs yet** — verified by Go tests over
an `httptest` TCP server (JSON shapes asserted against `protocol.py`'s documented
responses). The Python daemon still runs in the VM, so the whole existing suite is
untouched.

### Stage 7b — `/execute`: Go drives the Python kernel

The hard part: launch the stateful kernel (Decision 3), translate its messages into
the SSE OutputEvent stream, with lazy start, serialization, timeout→interrupt, and the
exit-code rules — byte-identical to `backend.py`. Translation is unit-tested (pure
function); the live kernel path is an integration test on the host (Python deps, no
KVM). Still not in the rootfs.

### Stage 7c — flip the rootfs to the Go daemon

`build-rootfs.sh` builds + injects the binary and rewrites `/init`; `Dockerfile` swaps
in the kernel gateway; stop injecting the Python daemon; rebuild the snapshot. Now the
**existing Python e2e suite runs against the Go daemon** — green = byte-for-byte parity.
The SDK, protocol, and control plane are untouched throughout.

## 7. Keeping tests green (honest trade-offs)

- **7a/7b add only Go tests** (httptest handlers + translation unit tests), KVM-free,
  so `go test ./daemon` is green anywhere — mirroring `control-plane/`'s KVM-free
  units. The Python suite is untouched until 7c because the rootfs still runs Python.
- **7b's live-kernel integration test needs Python + the kernel gateway** (not KVM).
  It is gated/skips when those are absent, like the VM tests gate on KVM.
- **7c is the risky flip**: until then the daemon isn't exercised in a real VM here
  (`/dev/kvm` is root:root on this host), so 7c's parity rests on the existing e2e
  suite, which must be run on a KVM terminal. The byte-stable protocol is what makes
  this swap safe — same discipline as every prior stage.
- **New Go deps** (Decision 6) and a **Dockerfile dep swap** (Decision 4) are called
  out so they aren't mistaken for drift; the daemon module stays minimal.
- After 7c lands: update `CLAUDE.md` (the daemon is Go now; the rootfs/`/init` notes;
  `go test ./daemon`), `docs/ARCHITECTURE.md` (the E2B mapping: `server.py` → Go
  daemon), and the README.
```
