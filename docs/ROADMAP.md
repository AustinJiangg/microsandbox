# Roadmap (ROADMAP)

Each stage lists: **learning goal**, **what to do**, **acceptance criteria**.
The recommendation is to check off each item here as you finish it, and keep the "You are here" marker at the top of `CLAUDE.md` in sync.

---

## ✅ Stage 0: Process-level skeleton (done)

**Learning goal**: Understand the RPC / streaming communication model between client↔daemon, and stand up the three-layer skeleton.

- [x] Define the protocol (ExecuteRequest / OutputEvent / Execution)
- [x] LocalSubprocessBackend: subprocess execution + timeout + stdout/stderr separation
- [x] daemon: HTTP + SSE streaming
- [x] client SDK: run_code + streaming callback + auto start/stop of the daemon
- [x] Tests and examples

**Acceptance criteria**: `python examples/quickstart.py` runs end to end; `pytest` is all green.

---

## ✅ Stage 1: Docker container isolation (done)

**Learning goal**: The first real isolation. Understand filesystem/network isolation, cgroups resource limits, and the container lifecycle.

- [x] Add `DockerBackend(ExecutionBackend)`: drive the `docker` CLI directly via asyncio subprocesses
      (no docker-py—to keep zero runtime dependencies, and because docker-py is a synchronous library that would only get more convoluted inside an asyncio daemon).
- [x] Each execution maps to a single throwaway container (per-Sandbox container reuse is the stateful work in Stage 2):
   - Image: the official `python:3.12-slim`; the execution path uses `--pull never`, so you must `docker pull` it beforehand.
   - Limits: `--memory`/`--memory-swap`, `--cpus`, `--network none`, `--pids-limit`,
     `--read-only` + `--tmpfs /tmp` (read-only root + a temporary writable layer).
   - Execution: code is passed via argv (`python -u -c <code>`, structurally the same as Stage 0; the ~2MB argv limit is enough for Stage 1).
- [x] The daemon gets a `--backend {local,docker}` switch + a startup-time Docker availability check;
      the client gets `Sandbox(backend=...)` to pass it through. **The daemon still lives on the host in this stage**—
      "move the daemon into the container" is Stage 2's job (corresponding to E2B's envd); what this stage validates is precisely
      "swapping isolation = swapping the backend, while the client and protocol stay untouched".
- [x] Timeout and cleanup: on timeout, `docker rm -f` kills the container (killing the docker run client process does not kill the container!),
      with an idempotent fallback in `finally`; containers are uniformly named `microsandbox-exec-*` to make fallback cleanup easy.

**Acceptance criteria (met)**: the original 7 tests pass under parametrization across both the local/docker backends (7×2);
plus 4 new isolation tests: host files invisible, no network, read-only root + writable /tmp, and no leftover container after a timeout.

**Note**: container isolation is still not enough to run fully untrusted code (the container escape surface is fairly large); the docs must say so honestly.

---

## ✅ Stage 2: Resident agent inside the container + stateful REPL (done)

**Learning goal**: Align with E2B's core architecture—a resident agent (envd) lives inside the sandbox and supports preserving state across calls.

**The core is one "ownership inversion"**: the daemon moves out of the host and into a **long-lived** container where it stays resident,
and the responsibility of "creating the isolated environment" moves up from the backend to the client. See `docs/STAGE2_DESIGN.md` for the detailed design.

Split into three sub-steps, each keeping the existing tests all green:

- [x] **2a envd-ification (relocation, state not yet preserved)**
   - No image build during development: mount the host's `src/` read-only into `python:3.12-slim` and
     run `python -m microsandbox.server` inside the container.
   - `client` adds `backend="container"`: `docker run -d` to start a resident container, map the port,
     do a health probe, and `docker rm -f` on `close`. The in-container daemon still uses the stateless
     `LocalSubprocessBackend`. **Not a single line of `server.py` changes** (it already supports `--host/--port/--backend`).
   - Acceptance met: the end-to-end cases pass under parametrization across the three topologies `local`/`docker`/`container` (7×3).
