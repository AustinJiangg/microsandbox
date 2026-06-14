"""Client SDK -- the interface users actually face when writing code.

Design goal: keep the feel as close to E2B as possible. Typical usage:

    from microsandbox import Sandbox

    with Sandbox() as sandbox:
        execution = sandbox.run_code("print('hello from sandbox')")
        print(execution.stdout)

Every Sandbox is a **Firecracker microVM** managed by the Go **control plane**
(vendor/control-plane). As of Stage 4b the SDK is a thin **pure-HTTP** client: it
asks the control plane for a sandbox (POST /sandboxes) and runs code by POSTing
through it (/sandboxes/{id}/execute, ...); the control plane bridges to the in-VM
daemon over vsock. Start the control plane first (scripts/build-control-plane.sh,
then ./vendor/control-plane); it needs the vendor/ artifacts -- see
docs/MICROVM_DESIGN.md §7 and docs/STAGE4_DESIGN.md.

History: this project grew stage by stage (host subprocess -> Docker container ->
resident container -> microVM -> control-plane split). Those earlier isolation
backends were learning scaffolding; now that the microVM works they have been
removed, leaving only the Firecracker path. The staged journey is preserved in the
git history.
"""

from __future__ import annotations

import json
import os
import urllib.error
import urllib.request
from collections.abc import Callable, Iterator
from typing import Any

from .protocol import EventType, Execution, ExecuteRequest, OutputEvent

# Where to reach the control plane; overridable per-Sandbox (base_url=) or via env.
# As of Stage 4b the SDK speaks only plain HTTP to the control plane -- the vsock
# handshake + bridge now live in the control plane (control-plane/proxy.go), not here.
_DEFAULT_CONTROL_PLANE_URL = "http://127.0.0.1:8080"


class Sandbox:
    """A client handle for a sandbox session, backed by a Firecracker microVM.

    Args:
        timeout_seconds: per-execution timeout passed to the daemon.
        from_snapshot: if True, ask the control plane to restore from a pre-warmed
            snapshot in milliseconds (skipping kernel boot + Jupyter kernel cold start,
            ~30ms to ready vs ~0.94s cold start). Run scripts/build-snapshot.sh first.
            Several sandboxes can be restored from the one snapshot concurrently -- the
            control plane gives each VM its own vsock socket (Stage 5a).
        base_url: where the Go control plane is reachable. Defaults to the
            MICROSANDBOX_URL env var, then http://127.0.0.1:8080.

    The SDK is a thin pure-HTTP client. On construction it asks the control plane to
    spawn or restore a microVM (POST /sandboxes), which returns only once the VM is
    healthy ("ready on delivery"). run_code / files / commands then POST through the
    control plane (/sandboxes/{id}/...), which bridges to the in-VM daemon over vsock.
    close() (or leaving the `with` block) destroys it (DELETE /sandboxes/{id}).
    """

    def __init__(
        self,
        timeout_seconds: float = 30.0,
        from_snapshot: bool = False,
        base_url: str | None = None,
    ) -> None:
        self.timeout_seconds = timeout_seconds
        self._from_snapshot = from_snapshot
        self._base_url = (
            base_url or os.environ.get("MICROSANDBOX_URL", _DEFAULT_CONTROL_PLANE_URL)
        ).rstrip("/")
        self._sandbox_id: str | None = None  # the control plane's handle for the VM (set by _create)

        # File / shell namespaces, with a feel aligned to E2B's sandbox.files /
        # sandbox.commands. They only talk to the control plane when called.
        self.files = _Files(self)
        self.commands = _Commands(self)

        # _create returns only once the control plane reports the VM healthy, so there
        # is nothing more to wait for here. If it raises, no sandbox id was stored and
        # the control plane has already torn the VM down -- nothing leaks.
        self._create()

    # ----- lifecycle (delegated to the control plane) -----

    def _create(self) -> None:
        """Ask the control plane to spawn (or restore) a microVM (POST /sandboxes).

        The control plane returns only once the VM is healthy, so there is nothing to
        wait for here. It owns the vsock bridge; the SDK never speaks vsock itself.
        """
        info = self._control_plane(
            "POST", "/sandboxes", {"from_snapshot": self._from_snapshot}
        )
        self._sandbox_id = info["id"]

    def close(self) -> None:
        # Ask the control plane to destroy the VM. Idempotent: once the id is cleared,
        # repeated calls and the __exit__ path are no-ops, and a failed _create leaves
        # nothing to destroy.
        if self._sandbox_id is not None:
            try:
                self._control_plane("DELETE", f"/sandboxes/{self._sandbox_id}")
            except Exception:
                pass  # best-effort: the control plane also destroys all VMs on its own shutdown
            self._sandbox_id = None

    def __enter__(self) -> "Sandbox":
        return self

    def __exit__(self, *exc: Any) -> None:
        self.close()

    # ----- HTTP to the control plane -----

    def _sandbox_path(self, path: str) -> str:
        """Map a daemon endpoint (e.g. /execute, /files/read) to its control-plane proxy path."""
        if self._sandbox_id is None:
            raise RuntimeError("sandbox is closed")
        return f"/sandboxes/{self._sandbox_id}{path}"

    def _control_plane(
        self,
        method: str,
        path: str,
        body: dict | None = None,
        timeout: float | None = None,
    ) -> dict:
        """Make one HTTP call to the control plane, returning the parsed JSON (or {}).

        Used for both lifecycle (/sandboxes) and the proxied data path (/sandboxes/{id}/
        files|commands). A non-2xx carrying {"error": ...} becomes a RuntimeError; an
        unreachable control plane becomes a RuntimeError with a hint to start it.
        timeout=None blocks (a command may legitimately run a while).
        """
        data = json.dumps(body).encode() if body is not None else None
        request = urllib.request.Request(
            self._base_url + path,
            data=data,
            method=method,
            headers={"Content-Type": "application/json"} if data is not None else {},
        )
        try:
            with urllib.request.urlopen(request, timeout=timeout) as resp:
                raw = resp.read()
            return json.loads(raw) if raw else {}
        except urllib.error.HTTPError as exc:
            detail = exc.read().decode(errors="replace")
            try:
                detail = json.loads(detail).get("error", detail)
            except json.JSONDecodeError:
                pass
            raise RuntimeError(f"control plane {method} {path} failed: {detail}") from exc
        except urllib.error.URLError as exc:
            raise RuntimeError(
                f"cannot reach the control plane at {self._base_url} ({exc.reason}); "
                "is it running? build it with scripts/build-control-plane.sh, then run "
                "./vendor/control-plane"
            ) from exc

    # ----- core API -----

    def run_code(
        self,
        code: str,
        language: str = "python",
        on_stdout: Callable[[str], None] | None = None,
        on_stderr: Callable[[str], None] | None = None,
    ) -> Execution:
        """Execute a piece of code, returning the aggregated Execution result.

        Optionally pass on_stdout / on_stderr callbacks to get streaming output in real
        time (e.g. print as it runs), while still ultimately returning the complete
        Execution.
        """
        execution = Execution()
        for event in self._stream(code, language):
            execution.events.append(event)
            if event.type == EventType.STDOUT:
                execution.stdout += event.data
                if on_stdout:
                    on_stdout(event.data)
            elif event.type == EventType.STDERR:
                execution.stderr += event.data
                if on_stderr:
                    on_stderr(event.data)
            elif event.type == EventType.ERROR:
                execution.error = (execution.error or "") + event.data
            elif event.type == EventType.END:
                execution.exit_code = event.exit_code
        return execution

    def _stream(self, code: str, language: str) -> Iterator[OutputEvent]:
        """POST /execute (via the control-plane proxy) and parse the SSE stream line by line.

        The control plane proxies the daemon's SSE response straight through, so the SDK
        reads it over plain HTTP -- no vsock on this side anymore.
        """
        request = ExecuteRequest(
            code=code, language=language, timeout_seconds=self.timeout_seconds
        )
        http_request = urllib.request.Request(
            self._base_url + self._sandbox_path("/execute"),
            data=request.to_json().encode(),
            method="POST",
            headers={"Content-Type": "application/json"},
        )
        # No read timeout: an execution can run for a while; the daemon enforces
        # timeout_seconds and ends the stream.
        with urllib.request.urlopen(http_request) as resp:
            for raw in resp:
                line = raw.decode(errors="replace").rstrip("\n")
                if not line.startswith("data: "):
                    continue
                payload = line[len("data: "):]
                if not payload:
                    continue
                yield OutputEvent.from_sse_payload(payload)

    def _post_json(self, path: str, payload: dict) -> dict:
        """POST JSON to one of the sandbox's daemon endpoints (via the control-plane proxy).

        The file/command endpoints go through here (run_code uses _stream's SSE instead).
        A daemon error (non-200 carrying {"error": ...}) surfaces as a RuntimeError.
        """
        return self._control_plane("POST", self._sandbox_path(path), payload)


