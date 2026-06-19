# Stage 11 design: `envd` → E2B form (ConnectRPC `Process`/`Filesystem` + a separate `code-interpreter`)

> Status: **agreed direction.** The first of the roadmap's *deferred* stages, and the
> **largest and most invasive** so far. It rewrites the in-VM daemon from hand-rolled
> HTTP/SSE into **ConnectRPC** services, matching E2B's split of `envd` (Process +
> Filesystem) and a separate `code-interpreter` (the Jupyter kernel). Read
> `docs/STAGE7_DESIGN.md` (the current daemon), `docs/ARCHITECTURE.md`, and
> `docs/E2B_ALIGNMENT_ROADMAP.md` first. Three sub-steps (11a → 11b → 11c).
>
> **⚠ This stage deliberately ends the project's byte-stable-protocol discipline.** Stages
> 0–10 kept `protocol.py`'s wire bytes fixed so the e2e suite was a *byte-for-byte* parity
> oracle. Stage 11 replaces that protocol with ConnectRPC, so the oracle becomes
> *behavioral* parity (the suite is ported to the new SDK and must still pass, but the bytes
> change). This is called out everywhere it matters; it is the conscious cost of reaching
> E2B's real in-VM shape.

## 1. Goal & non-goals

**Goal.** Make the in-VM layer match E2B's:

- **`envd`** — ConnectRPC `FilesystemService` (Read/Write/List) + `ProcessService` (Run),
  plus a plain-HTTP `/health`. Replaces today's `/files/*` and `/commands` JSON endpoints.
- **`code-interpreter`** — a separate ConnectRPC service on its **own vsock port**, owning
  the stateful Jupyter kernel: `Execute` is **server-streaming** (a stream of `OutputEvent`s).
  This is today's `/execute` (SSE) + `kernel.go` + `translate.go`, moved out of envd.
- **SDK** — speaks ConnectRPC to both: `run_code` → code-interpreter's streaming `Execute`;
  `files`/`commands` → envd's unary RPCs. The user-facing `Sandbox` surface is unchanged.

E2B runs `envd` on `:49983` and `code-interpreter` on `:49999`; we map those two ports onto
two **vsock** ports and route to them through the existing two-hop proxy.

**Non-goals** (bounded out / single-machine simplifications):

- **No HTTP/2 or gRPC.** We use the **Connect protocol over HTTP/1.1** (Decision 1) so it
  rides the existing vsock bridge unchanged. gRPC's HTTP/2 + trailers are out of scope.
