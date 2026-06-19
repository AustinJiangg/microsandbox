"""Client SDK -- the interface users actually face when writing code.

Design goal: keep the feel as close to E2B as possible. Typical usage:

    from microsandbox import Sandbox

    with Sandbox() as sandbox:
        execution = sandbox.run_code("print('hello from sandbox')")
        print(execution.stdout)

Every Sandbox is a **Firecracker microVM** managed by the Go host services (Stage 9:
an `api` REST front for lifecycle, a per-machine `orchestrator` over gRPC, and a
`client-proxy` edge that owns the routing catalog). The SDK is a thin **pure-HTTP**
client: it asks the api for a sandbox (POST /sandboxes) and runs code by POSTing to
client-proxy (POST /execute with an X-Sandbox-Id header, ...); the services bridge to
the in-VM daemon over vsock. Start them first (scripts/dev-up.sh); they need the vendor/
artifacts -- see docs/MICROVM_DESIGN.md §7, docs/STAGE9_DESIGN.md and docs/STAGE8_DESIGN.md.

History: this project grew stage by stage (host subprocess -> Docker container ->
resident container -> microVM -> control-plane split). Those earlier isolation
backends were learning scaffolding; now that the microVM works they have been
removed, leaving only the Firecracker path. The staged journey is preserved in the
git history.
"""

from __future__ import annotations

import json
import os
import time
import urllib.error
import urllib.request
from collections.abc import Callable, Iterator
from typing import Any

from .connect import server_stream
from .protocol import EventType, Execution, OutputEvent

# Where to reach the services; overridable per-Sandbox (base_url= / data_url=) or via env.
# The SDK speaks only plain HTTP -- the vsock handshake + bridge live in the services
# (services/pkg/proxy), not here. Stage 9 split the two faces: lifecycle goes to the api
# (base_url), the data path to client-proxy (data_url, learned from the create response).
_DEFAULT_CONTROL_PLANE_URL = "http://127.0.0.1:8080"  # the api (lifecycle): POST/DELETE/GET /sandboxes
_DEFAULT_DATA_PLANE_URL = "http://127.0.0.1:8081"  # client-proxy (data): /execute, /files/*, /commands