# ----- file / shell namespaces (attached to Sandbox, with a feel aligned to E2B) -----


class _Files:
    """sandbox.files.*: read/write/list directories in the sandbox filesystem (aligned with E2B's sandbox.files).

    Done by the in-VM daemon directly on the VM's own filesystem. Note the VM has a
    --read-only root + only /tmp writable -- writing outside /tmp raises a RuntimeError.
    """

    def __init__(self, sandbox: Sandbox) -> None:
        self._sb = sandbox

    def write(self, path: str, content: str) -> None:
        self._sb._post_json("/files/write", {"path": path, "content": content})

    def read(self, path: str) -> str:
        return self._sb._post_json("/files/read", {"path": path})["content"]

    def list(self, path: str) -> list[dict]:
        """List a directory, returning [{"name": str, "is_dir": bool}, ...]."""
        return self._sb._post_json("/files/list", {"path": path})["entries"]


class _Commands:
    """sandbox.commands.*: run shell commands in the sandbox (aligned with E2B's sandbox.commands)."""

    def __init__(self, sandbox: Sandbox) -> None:
        self._sb = sandbox

    def run(self, command: str, timeout_seconds: float | None = None) -> Execution:
        """Run a shell command, returning the same Execution as run_code (stdout/stderr/exit_code)."""
        payload = {
            "command": command,
            # Use `is None` rather than `or`: the latter would treat an explicitly passed 0 as "not passed" and fall back to the default.
            "timeout_seconds": (
                timeout_seconds
                if timeout_seconds is not None
                else self._sb.timeout_seconds
            ),
        }
        data = self._sb._post_json("/commands", payload)
        return Execution(
            stdout=data["stdout"],
            stderr=data["stderr"],
            exit_code=data["exit_code"],
        )