- **No streaming `Process`.** E2B's `Process` is `Start` + a streamed stdout/stderr; our
  `Run` stays unary run-to-completion (today's `/commands` behavior). Streaming processes,
  PTYs, signals: later.
- **No TAP networking** (Stage 12): the two services are reached over **vsock**, not
  `<port>-<id>` TCP hostnames. **No auth.** **No protobuf runtime in the Python SDK** — it
  hand-rolls the Connect protocol with the JSON codec (Decision 2), staying stdlib-only.
- **`protocol.py` is retired to reference** (like `server.py`/`backend.py` were in Stage 7),
  not deleted, so the prior protocol stays legible.

## 2. Target architecture (Stage 11 end state)

```
 SDK (hand-rolled Connect-JSON over HTTP/1.1)
  │  /envd.FilesystemService/* , /envd.ProcessService/*  ─────────────┐
  │  /codeinterpreter.CodeInterpreterService/Execute (server-stream) ─┤
  ▼                                                                    │
 client-proxy ──(X-Sandbox-Id)──▶ orchestrator data proxy ──route by ConnectRPC path prefix──▶ VM
                                  • /codeinterpreter.* → vsock port B (code-interpreter)
                                  • everything else    → vsock port A (envd)
                                                                       │
   VM (one daemon binary, two vsock listeners):                        │
     • envd            (port A = 1024): FilesystemService + ProcessService + plain GET /health
     • code-interpreter(port B = 1025): CodeInterpreterService.Execute (streams), drives the kernel
```

`envd` keeps `/health` on port A, so the orchestrator's "ready on delivery" probe
(`proxy.WaitHealthy` on `fc.VsockPort`) is unchanged. The kernel's cold start stays lazy
(first `Execute`); the warm snapshot warms it via port B (Decision 4).

## 3. Key design decisions

### Decision 1 — the **Connect protocol over HTTP/1.1** (not gRPC)

Our vsock bridge (`services/pkg/proxy`) is a hand-rolled HTTP/1.1 round-tripper behind an
`httputil.ReverseProxy`. gRPC needs HTTP/2 + trailers, which the bridge does not speak. The
**Connect protocol** runs over HTTP/1.1:

- **Unary** = an ordinary `POST /<pkg>.<Service>/<Method>` with the message as the body.
- **Server-streaming** = a body of *enveloped frames* — `[1 flag byte][4-byte big-endian
  length][payload]` — with the final frame's flag bit `0x02` marking an `EndStreamResponse`
  (trailers / error). With `FlushInterval: -1` (already set on the proxies) the frames flush
  live — the same pipe today's SSE flows through.

So `code-interpreter`'s streamed `Execute` reaches the SDK exactly as `/execute`'s SSE does
now; only the framing changes. **This is the linchpin that lets ConnectRPC ride the existing
vsock two-hop proxy with no transport rewrite.**

### Decision 2 — server uses **connect-go**; the SDK **hand-rolls Connect-JSON** (no new Python dep)

- **Go (`daemon/`):** `connectrpc.com/connect` (v1.20) + stubs from the `.proto` via
  `protoc-gen-connect-go` (installed alongside the existing `protoc-gen-go`). The server side
  uses the real, tested library.
- **Python SDK:** stays stdlib-only. It hand-rolls a tiny Connect **JSON-codec** client over
  `urllib`: unary is a `POST` of a JSON body; server-streaming is a loop reading the 5-byte
  envelope + JSON payload (and the final `0x02` end frame) — structurally the same as today's
  hand-parsed SSE loop. Connect's JSON encoding means **no protobuf runtime, no grpcio**.

The pedagogy is nice: the server uses the standard library, the client lays the wire protocol
bare by hand — exactly the spirit of this repo.

### Decision 3 — the `.proto` shapes mirror today's JSON (behavior unchanged; only the framing moves)

`daemon/proto/envd/envd.proto` and `daemon/proto/codeinterpreter/codeinterpreter.proto`:

```proto
// envd
service FilesystemService {
  rpc Read (ReadRequest)  returns (ReadResponse);   // {path} -> {content}
  rpc Write(WriteRequest) returns (WriteResponse);  // {path, content} -> {}
  rpc List (ListRequest)  returns (ListResponse);   // {path} -> repeated Entry{name, is_dir}
}
service ProcessService {
  rpc Run(RunRequest) returns (RunResponse);        // {command, timeout_seconds} -> {stdout, stderr, exit_code}
}
// code-interpreter
service CodeInterpreterService {
  rpc Execute(ExecuteRequest) returns (stream OutputEvent); // {code, language, timeout_seconds} -> stream {type, data, exit_code}
}
```

The field semantics are exactly today's (`server.go`, `protocol.py`), so each handler is the
current logic with the HTTP plumbing swapped for a Connect handler — same files written, same
commands run, same kernel translation (`translate.go`).

### Decision 4 — `code-interpreter` on a **separate vsock port**; the orchestrator routes by path prefix

- `envd` keeps **vsock port A** = `fc.VsockPort` (1024): Filesystem + Process + `/health`.
  `code-interpreter` gets **vsock port B** = a new `fc.CodeInterpreterVsockPort` (1025).
- **One daemon binary, two vsock listeners** (a bounded simplification of E2B's two
  processes): `/init` still execs the single binary; `build-rootfs.sh` is unchanged. The
  binary serves the envd services on A and the code-interpreter service on B.
- **`orchestrator/dataproxy.go` routes by the ConnectRPC path prefix**: a request whose path
  starts with `/codeinterpreter.` bridges to port B; everything else to port A. `client-proxy`
  is unchanged (still routes by `X-Sandbox-Id`, forwards the path verbatim) — the per-service
  vsock-port choice is the orchestrator's job, since it owns the VM's vsock.
- **`build-snapshot.sh`'s kernel warm-up moves to port B + a Connect `Execute` call** (the
  kernel now lives there). This is the one build-script change the split forces.

### Decision 5 — the SDK swaps to Connect clients; `protocol.py` retires

`run_code` calls `CodeInterpreterService.Execute` (streaming) and aggregates `OutputEvent`s
into the same `Execution` it returns today; `files`/`commands` call the envd unary RPCs.
`Sandbox.run_code / files / commands` signatures are unchanged. `protocol.py` becomes
reference (the new message shapes live as small Python dataclasses in the SDK + the `.proto`).

### Decision 6 — the parity oracle changes from byte-stable to behavioral (the discipline reversal)

Every prior stage proved "topology changed, behavior didn't" by an *unchanged* e2e suite over
a byte-stable protocol. Stage 11 changes the protocol, so:

- The e2e suite is **ported to the new SDK** and must pass on **behavior** (same `Execution`
  results, same file/command semantics, same timeout/interrupt behavior).
- `docs/ARCHITECTURE.md` + `CLAUDE.md` must state plainly that the byte-stable-protocol
  invariant ended at Stage 11, replaced by behavioral parity — so future work doesn't assume a
  guarantee that no longer holds.

## 4. Code "from → to" map

