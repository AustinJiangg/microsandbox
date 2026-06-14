"""Client SDK -- the interface users actually face when writing code.

Design goal: keep the feel as close to E2B as possible. Typical usage:

    from microsandbox import Sandbox

    with Sandbox() as sandbox:
        execution = sandbox.run_code("print('hello from sandbox')")
        print(execution.stdout)

Every Sandbox is a **Firecracker microVM**: an independent guest kernel behind
the KVM hardware-virtualization boundary (the strongest isolation in this
project), with the control channel carried over **vsock** and a stateful Jupyter
kernel running inside the VM (variables persist across run_code, like E2B).
Prepare the vendor/ artifacts first (firecracker binary + guest kernel + rootfs):
see docs/MICROVM_DESIGN.md §7 and scripts/build-rootfs.sh.

History: this project grew stage by stage (host subprocess -> Docker container ->
resident container -> microVM). Those earlier isolation backends were learning
scaffolding; now that the microVM works they have been removed, leaving only the
Firecracker path. The staged journey is preserved in the git history.
"""

from __future__ import annotations

import json
import os
import socket
import time
import urllib.error
import urllib.request
from collections.abc import Callable, Iterator
from typing import Any

from .protocol import EventType, Execution, ExecuteRequest, OutputEvent

# ---- microVM control channel ----
# As of Stage 4 the client no longer creates the microVM itself: the Go control
# plane (vendor/control-plane) owns spawn/restore/destroy. The client asks it for
# a sandbox over HTTP, then -- in Stage 4a -- still connects to that VM's vsock
# UDS directly (client and control plane share a host). The only topology
# constant the client still needs is the vsock port the in-VM daemon listens on.
_MICROVM_VSOCK_PORT = 1024   # vsock port the daemon listens on inside the VM (matches the rootfs's /init)

# Where to reach the control plane; overridable per-Sandbox (base_url=) or via env.
_DEFAULT_CONTROL_PLANE_URL = "http://127.0.0.1:8080"


# ---- vsock transport ----
#
# The control channel between the client and the in-VM daemon is HTTP/SSE carried
# over vsock. Firecracker multiplexes the guest's vsock onto a single Unix domain
# socket (UDS) on the host; after a text handshake (CONNECT <port> -> OK <hostport>)
# the byte stream is wired through to the daemon listening on that vsock port inside
# the guest, and from then on both sides speak the same HTTP/SSE. The protocol bytes
# (protocol.py) are byte-for-byte what earlier stages carried over TCP -- only the
# underlying pipe is vsock now. urllib can't do this CONNECT handshake, so we
# hand-write a minimal HTTP/1.1 client over the raw socket.


class _Response:
    """The normalized result of one HTTP round-trip over vsock: a status code + a file-like.

    Two usages share it; the only difference is how the caller reads fp:
      - streaming (SSE): `for line in resp:` consumes line by line until the connection
        closes (EOF);
      - single-shot (JSON): `resp.read()` reads the whole body at once.
    fp is the vsock socket's read file, which supports both "line-by-line iteration" and
    read(), so the upper-level run_code / files / commands logic doesn't care that the
    transport underneath is vsock.
    """

    def __init__(
        self, status: int, fp: Any, on_close: Callable[[], None] | None = None
    ) -> None:
        self.status = status
        self.fp = fp
        self._on_close = on_close

    def __iter__(self) -> Iterator[bytes]:
        return iter(self.fp)

    def read(self) -> bytes:
        return self.fp.read()

    def close(self) -> None:
        try:
            self.fp.close()
        finally:
            if self._on_close is not None:
                self._on_close()

    def __enter__(self) -> "_Response":
        return self

    def __exit__(self, *exc: Any) -> None:
        self.close()


