# Stage 2 Design: Resident agent inside the container + stateful REPL

> This document is the design and implementation plan for Stage 2. Stage 2 is the **most invasive** stage of the whole project—
> it turns the "isolated environment" from "created fresh on each execution" into "long-lived at the sandbox level",
> corresponding to E2B's core architecture, `envd`. We recommend reading it alongside `docs/ARCHITECTURE.md`.

---

## 1. What this stage solves: one "ownership inversion"

The fundamental difference between Stage 0/1 and Stage 2 is **not "we swapped a backend"**, but that the ownership relationship between the daemon and
the container is completely flipped.

**Stage 1 (current)—the host daemon is in charge, the container is a throwaway laborer:**

```
host
┌─────────────────────────────────────────────┐
│  client (Popen starts daemon)                 │
│     │                                          │
│     ▼                                          │
│  daemon (server.py, resident on host)          │
│     │ each run_code                             │
│     ▼                                          │
│  DockerBackend.execute()                       │
│     └── docker run a throwaway container ─► dies when done │
│         (--rm, --network none)                 │
└─────────────────────────────────────────────┘
```

- The daemon is on the **host** (`client.py`'s `_spawn_daemon` uses `subprocess.Popen`).
- Each `run_code` → `backend.py`'s `DockerBackend.execute` **spins up a throwaway container**,
  which is deleted as soon as it finishes (`--rm`). The container uses `--network none` and is fully cut off, because it has no one to talk to.

**Stage 2 (E2B's envd model)—the container is in charge, the daemon moves in and stays resident:**

```
host                            sandbox container (long-lived, one per Sandbox)
┌──────────────┐               ┌──────────────────────────────────┐
│ client       │  HTTP/SSE     │  daemon (server.py) ← this is envd │
│  docker run  │ ────────────► │     │                              │
│  -d once      │  maps port    │     ▼                              │
│  connects in  │ ◄──────────── │  PersistentBackend (persistent interp.) │
│  docker rm -f│               │     └── variables persist across run_code │
└──────────────┘               └──────────────────────────────────┘
```

- **The container is created once and is long-lived**: one `Sandbox` maps to one container, no longer "one per execution".
- **The whole `server.py` moves into the container**, running resident as the container entrypoint—this is E2B's `envd`.
- **Who creates the container?** It can no longer be the backend—the daemon now lives inside the container and can't create
  the very container it lives in (chicken and egg). So **the responsibility of creating the container moves up to the client**.

In one sentence:

> **Stage 2 = move `server.py` into the container as-is, and shift the work of "creating the isolated environment" from the backend to the client.**

`server.py`'s `SandboxServer` barely needs to change—this is the most elegant validation yet of the
"stable protocol, swappable isolation" main thread.

---

## 2. Responsibility migration table (read it against the real code to see what changed)

| Code location | What Stage 1 does now | What Stage 2 changes it to |
|----------|------------------|----------------|
| `client.py:_spawn_daemon` | host `subprocess.Popen` starts the daemon | `docker run -d` a resident container, running the daemon inside it |
| `client.py:_wait_until_healthy` | poll `/health`, on failure read Popen's stderr | poll `/health`, on failure read `docker logs` |
| `client.py:close` | `proc.terminate()` | `docker rm -f <container name>` |
| `server.py` | listens on `127.0.0.1`, runs on the host | **code unchanged**; in the container, start with `--host 0.0.0.0` so the host can connect to it |
| `backend.py` | `DockerBackend` spins up a throwaway container each time | add a persistent-interpreter backend that runs inside the container (see §4) |
| `protocol.py` | a single `/execute` endpoint | **backward-compatible addition** of `/files/*`, `/commands` (see 2c in §4) |

**Key observation**: Stage 2 barely touches the existing parts of `server.py` and `protocol.py`.
The changes concentrate in the client (which takes over the container lifecycle) and the backend (swapped to a persistent interpreter).
This is the dividend of the three-layer decoupling.

---

## 3. Split into three sub-steps (each keeps the existing tests all green)

Stage 2 is too big, so split it into three independently verifiable sub-steps, in line with the "one concept per step" cadence:

### 2a — envd-ification (relocation, state not yet preserved) ← this round

Move the daemon into a resident container, proving "the daemon code didn't change, it just runs somewhere else".

- No image build during development: directly mount the host's `src/` **read-only** into `python:3.12-slim`,
  set `PYTHONPATH`, then `python -m microsandbox.server`. Zero dependencies, no rebuild on code changes.
- `client.py` adds `backend="container"`: `docker run -d` to start a resident container, map the port,
  do a health probe, and `docker rm -f` on `close`.
- The in-container daemon still uses `LocalSubprocessBackend` (one subprocess per exec)—
  **it is safe again now**, because the whole daemon is already inside the container. State is not yet preserved,
  but the "inversion" architectural argument already holds.
- **Not a single line of `server.py` needs to change** (it already supports `--host/--port/--backend`),
  which is exactly what 2a wants to prove.

**Acceptance**: `backend="container"` runs `run_code` end to end; the existing 7 end-to-end cases pass under parametrization across the
three topologies `local`/`docker`/`container` (7×3).

### 2b — stateful REPL (this is the step that makes a "real REPL")

Replace "fork a subprocess each time" with a **persistent interpreter**, so variables persist across `run_code`.

- **Mechanism: a Jupyter / IPython kernel** (decision in §5). The daemon hosts a resident
  Jupyter kernel; `run_code` sends an `execute_request` over ZMQ and collects
  stdout/stderr/execute_result/error from iopub, mapping them back to our existing `OutputEvent`.
- This step **introduces dependencies**: `ipykernel` + `jupyter_client`. That is why 2b introduces a
  **Dockerfile** (pip-install these two into the agent image), and the in-container daemon starts with
  `--backend kernel`.
- Timeout/crash semantics: if the kernel hangs, interrupt it (SIGINT) or restart the kernel (state is lost, but
  the daemon and container survive). It must keep the contract of the existing `test_timeout` (the error contains "timed out").

**Acceptance**: across two consecutive `run_code` calls, the second can use a variable defined by the first.

### 2c — file / shell API

Give the sandbox the ability to "read/write files" and "run shell commands", matching the feel of E2B's
`sandbox.files` / `sandbox.commands`.

- `protocol.py` gets a **backward-compatible addition** of `/files/{read,write,list}` and `/commands`;
  `/execute` and the existing dataclasses are left untouched.
- `client.py` adds two namespaces, `sandbox.files.*` and `sandbox.commands.*`.

**Acceptance**: write a file → read it back, round-trip succeeds; `sandbox.commands.run("ls /tmp")` returns output.

---

## 4. Protocol evolution principle (2c)

`protocol.py` is the most important stable boundary in the whole project. Stage 2's protocol evolution is **additive only, never mutating**:

- Not a single field of the existing `ExecuteRequest` / `OutputEvent` / `Execution` changes.
- The newly added file/shell features use **new endpoints + new dataclasses**, which old clients can't see and aren't affected by.
- This way the old tests need no changes to stay all green—the protocol's backward compatibility is proven, backed by the tests.

---

## 5. Key decision: 2b uses a Jupyter kernel (rather than a homegrown worker)

There are three paths to a persistent interpreter; this project chooses a **Jupyter / IPython kernel**:

| Option | Trade-off |
|------|------|
| **Jupyter / IPython kernel ✅ chosen** | Closest to E2B (E2B's code interpreter is a Jupyter kernel); rich output and kernel restart work out of the box. Cost: introduces the heavy `ipykernel`+`jupyter_client` dependencies, and the kernel internals are a black box. |
| Homegrown resident worker | Zero dependencies, build a mini-kernel by hand, readable line by line. Cost: you have to handle output framing/timeout/crash recovery yourself, which means more code. |
| In-process `exec()` | The least code. Cost: a timeout can't kill the thread, and crashing code drags down the whole daemon. The simplest but most fragile; not used. |

> Why choose Jupyter: Stage 2's learning goal explicitly says "align with E2B's core architecture",
> so using E2B's same Jupyter kernel means that when you later go read E2B's `envd` source, it resonates directly.
> This is the **first time we introduce a dependency because a stage needs it** after the "zero dependencies" of Stage 0/1, in line with the development conventions.

---

## 6. Network and security trade-offs (record them honestly)

A Stage 2 container has to open a **management port** for the client (otherwise you can't connect to the daemon inside),
so it **can no longer use `--network none`**. Consequences:

- The **outbound network of the in-container code is opened up along with it**—on this point the **isolation is actually weaker than Stage 1**
  (in Stage 1 each execution container is fully cut off from the network).
- "Let code reach the network while keeping a management channel" is more fine-grained work (a separate internal network / firewall rules),
  which this project defers to Stage 3/4.

**Safety rule (consistent with CLAUDE.md)**: Stage 2's isolation is **still not enough** to run fully untrusted
code; it is **strictly forbidden** to offer it as a service or accept arbitrary input. Kernel-level strong isolation starts at Stage 3 (microVM).
Both the docs and the code comments must state this honestly.

The isolation measures that remain (container-level, applied to the whole sandbox rather than a single execution):
`--memory` / `--cpus` / `--pids-limit` / `--read-only` + `--tmpfs /tmp`.

---

## 7. Compatibility: Stage 0/1 are not removed, Stage 2 is a "new topology"

Stage 2 does not replace `backend="local"/"docker"`; instead it **adds** the resident-container topology alongside them.
Benefits: the existing 18 tests stay all green as-is (the safety net is untouched), and you can study old vs. new side by side.

The values of `Sandbox(backend=...)` evolve into a single "complete strategy" enum:

| `backend` value | topology | in-container execution backend | corresponding stage |
|----------------|------|----------------|----------|
| `"local"` | host daemon | local subprocess (no isolation) | 0 |
| `"docker"` | host daemon | a throwaway container per execution | 1 |
| `"container"` | **resident container**, daemon inside | in-container subprocess (stateless) | **2a** |
| `"kernel"` | **resident container**, daemon inside | Jupyter kernel (stateful) | 2b (final state) |

`"container"` and `"kernel"` share the client-side "start a resident container" code; the only difference is
which `--backend` the in-container daemon is told to use (`container`→`local`, `kernel`→`kernel`).

---

## 8. Stage 2 overall acceptance criteria

- [x] 2a: `backend="container"` works end to end; the end-to-end cases pass under parametrization across the three topologies (7×3).
- [x] 2b: across two consecutive `run_code` calls, the second can use a variable defined by the first (after a timeout interrupt the kernel survives and old variables are still usable).
- [x] 2c: file read/write round-trips, files are visible to `run_code`, directory listing works, `commands.run` returns shell output (including a non-zero exit code).
- [x] `pytest` is all green throughout (41 items); the docs honestly state Stage 2's network/security weakening and the "only /tmp is writable" constraint.

**Stage 2 is fully done.** Files/commands use a daemon-level implementation (not going through the ExecutionBackend), aligned with E2B's
envd design of separating the file/process services from the code kernel; the next step enters Stage 3 (Firecracker microVM).