- [x] **2b stateful REPL** — the persistent interpreter uses a **Jupyter / IPython kernel** (aligned with E2B).
   - Add `JupyterKernelBackend`: the daemon hosts a resident kernel, speaks the Jupyter messaging protocol over ZMQ, and
     translates iopub's stream/execute_result/error/idle back into `OutputEvent`s; the `/execute` protocol is unchanged.
   - **First introduction of runtime dependencies**: `ipykernel` + `jupyter_client` (a `[kernel]` optional extra,
     lazily imported inside the backend); a new `Dockerfile` builds the agent image; `client` adds `backend="kernel"`.
   - Timeout uses **interrupt (SIGINT) rather than killing the process**: it aborts the current cell but the kernel and its namespace survive.
   - Acceptance met: variables/functions/imports persist across `run_code`; after a timeout the kernel does not die and old variables are still usable.
- [x] **2c file / shell API** — `protocol.py` gets a **backward-compatible addition** of `/files/{read,write,list}` and
   `/commands` (`/execute` and the existing dataclasses are untouched); the client adds `sandbox.files.*` /
   `sandbox.commands.*`, matching the E2B feel.
   - Key design: files/commands are handled **by the daemon directly on its own FS, without going through the ExecutionBackend**—
     aligned with E2B's envd (the file/process services are separate from the kernel that runs code).
   - Acceptance met: write-then-read round-trip, files visible to `run_code`, directory listing, and `commands.run` returning shell output
     (including a non-zero exit code). The resident container has a `--read-only` root, writes are limited to `/tmp`, and writing elsewhere reports an honest error.

**Note (network/security weakening)**: a Stage 2 container has to open a management port for the client, so it **can no longer use `--network none`**,
which in turn opens up the in-container code's outbound network—on this point the **isolation is actually weaker than Stage 1**. Strong isolation still has to wait for Stage 3.

**Acceptance criteria**: across two consecutive `run_code` calls, the second can use a variable defined by the first; the file API round-trips successfully; `pytest` is all green throughout.

---

## ✅ Stage 3: Firecracker microVM isolation (done)

**Learning goal**: Understand the principles of strong isolation, microVMs, vsock communication, and how snapshots achieve millisecond-scale cold starts. **Slow down and understand this stage by hand—don't run on vibes alone.**

See `docs/STAGE3_DESIGN.md` for the detailed design and measured records. Split into 3a/3b/3c:

- [x] **3a**: vsock transport abstraction—client/server factor out a `Transport` layer, with the TCP path byte-for-byte unchanged
      (existing tests pass with no edits), plus a new `_VsockTransport` (CONNECT handshake + minimal HTTP over a raw socket).
- [x] **3b**: export a rootfs from the Docker image (`mkfs.ext4 -d`, no root needed) + launch a Firecracker microVM
      (declaratively via `--config-file`, not REST), put the Stage 2 agent into the rootfs, and have the daemon listen on vsock instead of
      TCP; end-to-end `run_code`, with the in-VM kernel backend stateful; resource limits go through machine-config (vCPU/memory).
- [x] **3c**: cold start ~0.94s recorded, resource-limit tests added; **the stretch goal is done**—Firecracker snapshot restore gives
      millisecond-scale cold starts (~30ms to ready, 10× to first result, see STAGE3_DESIGN §9). The warm pool belongs to Stage 4.

**Acceptance criteria (met)**: able to run code inside a microVM and get the result back ✅; cold start time measured and recorded ✅
(cold start ~0.94s, snapshot restore ~30ms).

---

## ⬜ Stage 4: Productization periphery (pick by interest)

**Learning goal**: Turn "a sandbox" into "a sandbox service".

Candidates:
- [ ] Control-plane API: `POST /sandboxes` to create, `DELETE` to destroy, plus list.
- [ ] Sandbox pool warm-up to reduce cold start.
- [ ] Custom templates/images (with dependencies pre-installed).
- [ ] Authentication and quotas, timeout-based reclamation, usage statistics.
- [ ] Multi-language backends (node, bash).

---

## Suggested comparative study

After finishing Stage 2, go back and read the `envd` and orchestration parts of the E2B source—it will resonate strongly.
Skip the SDK's multi-language bindings, the dashboard, and billing—those are productization periphery, not the core mechanism.