class _VsockTransport:
    """HTTP over vsock: the microVM control channel.

    Firecracker multiplexes the guest's vsock onto a single Unix domain socket (UDS) on
    the host. The handshake is a text protocol: after connecting to the UDS, send
    `CONNECT <port>\\n`, Firecracker replies `OK <hostport>\\n`, and from then on this
    byte stream is wired to the daemon listening on that vsock port inside the guest.
    Once the handshake is done, both sides speak the same HTTP/SSE -- so here we only
    need to hand-write a minimal HTTP/1.1 client over the raw socket.
    """

    def __init__(self, uds_path: str, vsock_port: int) -> None:
        self._uds = uds_path
        self._vsock_port = vsock_port

    def request(
        self,
        method: str,
        path: str,
        body: bytes | None = None,
        headers: dict[str, str] | None = None,
        timeout: float | None = None,
    ) -> _Response:
        """Send one HTTP request over vsock, returning a _Response. timeout=None means no timeout (blocking)."""
        sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        sock.settimeout(timeout)  # None=blocking (used by streaming /execute); health check passes 1s
        try:
            sock.connect(self._uds)
            # 1. Firecracker vsock handshake: CONNECT <port> -> OK <hostport>
            sock.sendall(f"CONNECT {self._vsock_port}\n".encode())
            rfile = sock.makefile("rb")  # reading the response goes through it; writes always use sock.sendall, to avoid read/write buffers interfering
            ack = rfile.readline()
            if not ack.startswith(b"OK"):
                # e.g. nobody is listening on that vsock port inside the guest, so Firecracker replies non-OK.
                raise ConnectionError(f"vsock CONNECT rejected: {ack!r}")
            # 2. Hand-write the minimal HTTP/1.1 request line + headers + body
            head = [f"{method} {path} HTTP/1.1", "Host: sandbox", "Connection: close"]
            hdrs = dict(headers or {})
            if body is not None:
                hdrs.setdefault("Content-Length", str(len(body)))
            head += [f"{k}: {v}" for k, v in hdrs.items()]
            sock.sendall(("\r\n".join(head) + "\r\n\r\n").encode())
            if body:
                sock.sendall(body)
            # 3. Read the status line + skip the response headers, leaving rfile at the start of the body for the upper layer (both streaming and single-shot read from here)
            status_line = rfile.readline().decode("latin-1")
            status = int(status_line.split(" ", 2)[1])
            while True:
                line = rfile.readline()
                if line in (b"\r\n", b"\n", b""):
                    break
            return _Response(status, rfile, on_close=sock.close)
        except Exception:
            sock.close()
            raise