class Sandbox:
    """A client handle for a sandbox session, backed by a Firecracker microVM.

    Args:
        timeout_seconds: per-execution timeout passed to the daemon.
        from_snapshot: if True, ask the control plane to restore from a pre-warmed
            snapshot in milliseconds (skipping kernel boot + Jupyter kernel cold start,
            ~30ms to ready vs ~0.94s cold start). Run scripts/build-snapshot.sh first.
            Several sandboxes can be restored from the one snapshot concurrently -- the
            control plane gives each VM its own vsock socket (Stage 5a).
        template: name of a custom image to boot, built with scripts/build-template.sh
            (see docs/STAGE6_DESIGN.md). Defaults to the stock image. The name must be a
            template built under vendor/templates/<name>/; an unknown name is rejected.
        base_url: where the api (lifecycle) is reachable. Defaults to the
            MICROSANDBOX_URL env var, then http://127.0.0.1:8080.
        data_url: where client-proxy (the data path) is reachable. Normally learned from
            the create response; set this (or MICROSANDBOX_DATA_URL) to override it, else
            http://127.0.0.1:8081.

    The SDK is a thin pure-HTTP client. On construction it asks the api to spawn or
    restore a microVM (POST /sandboxes), which returns only once the VM is healthy
    ("ready on delivery") along with the data_url to reach it. run_code / files /
    commands then POST to client-proxy (data_url) with an X-Sandbox-Id header, which
    routes to the in-VM daemon over vsock. close() (or leaving the `with` block) destroys
    it (DELETE /sandboxes/{id} on the api).
    """

    def __init__(
        self,
        timeout_seconds: float = 30.0,
        from_snapshot: bool = False,
        template: str | None = None,
        base_url: str | None = None,
        data_url: str | None = None,
    ) -> None:
        self.timeout_seconds = timeout_seconds
        self._from_snapshot = from_snapshot
        self._template = template
        self._base_url = (
            base_url or os.environ.get("MICROSANDBOX_URL", _DEFAULT_CONTROL_PLANE_URL)
        ).rstrip("/")
        # The data path goes to client-proxy. Its URL is normally learned from the create
        # response; an explicit data_url= / MICROSANDBOX_DATA_URL overrides that.
        self._data_url_override = data_url or os.environ.get("MICROSANDBOX_DATA_URL")
        self._data_url: str | None = None  # set by _create (override, response, or default)
        self._sandbox_id: str | None = None  # the api's handle for the VM (set by _create)

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
        body: dict = {"from_snapshot": self._from_snapshot}
        # Only send `template` when set, so the default case stays byte-identical to
        # the pre-Stage-6 request (an absent field means the default template).
        if self._template is not None:
            body["template"] = self._template
        info = self._control_plane("POST", "/sandboxes", body)
        self._sandbox_id = info["id"]
        # The api tells the SDK where to reach the sandbox's data path (client-proxy). An
        # explicit override wins; else use the response; else the default.
        self._data_url = (
            self._data_url_override or info.get("data_url") or _DEFAULT_DATA_PLANE_URL
        ).rstrip("/")

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

    def _data_headers(self) -> dict:
        """Headers that route a data request to this sandbox via client-proxy.

        client-proxy routes by the X-Sandbox-Id header (the daemon endpoint is just the
        request path), so a data call carries the id here rather than in the URL.
        """
        if self._sandbox_id is None:
            raise RuntimeError("sandbox is closed")
        return {"X-Sandbox-Id": self._sandbox_id}

    def _control_plane(
        self,
        method: str,
        path: str,
        body: dict | None = None,
        timeout: float | None = None,
        base: str | None = None,
        headers: dict | None = None,
    ) -> dict:
        """Make one HTTP call to a service, returning the parsed JSON (or {}).

        Lifecycle calls go to the api (base_url, the default); data calls pass
        base=self._data_url and headers={"X-Sandbox-Id": ...} to reach client-proxy. A
        non-2xx carrying {"error": ...} becomes a RuntimeError; an unreachable service
        becomes a RuntimeError with a hint to start it. timeout=None blocks (a command may
        legitimately run a while).
        """
        base = base or self._base_url
        data = json.dumps(body).encode() if body is not None else None
        req_headers = dict(headers or {})
        if data is not None:
            req_headers.setdefault("Content-Type", "application/json")
        request = urllib.request.Request(
            base + path, data=data, method=method, headers=req_headers
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
            raise RuntimeError(f"{method} {path} failed: {detail}") from exc
        except urllib.error.URLError as exc:
            raise RuntimeError(
                f"cannot reach {base} ({exc.reason}); "
                "is it running? start the services with scripts/dev-up.sh"
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
        """Run code via the code-interpreter's server-streaming Execute (ConnectRPC).

        client-proxy routes /codeinterpreter.* to the code-interpreter's vsock port; the
        Connect stream's frames flush through live, so this yields each OutputEvent as the
        cell runs -- the Connect-protocol successor to the daemon's old /execute SSE.
        """
        url = self._data_url + "/codeinterpreter.CodeInterpreterService/Execute"
        # protojson uses camelCase JSON names: timeoutSeconds maps to the proto's timeout_seconds.
        message = {"code": code, "language": language, "timeoutSeconds": self.timeout_seconds}
        for msg in server_stream(url, message, headers=self._data_headers()):
            yield _output_event(msg)

    def _post_json(self, path: str, payload: dict) -> dict:
        """POST JSON to one of the sandbox's daemon endpoints (via client-proxy).

        The file/command endpoints go through here (run_code uses _stream's SSE instead).
        A daemon error (non-200 carrying {"error": ...}) surfaces as a RuntimeError.
        """
        return self._control_plane(
            "POST", path, payload, base=self._data_url, headers=self._data_headers()
        )


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


def _output_event(msg: dict) -> OutputEvent:
    """Build an OutputEvent from a code-interpreter stream message (Stage 11).

    protojson omits zero/empty values, so the fields default. exit_code (protojson's
    camelCase 'exitCode') is meaningful only on the 'end' event, where the old daemon
    always conveyed a number -- so default a missing one to 0 there, keeping success /
    exit_code behavior identical to the SSE days.
    """
    etype = EventType(msg.get("type", "end"))
    exit_code = None
    if etype == EventType.END:
        exit_code = int(msg.get("exitCode", msg.get("exit_code", 0)))
    return OutputEvent(type=etype, data=msg.get("data", ""), exit_code=exit_code)


# ----- template build API (Stage 10), aligned with E2B's `template build` -----


def build_template(
    name: str,
    dockerfile: str,
    *,
    with_snapshot: bool = True,
    base_url: str | None = None,
    poll_interval: float = 1.0,
    timeout: float = 600.0,
) -> None:
    """Build a custom template image from a Dockerfile recipe, blocking until it is ready.

    The local equivalent of `e2b template build`: it POSTs the recipe to the api, which
    kicks an asynchronous build in the orchestrator (docker build -> rootfs -> snapshot),
    then polls the build status until it succeeds (returns) or fails (raises RuntimeError
    with the build's error detail). On success the image boots with Sandbox(template=name).

    The recipe is the Dockerfile contents (FROM microsandbox-agent + RUN ...); arbitrary
    local-file COPY is out of scope (no build-context upload yet). Pass with_snapshot=False
    to skip the warm snapshot -- a faster build, but from_snapshot won't work until one is
    built.

    Args:
        name: the template name to publish (an existing one is replaced); "default" is
            rejected (it is the baked stock image).
        dockerfile: the recipe contents.
        base_url: where the api is reachable (defaults to MICROSANDBOX_URL, then :8080).
        poll_interval: seconds between status polls.
        timeout: seconds to wait for the build before raising.
    """
    api = (
        base_url or os.environ.get("MICROSANDBOX_URL", _DEFAULT_CONTROL_PLANE_URL)
    ).rstrip("/")

    created = _api_request(
        "POST",
        api + "/templates",
        {"name": name, "dockerfile": dockerfile, "with_snapshot": with_snapshot},
    )
    build_id = created["build_id"]

    deadline = time.monotonic() + timeout
    while True:
        status = _api_request("GET", api + f"/templates/builds/{build_id}")
        state = status.get("state")
        if state == "success":
            return
        if state == "failed":
            raise RuntimeError(
                f"template build {build_id} failed: {status.get('detail', '')}"
            )
        if time.monotonic() > deadline:
            raise RuntimeError(
                f"template build {build_id} did not finish within {timeout}s (last state: {state})"
            )
        time.sleep(poll_interval)


def _api_request(method: str, url: str, body: dict | None = None) -> dict:
    """One HTTP call to the api, returning parsed JSON (or {}). A module-level twin of
    Sandbox._control_plane, for the lifecycle-free template build API."""
    data = json.dumps(body).encode() if body is not None else None
    request = urllib.request.Request(
        url,
        data=data,
        method=method,
        headers={"Content-Type": "application/json"} if data is not None else {},
    )
    try:
        with urllib.request.urlopen(request) as resp:
            raw = resp.read()
        return json.loads(raw) if raw else {}
    except urllib.error.HTTPError as exc:
        detail = exc.read().decode(errors="replace")
        try:
            detail = json.loads(detail).get("error", detail)
        except json.JSONDecodeError:
            pass
        raise RuntimeError(f"{method} {url} failed: {detail}") from exc
    except urllib.error.URLError as exc:
        raise RuntimeError(
            f"cannot reach the api at {url} ({exc.reason}); "
            "is it running? start the services with scripts/dev-up.sh"
        ) from exc