| Now (Stage 7 daemon) | Stage 11 |
|---|---|
| `daemon/` one vsock port (1024), `net/http` mux: `/health`, `/execute` (SSE), `/files/*`, `/commands` | two vsock listeners: envd (A) Connect Filesystem+Process + plain `/health`; code-interpreter (B) Connect streaming `Execute` |
| `daemon/kernel.go` + `translate.go` (inside envd) | moved into the code-interpreter service (port B) |
| `daemon/server.go` HTTP handlers | Connect handlers (Filesystem/Process); the file/command logic is unchanged |
| `daemon/protocol.go` (SSE) | retired; replaced by generated Connect stubs + the `.proto` |
| `src/microsandbox/protocol.py` | retired to reference |
| `src/microsandbox/client.py` urllib + SSE parse | a hand-rolled Connect-JSON client (unary + enveloped streaming) |
| `services/cmd/orchestrator/dataproxy.go` fixed `fc.VsockPort` | route by `/codeinterpreter.` prefix → port B, else port A |
| `services/pkg/fc` `VsockPort` | + `CodeInterpreterVsockPort` |
| `scripts/build-snapshot.sh` warm-up (`POST /execute` on 1024) | a Connect `Execute` on port B |
| `scripts/gen-proto.sh` | also generates the daemon's Connect stubs |
| e2e: **byte-stable** parity oracle | **behavioral** parity oracle (ported SDK) |
| `client-proxy`, the store/catalog, `daemon/`'s loopback bring-up, `pkg/template` | **unchanged** |

## 5. Layout introduced this stage

```
daemon/
  proto/
    envd/envd.proto                       # FilesystemService + ProcessService
    codeinterpreter/codeinterpreter.proto # CodeInterpreterService (streaming Execute)
  genpb/                                   # generated *.pb.go + *.connect.go (committed)
    envd/  codeinterpreter/
  envd.go            # the envd Connect services (Filesystem/Process) + /health, on port A
  codeinterpreter.go # the code-interpreter Connect service, on port B (kernel.go/translate.go move under it)
  main.go            # two vsock listeners (A + B); --addr stays for TCP dev/test
src/microsandbox/
  connect.py         # the hand-rolled Connect-JSON client (unary + enveloped streaming)
  client.py          # run_code/files/commands now call connect.py
```

## 6. Three independently verifiable sub-steps

### Stage 11a — protos + connect-go codegen; stand up envd's Connect surface **alongside** the HTTP endpoints
Add `envd.proto`/`codeinterpreter.proto` + extend `gen-proto.sh` to generate the daemon's
Connect stubs (and add `connectrpc.com/connect` to `daemon/go.mod`). Implement
`FilesystemService` + `ProcessService` as Connect handlers, **mounted on the existing vsock
port 1024 alongside the current HTTP mux** — nothing routes to them yet, the HTTP endpoints
stay. Go unit tests drive the Connect handlers directly. **Verify:** `go test ./daemon` green;
rebuild the rootfs and run the full e2e (still over the old HTTP path) green — proving the
connect-go-linked daemon still boots and serves the old protocol. Purely additive.

### Stage 11b — `code-interpreter` on its own vsock port; route to it; flip `run_code` to Connect streaming
Move `kernel.go`/`translate.go` into a `CodeInterpreterService` served on vsock **port B**;
the daemon opens the second listener. Route `/codeinterpreter.*` to port B in the orchestrator
data proxy (+ `fc.CodeInterpreterVsockPort`). Flip the SDK's `run_code` to the streaming
Connect client; move `build-snapshot.sh`'s warm-up to port B. **Verify:** the e2e `run_code`
/ stateful / snapshot cases pass over Connect streaming — the **first** break from byte-stable
to behavioral parity. (This is the riskiest step: streaming Connect over the vsock two-hop
proxy. Validate it early.)

### Stage 11c — flip `files`/`commands` to envd Connect; remove the HTTP endpoints; retire `protocol.py`
Point the SDK's `files`/`commands` at envd's unary RPCs; delete the daemon's `/execute`,
`/files/*`, `/commands` HTTP handlers (keep `/health`); retire `protocol.py` to reference.
**Verify:** the whole e2e suite passes behaviorally on the new SDK. Update `CLAUDE.md`,
`docs/ARCHITECTURE.md`, the README, and `E2B_ALIGNMENT_ROADMAP.md` — and **record the end of
the byte-stable-protocol discipline** (Decision 6).

## 7. Keeping tests green (honest trade-offs)

- **11a is additive and proven by the unchanged HTTP e2e** (the new Connect surface is just
  mounted, not used) — the safe way to absorb the big new dependency + codegen first.
- **The byte-stable oracle ends at 11b** (Decision 6); from there the e2e proves *behavior*.
  This is the single biggest departure from the project's discipline and is documented as
  such, not slipped in.
- **New dependencies, called out:** Go gains `connectrpc.com/connect` + `protoc-gen-connect-go`
  (the whole point of the stage). The Python SDK gains **nothing** — it hand-rolls Connect-JSON
  over `urllib`, consistent with the repo's stdlib-only client.
- **Rootfs rebuild every step:** the daemon changes, so each sub-step needs
  `scripts/build-rootfs.sh` (+ `build-snapshot.sh` for 11b's warm-up) before the real-VM e2e —
  the e2e fixture only *builds the rootfs if absent*, so a stale rootfs would silently test the
  old daemon.
- **Streaming-over-vsock is the key risk** (11b): connect-go server-streaming over HTTP/1.1,
  flushed through the vsock round-tripper. It is the same mechanism SSE uses today, but it is
  the thing to validate first in 11b before building the rest on it.
- **Safety note carried forward:** still a learning implementation, not security-audited; the
  sandbox stays offline (vsock only). Adding ConnectRPC does not change the isolation.
