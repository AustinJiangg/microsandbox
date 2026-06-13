"""Execution backend.

[This is the part of the project that will be replaced again and again.]

Stage 0: execute via a local subprocess -- almost no isolation, just enough to
    get the skeleton working.
Stage 1 (current): execute in a Docker container -- the first real isolation.
Stage 2: turn the execution logic into a long-lived agent inside the container.
Stage 3: switch to a Firecracker microVM.

So that future replacements don't ripple into the rest of the code, we define an
abstract base class `ExecutionBackend` here, and every other module depends only
on this interface. Later stages just add implementations like `DockerBackend` or
`FirecrackerBackend`; the server code stays untouched.
"""

from __future__ import annotations

import asyncio
import re
import shutil
import subprocess
import sys
import uuid
from abc import ABC, abstractmethod
from collections.abc import AsyncIterator

from .protocol import EventType, ExecuteRequest, OutputEvent


class ExecutionBackend(ABC):
    """Abstract interface for an execution backend. Every isolation scheme implements it."""

    @abstractmethod
    def execute(self, request: ExecuteRequest) -> AsyncIterator[OutputEvent]:
        """Execute a piece of code, producing output events as an async stream.

        Note the return type is AsyncIterator: the caller consumes it with
        `async for`, giving streaming output that "returns as it runs" rather
        than waiting for everything to finish.
        """
        raise NotImplementedError


async def _merge_output_streams(
    proc: asyncio.subprocess.Process,
) -> AsyncIterator[OutputEvent]:
    """Merge a subprocess's two streams (stdout / stderr) into one event stream, in arrival order.

    Why factor this out into a shared function: comparing LocalSubprocessBackend
    and DockerBackend, the difference between backends is only in "how to start,
    how to kill, how to clean up" -- whereas "how to interleave the two output
    streams and emit them streaming" is exactly the same. Factoring it out makes
    that boundary stand out.

    Implementation: spin up one pump task each for stdout / stderr, both pushing
    events into the same queue (a queue is what lets them interleave in arrival
    order, instead of all of stdout followed by all of stderr); each side pushes
    a None sentinel on EOF, and the main loop ends once it has counted two
    sentinels.
    """
    queue: asyncio.Queue[OutputEvent | None] = asyncio.Queue()

    async def pump(stream: asyncio.StreamReader, etype: EventType) -> None:
        while True:
            line = await stream.readline()
            if not line:
                break
            await queue.put(OutputEvent(type=etype, data=line.decode(errors="replace")))
        await queue.put(None)  # sentinel: signals this side is done reading

    assert proc.stdout is not None and proc.stderr is not None
    pumps = [
        asyncio.create_task(pump(proc.stdout, EventType.STDOUT)),
        asyncio.create_task(pump(proc.stderr, EventType.STDERR)),
    ]

    finished = 0
    try:
        while finished < len(pumps):
            event = await queue.get()
            if event is None:
                finished += 1
                continue
            yield event
    finally:
        # On a normal read-to-completion both pumps have already finished on
        # their own, so cancel is a no-op. But if the caller times out
        # (CancelledError punches through the await point above) or the client
        # disconnects mid-stream and this generator gets closed early, this
        # guarantees the pump tasks don't leak.
        for task in pumps:
            task.cancel()


class LocalSubprocessBackend(ExecutionBackend):
    """Stage 0 backend: spawn a subprocess on the local machine to run the code.

    WARNING: there is almost no isolation here. The code can access the local
    filesystem, network, and environment variables. For local learning only --
    never use it to accept untrusted input. Real isolation starts at Stage 1.
    """

    # How each language feeds code to its interpreter. Stage 0 only accepts python for now.
    _INTERPRETERS = {
        "python": [sys.executable, "-u", "-c"],  # -u disables buffering so streaming output is real-time
    }

    async def execute(
        self, request: ExecuteRequest
    ) -> AsyncIterator[OutputEvent]:
        interpreter = self._INTERPRETERS.get(request.language)
        if interpreter is None:
            yield OutputEvent(
                type=EventType.ERROR,
                data=f"unsupported language: {request.language}",
            )
            yield OutputEvent(type=EventType.END, exit_code=1)
            return

        cmd = [*interpreter, request.code]

        proc = await asyncio.create_subprocess_exec(
            *cmd,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
        )

        timed_out = False
        try:
            # Put a timeout around the whole execution. On timeout, kill the
            # process -- a local subprocess dies the moment it's killed.
            # Compare the same section in DockerBackend: the kill and cleanup
            # mechanics are precisely where the two backends differ.
            async with asyncio.timeout(request.timeout_seconds):
                async for event in _merge_output_streams(proc):
                    yield event
        except TimeoutError:
            timed_out = True
            proc.kill()
            yield OutputEvent(
                type=EventType.ERROR,
                data=f"execution timed out after {request.timeout_seconds}s",
            )

        # wait() both retrieves the exit code and reaps the subprocess, avoiding zombies.
        exit_code = await proc.wait()
        yield OutputEvent(type=EventType.END, exit_code=-1 if timed_out else exit_code)