class Sandbox:
    """A client handle for a sandbox session, backed by a Firecracker microVM.

    Args:
        timeout_seconds: per-execution timeout passed to the daemon.
        from_snapshot: if True, ask the control plane to restore from a pre-warmed
            snapshot in milliseconds (skipping kernel boot + Jupyter kernel cold start,
            ~30ms to ready vs ~0.94s cold start). Run scripts/build-snapshot.sh first;
            currently single-instance (the snapshot's vsock uds path is fixed).
        base_url: where the Go control plane is reachable. Defaults to the
            MICROSANDBOX_URL env var, then http://127.0.0.1:8080.

    As of Stage 4 the client does not create the microVM itself: on construction it
    asks the control plane (POST /sandboxes) to spawn or restore one, then connects
    in over that VM's vsock UDS and waits for the in-VM daemon to become healthy.
    close() (or leaving the `with` block) asks the control plane to destroy it
    (DELETE /sandboxes/{id}).
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
        self._sandbox_id: str | None = None              # the control plane's handle for the VM (set by _create)
        self._transport: _VsockTransport | None = None    # connects to the VM's vsock UDS (set by _create)

        # File / shell namespaces, with a feel aligned to E2B's sandbox.files /
        # sandbox.commands. They only use the transport when called, so building them here is enough.
        self.files = _Files(self)
        self.commands = _Commands(self)

        # Safety-net: once _create succeeds the control plane has a VM running; if the
        # immediately following health check fails, the exception propagates out of
        # __init__ before the `with` statement enters __enter__, so __exit__ never fires
        # and the VM would leak. So here we close it ourselves (DELETE /sandboxes/{id})
        # and re-raise -- close is idempotent, so it's safe.
        try:
            self._create()
            self._wait_until_healthy()
        except Exception:
            self.close()
            raise

    # ----- lifecycle (delegated to the control plane) -----

    def _create(self) -> None:
        """Ask the control plane to spawn (or restore) a microVM and wire up the transport.

        Stage 4a: the client still connects to the VM's vsock UDS directly (it shares a
        host with the control plane). Stage 4b will move the vsock proxying into the
        control plane, and the client will then talk pure HTTP to it.
        """
        info = self._control_plane(
            "POST", "/sandboxes", {"from_snapshot": self._from_snapshot}
        )
        self._sandbox_id = info["id"]
        self._transport = _VsockTransport(info["uds_path"], _MICROVM_VSOCK_PORT)

    def _control_plane(
        self, method: str, path: str, body: dict | None = None
    ) -> dict:
        """Make one HTTP call to the control plane, returning the parsed JSON (or {}).

        A non-2xx response carrying {"error": ...} becomes a RuntimeError; an unreachable
        control plane becomes a RuntimeError with a hint to start it.
        """
        data = json.dumps(body).encode() if body is not None else None
        request = urllib.request.Request(
            self._base_url + path,
            data=data,
            method=method,
            headers={"Content-Type": "application/json"} if data is not None else {},
        )
        try:
            with urllib.request.urlopen(request, timeout=60) as resp:
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

    def _ensure_transport(self) -> _VsockTransport:
        """Return the vsock transport, set by _create during construction."""
        assert self._transport is not None, "transport not initialized (did sandbox creation fail?)"
        return self._transport

    def _wait_until_healthy(self, attempts: int = 50, delay: float = 0.1) -> None:
        transport = self._ensure_transport()
        for _ in range(attempts):
            try:
                with transport.request("GET", "/health", timeout=1) as resp:
                    if resp.status == 200:
                        return
            except Exception:
                pass
            time.sleep(delay)
        # Stage 4a: the client no longer owns the firecracker process, so it can't read
        # the guest serial log on failure -- that now lives on the control plane.
        # (Stage 4b moves the health probe into the control plane, which can surface the
        # console tail directly.)
        raise RuntimeError(
            "sandbox did not become healthy in time; "
            "check the control-plane log for the guest serial output"
        )

    def close(self) -> None:
        # Ask the control plane to destroy the VM (kill firecracker + clean up its
        # working directory). Idempotent: once the id is cleared, repeated calls and the
        # __exit__ path are no-ops, and a failed _create leaves nothing to destroy.
        if self._sandbox_id is not None:
            try:
                self._control_plane("DELETE", f"/sandboxes/{self._sandbox_id}")
            except Exception:
                pass  # best-effort: the control plane also destroys all VMs on its own shutdown
            self._sandbox_id = None
        self._transport = None

    def __enter__(self) -> "Sandbox":
        return self

    def __exit__(self, *exc: Any) -> None:
        self.close()

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
        """Issue an execution request to the in-VM daemon, parsing the SSE stream line by line.

        The request travels over vsock; here we only translate /execute's SSE response back
        into OutputEvents line by line.
        """
        request = ExecuteRequest(
            code=code, language=language, timeout_seconds=self.timeout_seconds
        )
        with self._ensure_transport().request(
            "POST",
            "/execute",
            body=request.to_json().encode(),
            headers={"Content-Type": "application/json"},
        ) as resp:
            for raw in resp:
                line = raw.decode(errors="replace").rstrip("\n")
                if not line.startswith("data: "):
                    continue
                payload = line[len("data: "):]
                if not payload:
                    continue
                yield OutputEvent.from_sse_payload(payload)

    def _post_json(self, path: str, payload: dict) -> dict:
        """POST a JSON to a non-streaming endpoint of the daemon, returning the parsed JSON dict.

        The file/command endpoints go through it (as opposed to run_code's SSE streaming).
        A non-200 response with a {"error": ...} body is raised as a RuntimeError.
        """
        with self._ensure_transport().request(
            "POST",
            path,
            body=json.dumps(payload).encode(),
            headers={"Content-Type": "application/json"},
        ) as resp:
            raw = resp.read().decode(errors="replace")
            if resp.status != 200:
                try:
                    message = json.loads(raw).get("error", raw)
                except json.JSONDecodeError:
                    message = raw
                raise RuntimeError(f"{path} failed: {message}")
            return json.loads(raw)


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
