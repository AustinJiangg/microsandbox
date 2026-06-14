"""Execution backend: how the daemon actually runs code, once it has been handed
a request.

There is exactly one implementation now -- `JupyterKernelBackend`, a long-lived
Jupyter (IPython) kernel hosted inside the daemon. It runs *inside the Firecracker
microVM*: the client starts the VM, the in-VM daemon (server.py) starts with
`--backend kernel`, and this class is what gets instantiated.

We keep the abstract base class `ExecutionBackend` so the daemon depends only on
the interface, not the concrete kernel implementation. Historically there were
also a host-subprocess backend and a one-shot-Docker backend (the early learning
stages); those were removed once the microVM landed. The staged journey lives on
in the git history.
"""

from __future__ import annotations

import asyncio
import re
from abc import ABC, abstractmethod
from collections.abc import AsyncIterator

from .protocol import EventType, ExecuteRequest, OutputEvent


class ExecutionBackend(ABC):
    """Abstract interface for an execution backend. Every isolation/execution scheme implements it."""

    @abstractmethod
    def execute(self, request: ExecuteRequest) -> AsyncIterator[OutputEvent]:
        """Execute a piece of code, producing output events as an async stream.

        Note the return type is AsyncIterator: the caller consumes it with
        `async for`, giving streaming output that "returns as it runs" rather
        than waiting for everything to finish.
        """
        raise NotImplementedError


# Default agent image: built on slim with ipykernel + jupyter_client pre-installed.
# The microVM's rootfs is exported from this image (scripts/build-rootfs.sh), so the
# in-VM daemon can host a long-lived kernel. Requires a docker build first (see the
# Dockerfile at the repo root) -- docker is only a one-time build tool now, not a
# runtime isolation mode.
DEFAULT_AGENT_IMAGE = "microsandbox-agent:latest"

# IPython's traceback carries ANSI color codes; strip them before it lands in our plain-text stderr.
_ANSI_RE = re.compile(r"\x1b\[[0-9;]*m")


def _strip_ansi(text: str) -> str:
    return _ANSI_RE.sub("", text)


class JupyterKernelBackend(ExecutionBackend):
    """Host a **long-lived** Jupyter (IPython) kernel inside the daemon process.

    Unlike a "spin up a fresh interpreter per execution and throw it away when
    done" model, here the kernel lives on long-term and holds a Python namespace
    -- so variables defined across multiple run_code calls persist, and the second
    execution can directly use the first one's results. This is exactly how the
    E2B code interpreter works (it too is a Jupyter kernel underneath).

    Coordination: the client starts a Firecracker microVM (its rootfs is exported
    from the agent image, which pre-installs ipykernel/jupyter_client); the in-VM
    daemon starts with --backend kernel, and what gets instantiated is this class.

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
        # kernel backend, so importing backend.py elsewhere doesn't require jupyter.
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
            # This is exactly the value of being "stateful".
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

        # Exit code: 0 on success; non-zero on exception (-> success False); -1 on timeout.
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