# Stage 1 default image: the official slim image ships with a python interpreter, small and quick to pull.
DEFAULT_DOCKER_IMAGE = "python:3.12-slim"


class DockerBackend(ExecutionBackend):
    """Stage 1 backend: spin up a one-shot Docker container per execution to run the code.

    Compared to the Stage 0 local subprocess, this is where we get real
    isolation for the first time:
      - Filesystem isolation: the container has its own root filesystem (mount
        namespace) and can't see host files;
      - Network isolation: --network none gives it no NIC (network namespace),
        so the container is completely offline;
      - Resource limits: --memory / --cpus / --pids-limit are enforced by the
        kernel's cgroups.

    But the container shares the host kernel, so the escape surface is still far
    from small -- still don't use it to run fully untrusted code. Kernel-level
    strong isolation has to wait for the Stage 3 microVM.

    Implementation: use the `docker` CLI directly (as an asyncio subprocess),
    without pulling in docker-py. Reasons: (1) keep zero runtime dependencies;
    (2) docker-py is a synchronous library, and dropping it into an asyncio
    daemon would require wrapping it in a thread pool, which is more complex
    instead; (3) pumping output from the `docker run` subprocess is structurally
    identical to Stage 0 -- "swap isolation = swap the launch command + swap the
    kill" and the streaming pipeline (_merge_output_streams) is reused as-is.
    """

    # Inside the container the interpreter is just called python (shipped in the image),
    # no longer the host's sys.executable.
    _INTERPRETERS = {
        "python": ["python", "-u", "-c"],  # -u disables buffering so streaming output is real-time
    }

    def __init__(
        self,
        image: str = DEFAULT_DOCKER_IMAGE,
        memory: str = "256m",
        cpus: str = "1.0",
        pids_limit: int = 64,
    ) -> None:
        self.image = image
        self.memory = memory
        self.cpus = cpus
        self.pids_limit = pids_limit

    @staticmethod
    def check_available(image: str = DEFAULT_DOCKER_IMAGE) -> str | None:
        """Check whether Docker is available on this machine. Returns an error description, or None if all is well.

        Called synchronously during daemon startup (the event loop isn't running
        yet, hence subprocess.run), rather than waiting until the first /execute
        to report errors inside the SSE stream -- environment problems should be
        surfaced to "whoever started the daemon", not buried inside some
        execution's result.
        """
        if shutil.which("docker") is None:
            return "docker command not found; please install Docker first"
        try:
            probe = subprocess.run(["docker", "info"], capture_output=True, timeout=10)
        except subprocess.TimeoutExpired:
            return "docker info timed out; the Docker daemon may be hung"
        if probe.returncode != 0:
            return "Docker daemon is not running; please start it first (on WSL2 usually: sudo service docker start)"
        probe = subprocess.run(
            ["docker", "image", "inspect", image], capture_output=True, timeout=10
        )
        if probe.returncode != 0:
            return f"image {image} does not exist; please run: docker pull {image}"
        return None

    async def execute(
        self, request: ExecuteRequest
    ) -> AsyncIterator[OutputEvent]:
        interpreter = self._INTERPRETERS.get(request.language)
        if interpreter is None:
            yield OutputEvent(
                type=EventType.ERROR,
                data=f"unsupported language: {request.language}",
            )
            yield OutputEvent(type=EventType.END, exit_code=1)
            return

        # Give the container a recognizable name: first, timeout cleanup relies
        # on the name for docker rm -f; second, if the daemon gets kill -9'd and
        # leaves a stray container behind, you can clean up by prefix in one shot
        # (docker ps -a --filter name=microsandbox-exec -q | xargs -r docker rm -f).
        name = f"microsandbox-exec-{uuid.uuid4().hex[:12]}"

        cmd = [
            "docker", "run",
            "--rm",              # the docker daemon auto-deletes the container after it exits (the happy-path cleanup)
            "--pull", "never",   # error out immediately if the image is missing (exit 125). Never implicitly
                                 # pull an image inside the request path -- otherwise the tens of seconds of a first pull would eat up timeout_seconds
            "--name", name,
            "--network", "none",            # no NIC: the container is completely offline
            "--memory", self.memory,        # memory cap; exceeding it gets OOM-killed by the kernel (exit 137)
            "--memory-swap", self.memory,   # total cap including swap set to the same value = forbid using swap to bypass the limit
            "--cpus", self.cpus,
            "--pids-limit", str(self.pids_limit),  # guard against fork bombs
            "--read-only",                  # read-only root filesystem
            "--tmpfs", "/tmp:rw,size=64m",  # the only writable area: an in-memory /tmp
            self.image,
            # Structurally identical to Stage 0: python -u -c <code>. The code is
            # passed via argv, which on Linux is capped at ~2MB -- plenty for
            # Stage 1; this limit disappears in Stage 2 once we switch to a
            # long-lived agent inside the container.
            *interpreter, request.code,
        ]

        proc = await asyncio.create_subprocess_exec(
            *cmd,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
        )

        timed_out = False
        try:
            try:
                # The timeout covers the whole docker run, including the ~0.5-1s
                # of container cold start -- the "execution time" the user
                # perceives includes it anyway (cold-start optimization is a job
                # for the Stage 4 sandbox-pool warmup).
                async with asyncio.timeout(request.timeout_seconds):
                    async for event in _merge_output_streams(proc):
                        yield event
            except TimeoutError:
                timed_out = True
                # Key fact: killing the docker run client process does not kill
                # the container -- it's just the "remote control" wired to the
                # docker daemon, while the actual container process is managed by
                # the daemon. So we must docker rm -f to kill and delete the
                # container first, then reap the client process afterward.
                await self._remove_container(name)
                proc.kill()
                yield OutputEvent(
                    type=EventType.ERROR,
                    data=f"execution timed out after {request.timeout_seconds}s",
                )

            # Exit code: 0/1 are passed through verbatim from the container's
            # python; 125/126/127 are claimed by docker itself (respectively
            # docker error / command not executable / command not found), with
            # docker's error text mixed into the stderr events; 137 = 128+SIGKILL,
            # usually an OOM hit or being killed by rm -f.
            exit_code = await proc.wait()
            yield OutputEvent(type=EventType.END, exit_code=-1 if timed_out else exit_code)
        finally:
            # Idempotent safety-net: on the happy path --rm already deleted the
            # container, so a "No such container" failure here is ignored; this
            # also covers every error path, e.g. the client disconnecting
            # mid-stream and closing this generator early.
            await self._remove_container(name)

    @staticmethod
    async def _remove_container(name: str) -> None:
        """docker rm -f <name>: force-kill and delete the container. Any failure (e.g. container doesn't exist) is ignored."""
        proc = await asyncio.create_subprocess_exec(
            "docker", "rm", "-f", name,
            stdout=asyncio.subprocess.DEVNULL,
            stderr=asyncio.subprocess.DEVNULL,
        )
        await proc.wait()


# Stage 2b default agent image: built on slim with ipykernel + jupyter_client
# pre-installed, which the in-container daemon uses to host a long-lived kernel.
# Requires a docker build first (see the Dockerfile at the repo root).
DEFAULT_AGENT_IMAGE = "microsandbox-agent:latest"

# IPython's traceback carries ANSI color codes; strip them before it lands in our plain-text stderr.
_ANSI_RE = re.compile(r"\x1b\[[0-9;]*m")


def _strip_ansi(text: str) -> str:
    return _ANSI_RE.sub("", text)


class JupyterKernelBackend(ExecutionBackend):
    """Stage 2b backend: host a **long-lived** Jupyter (IPython) kernel inside the daemon process.

    This is the soul of Stage 2. Unlike Stage 0/1's "spin up a fresh interpreter
    per execution and throw it away when done", here the kernel lives on
    long-term and holds a Python namespace -- so variables defined across
    multiple run_code calls persist, and the second execution can directly use
    the first one's results. This is exactly how the E2B code interpreter works
    (it too is a Jupyter kernel underneath).

    Coordination with the client: the client uses backend="kernel" to spin up a
    long-lived container (the agent image pre-installs ipykernel/jupyter_client),
    the in-container daemon starts with --backend kernel, and what gets
    instantiated is this class. "Swap isolation / swap execution model = swap the
    backend"; the client and the /execute protocol stay untouched as before.

    Communication mechanism: via jupyter_client, speak the Jupyter message
    protocol to the kernel subprocess over ZMQ --
      - execute(code) sends the request on the shell channel and immediately
        returns that request's msg_id;
      - the kernel streams replies back on the iopub channel: stream
        (stdout/stderr), execute_result (the value of an expression), error (the
        exception traceback), status (busy/idle, where idle means this run is
        done).
    We translate these messages back into the project's unified OutputEvent, so
    the /execute protocol doesn't change at all.
    """

    def __init__(self) -> None:
        # Lazy dependency: jupyter_client is only needed when actually using the
        # kernel backend. This way, when running the local/docker/container
        # backends on the host, importing backend.py doesn't require jupyter.
        try:
            from jupyter_client.manager import AsyncKernelManager  # pyright: ignore[reportMissingImports]
        except ImportError as exc:  # pragma: no cover - only triggers in environments missing the dependency
            raise RuntimeError(
                "the kernel backend requires ipykernel + jupyter_client. Use the agent image"
                "(docker build -t microsandbox-agent .), "
                "or install locally with pip install 'microsandbox[kernel]'."
            ) from exc
        self._AsyncKernelManager = AsyncKernelManager
        self._km: object | None = None   # AsyncKernelManager
        self._kc: object | None = None   # AsyncKernelClient
        # The kernel is a shared, stateful resource that can only run one cell at
        # a time: use a lock to serialize concurrent execute calls (this is also
        # exactly the semantics a stateful REPL wants -- a later execution can
        # see the side effects of an earlier one).
        self._lock = asyncio.Lock()

    async def _ensure_started(self) -> None:
        """Lazily start the kernel on the first execution, then reuse it. The few seconds of cold start are paid only once."""
        if self._kc is not None:
            return
        km = self._AsyncKernelManager(kernel_name="python3")
        await km.start_kernel()
        kc = km.client()
        kc.start_channels()
        await kc.wait_for_ready(timeout=60)
        self._km, self._kc = km, kc

    async def execute(
        self, request: ExecuteRequest
    ) -> AsyncIterator[OutputEvent]:
        if request.language != "python":
            yield OutputEvent(
                type=EventType.ERROR,
                data=f"unsupported language: {request.language}",
            )
            yield OutputEvent(type=EventType.END, exit_code=1)
            return

        async with self._lock:  # serialize access to the shared kernel
            try:
                await self._ensure_started()
            except Exception as exc:  # noqa: BLE001 - startup failures must be reported faithfully to the user
                yield OutputEvent(
                    type=EventType.ERROR, data=f"kernel failed to start: {exc}"
                )
                yield OutputEvent(type=EventType.END, exit_code=1)
                return

            async for event in self._run_one(request):
                yield event

    async def _run_one(
        self, request: ExecuteRequest
    ) -> AsyncIterator[OutputEvent]:
        kc = self._kc
        # execute() is a synchronous method: it immediately sends the request on
        # the shell channel and returns msg_id. Afterward, every iopub message
        # belonging to this execution has parent_header.msg_id equal to this msg_id.
        msg_id = kc.execute(request.code, store_history=True, allow_stdin=False)

        had_error = False
        timed_out = False
        try:
            # The timeout covers only "the execution of this one cell", not the kernel's one-off cold start above.
            async with asyncio.timeout(request.timeout_seconds):
                while True:
                    msg = await kc.get_iopub_msg()
                    if msg.get("parent_header", {}).get("msg_id") != msg_id:
                        continue  # not a message for this execution, skip (filtering is more robust)
                    if msg["header"]["msg_type"] == "error":
                        had_error = True
                    event, done = self._translate(msg)
                    if event is not None:
                        yield event
                    if done:
                        break
        except TimeoutError:
            timed_out = True
            # Key design: on timeout, use interrupt (send the kernel a SIGINT)
            # rather than killing the process -- the cell is interrupted (e.g.
            # time.sleep raises KeyboardInterrupt), but the kernel and its
            # namespace stay alive, so previously defined variables aren't lost.
            # This is exactly the value of being "stateful", and the biggest
            # semantic difference from Stage 0/1's "timeout means kill the whole
            # interpreter".
            #
            # Known limitation: interrupt relies on SIGINT->KeyboardInterrupt,
            # which can't stop "code that swallows KeyboardInterrupt" or a hard
            # block inside a C extension -- in that case the cell keeps running
            # in the background and holds up subsequent executions. Production
            # (e.g. E2B) escalates to "restart the kernel" after a failed
            # interrupt (guaranteed to kill it, but loses state); this project
            # deliberately only does interrupt, preserving state.
            await self._km.interrupt_kernel()
            await self._drain_until_idle(msg_id)  # drain the leftovers produced by the interrupt, so the next run is clean
            yield OutputEvent(
                type=EventType.ERROR,
                data=f"execution timed out after {request.timeout_seconds}s",
            )

        # Reap this execution's shell execute_reply: beyond iopub, the kernel
        # also sends one reply on the shell channel, and if no one reads it, it
        # piles up in the channel -- once it hits the ZMQ high-water mark the
        # kernel's reply send will block. Both the normal and timeout paths
        # produce a reply, so placing this outside the try/except covers both.
        await self._drain_shell_reply(msg_id)

        # Exit code aligned with Stage 0/1 semantics: 0 on success; non-zero on exception (-> success False); -1 on timeout.
        if timed_out:
            exit_code = -1
        else:
            exit_code = 1 if had_error else 0
        yield OutputEvent(type=EventType.END, exit_code=exit_code)

    @staticmethod
    def _translate(msg: dict) -> tuple[OutputEvent | None, bool]:
        """Translate one iopub message into (OutputEvent | None, whether this execution has finished)."""
        mtype = msg["header"]["msg_type"]
        content = msg["content"]
        if mtype == "stream":
            etype = (
                EventType.STDOUT
                if content.get("name") == "stdout"
                else EventType.STDERR
            )
            return OutputEvent(type=etype, data=content.get("text", "")), False
        if mtype in ("execute_result", "display_data"):
            # The value of an expression (REPL echo, e.g. writing `x` directly
            # instead of print(x)). Folded into stdout, so we can carry it back
            # without adding a new event type to the /execute protocol.
            text = content.get("data", {}).get("text/plain")
            return (
                (OutputEvent(type=EventType.STDOUT, data=text + "\n"), False)
                if text
                else (None, False)
            )
        if mtype == "error":
            tb = _strip_ansi("\n".join(content.get("traceback", [])))
            return OutputEvent(type=EventType.STDERR, data=tb + "\n"), False
        if mtype == "status" and content.get("execution_state") == "idle":
            return None, True  # kernel back to idle: all output for this execution has streamed through
        return None, False  # messages that don't need forwarding, e.g. execute_input / busy

    async def _drain_until_idle(self, msg_id: str) -> None:
        """After an interrupt, drain this execution's leftover iopub messages up to idle, to avoid polluting the next execution."""
        try:
            async with asyncio.timeout(10):
                while True:
                    msg = await self._kc.get_iopub_msg()
                    if msg.get("parent_header", {}).get("msg_id") != msg_id:
                        continue
                    if (
                        msg["header"]["msg_type"] == "status"
                        and msg["content"].get("execution_state") == "idle"
                    ):
                        return
        except TimeoutError:
            return  # in extreme cases give up draining; the next execution's parent_header filtering tolerates leftovers anyway

    async def _drain_shell_reply(self, msg_id: str) -> None:
        """Reap this execution's shell execute_reply, to avoid unread messages
        piling up in the shell channel (once it hits the ZMQ high-water mark the
        kernel's reply send will block). Wait a little while at most; if nothing arrives, give up."""
        try:
            async with asyncio.timeout(5):
                while True:
                    reply = await self._kc.get_shell_msg()
                    if reply.get("parent_header", {}).get("msg_id") == msg_id:
                        return
        except TimeoutError:
            return
