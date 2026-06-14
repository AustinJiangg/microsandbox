"""Sandbox daemon (daemon / server).

It runs *inside the Firecracker microVM* as a resident agent -- the counterpart
of E2B's `envd`. It listens on vsock for HTTP requests, hands code off to the
execution backend (a stateful Jupyter kernel) to run, then streams the output
back to the client over SSE.

The client starts the VM and connects in over vsock; the daemon code itself is
transport-agnostic (it just listens on a different kind of socket). This is the
embodiment of the project's main thread: keep the wire protocol (protocol.py)
fixed, swap only the isolation/transport underneath.

Implemented with the standard library only. If hand-rolling HTTP feels too
tedious you can switch to FastAPI/uvicorn -- just keep the interface contract
(the wire protocol) unchanged.
"""

from __future__ import annotations

import argparse
import asyncio
import json
import logging
import pathlib
import socket

from .backend import ExecutionBackend, JupyterKernelBackend
from .protocol import (
    CommandRequest,
    ExecuteRequest,
    PathRequest,
    WriteFileRequest,
)

logger = logging.getLogger("microsandbox.server")


def _ensure_loopback_up() -> None:
    """Bring the loopback interface (lo) UP.

    Inside a microVM, lo defaults to down, but the kernel backend's Jupyter
    kernel talks to the daemon over ZMQ on 127.0.0.1 -- if lo isn't up the
    connection fails (manifesting as a 60s kernel-startup timeout). The minimal
    rootfs has no ip/ifconfig installed, so we bring it up directly via the
    SIOCSIFFLAGS ioctl (equivalent to `ip link set lo up`).

    Only called on the vsock path (= running inside a microVM): the lo of a
    container/host is already up, so there's no need -- and no business -- to
    touch it. Strictly speaking this is init/system-bootstrap's job, but our
    rootfs init is a minimal shell (no networking tools), whereas the daemon is
    an already-running root process, so doing it here is the least-effort spot
    and comes with a built-in conditional guard.
    """
    import fcntl
    import struct

    IFF_UP = 0x1
    SIOCGIFFLAGS, SIOCSIFFLAGS = 0x8913, 0x8914
    sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    try:
        # struct ifreq: char name[16] + flags(short), padded out to sizeof(ifreq)=40.
        req = struct.pack("16sH22s", b"lo", 0, b"")
        flags = struct.unpack("16sH22s", fcntl.ioctl(sock.fileno(), SIOCGIFFLAGS, req))[1]
        req = struct.pack("16sH22s", b"lo", flags | IFF_UP, b"")
        fcntl.ioctl(sock.fileno(), SIOCSIFFLAGS, req)
    finally:
        sock.close()


class SandboxServer:
    """Minimal asyncio-based HTTP daemon. Endpoints:

        GET  /health        Health check (used from Stage 2+ to probe whether the sandbox is ready)
        POST /execute       Execute code, stream OutputEvent back over SSE
        POST /files/read    Read a file       (Stage 2c)
        POST /files/write   Write a file      (Stage 2c)
        POST /files/list    List a directory  (Stage 2c)
        POST /commands      Run a shell command (Stage 2c)

    The Stage 2c file/command endpoints are served by the daemon directly against
    its own filesystem (not through the backend) -- mirroring E2B's envd: the
    file/process services are separate from the kernel that runs code.
    """

    def __init__(self, backend: ExecutionBackend | None = None) -> None:
        # Depend on the abstract interface rather than a concrete implementation.
        # Inside the microVM the only backend is the stateful Jupyter kernel.
        self.backend = backend or JupyterKernelBackend()

    async def handle(
        self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter
    ) -> None:
        try:
            request_line = await reader.readline()
            if not request_line:
                return
            method, path, _ = request_line.decode().split(" ", 2)

            # Read the headers to get Content-Length, so we can read the body
            headers: dict[str, str] = {}
            while True:
                line = await reader.readline()
                if line in (b"\r\n", b"\n", b""):
                    break
                key, _, value = line.decode().partition(":")
                headers[key.strip().lower()] = value.strip()

            if method == "GET" and path == "/health":
                await self._write_json(writer, 200, {"status": "ok"})
                return

            if method == "POST":
                # Every POST endpoint carries a JSON body, so read it once and then dispatch by path.
                length = int(headers.get("content-length", "0"))
                body = (await reader.readexactly(length) if length else b"{}").decode()
                if path == "/execute":
                    await self._handle_execute(writer, body)
                    return
                if path == "/files/read":
                    await self._handle_file_read(writer, body)
                    return
                if path == "/files/write":
                    await self._handle_file_write(writer, body)
                    return
                if path == "/files/list":
                    await self._handle_file_list(writer, body)
                    return
                if path == "/commands":
                    await self._handle_command(writer, body)
                    return

            await self._write_json(writer, 404, {"error": "not found"})
        except Exception as exc:  # noqa: BLE001 - a single request must not crash the daemon
            logger.exception("request handling failed")
            try:
                await self._write_json(writer, 500, {"error": str(exc)})
            except Exception:
                pass
        finally:
            writer.close()
            await writer.wait_closed()

    async def _handle_execute(
        self, writer: asyncio.StreamWriter, raw_body: str
    ) -> None:
        try:
            request = ExecuteRequest.from_json(raw_body)
        except (json.JSONDecodeError, KeyError) as exc:
            await self._write_json(writer, 400, {"error": f"bad request: {exc}"})
            return

        # Write the SSE response headers first, then flush event by event to achieve streaming.
        writer.write(
            b"HTTP/1.1 200 OK\r\n"
            b"Content-Type: text/event-stream\r\n"
            b"Cache-Control: no-cache\r\n"
            b"Connection: close\r\n"
            b"\r\n"
        )
        await writer.drain()

        async for event in self.backend.execute(request):
            writer.write(event.to_sse().encode())
            await writer.drain()

    # ----- Stage 2c: file / shell endpoints -----
    # All served by the daemon directly against the filesystem/environment it
    # lives in (not through the ExecutionBackend) -- for the container/kernel
    # backends that's inside the container (in the sandbox), and for the local
    # backend that's the host.
    #
    # Path restrictions: we *deliberately* do no path allowlisting/validation
    # here. Under the container/kernel backends the daemon can only see the
    # container's own files; the "container boundary" is the natural confinement,
    # and adding artificial restrictions on top would instead block legitimate
    # reads (e.g. /etc/os-release). The only unsafe case is the local backend
    # (which can touch any file on the host) -- consistent with local's
    # longstanding "no isolation, never expose externally" positioning, not a
    # new problem introduced here.

    async def _handle_file_read(
        self, writer: asyncio.StreamWriter, raw_body: str
    ) -> None:
        try:
            req = PathRequest.from_json(raw_body)
        except (json.JSONDecodeError, KeyError) as exc:
            await self._write_json(writer, 400, {"error": f"bad request: {exc}"})
            return
        try:
            # Small files: read directly, the blocking is negligible. For large files / high concurrency, consider run_in_executor (Stage 4).
            content = pathlib.Path(req.path).read_text()
        except OSError as exc:
            await self._write_json(writer, 404, {"error": str(exc)})
            return
        await self._write_json(writer, 200, {"content": content})

    async def _handle_file_write(
        self, writer: asyncio.StreamWriter, raw_body: str
    ) -> None:
        try:
            req = WriteFileRequest.from_json(raw_body)
        except (json.JSONDecodeError, KeyError) as exc:
            await self._write_json(writer, 400, {"error": f"bad request: {exc}"})
            return
        try:
            p = pathlib.Path(req.path)
            p.parent.mkdir(parents=True, exist_ok=True)  # create intermediate dirs along the way (e.g. when writing /tmp/a/b.txt)
            p.write_text(req.content)
        except OSError as exc:
            # Common case: a resident container with a --read-only root, so writing outside /tmp lands here. Report the reason verbatim.
            await self._write_json(writer, 400, {"error": str(exc)})
            return
        await self._write_json(writer, 200, {"ok": True})

    async def _handle_file_list(
        self, writer: asyncio.StreamWriter, raw_body: str
    ) -> None:
        try:
            req = PathRequest.from_json(raw_body)
        except (json.JSONDecodeError, KeyError) as exc:
            await self._write_json(writer, 400, {"error": f"bad request: {exc}"})
            return
        try:
            entries = [
                {"name": e.name, "is_dir": e.is_dir()}
                for e in sorted(pathlib.Path(req.path).iterdir())
            ]
        except OSError as exc:
            await self._write_json(writer, 404, {"error": str(exc)})
            return
        await self._write_json(writer, 200, {"entries": entries})

    async def _handle_command(
        self, writer: asyncio.StreamWriter, raw_body: str
    ) -> None:
        try:
            req = CommandRequest.from_json(raw_body)
        except (json.JSONDecodeError, KeyError) as exc:
            await self._write_json(writer, 400, {"error": f"bad request: {exc}"})
            return
        # Run the shell in the daemon's own environment (container/kernel backend = inside the container = in the sandbox).
        # Non-streaming: once it finishes, collect stdout/stderr/exit_code all at once (streaming can be a later extension).
        proc = await asyncio.create_subprocess_shell(
            req.command,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
        )
        try:
            out, err = await asyncio.wait_for(
                proc.communicate(), timeout=req.timeout_seconds
            )
            payload = {
                "stdout": out.decode(errors="replace"),
                "stderr": err.decode(errors="replace"),
                "exit_code": proc.returncode,
            }
        except TimeoutError:
            proc.kill()
            await proc.wait()
            payload = {
                "stdout": "",
                "stderr": f"command timed out after {req.timeout_seconds}s",
                "exit_code": -1,
            }
        await self._write_json(writer, 200, payload)

    async def _write_json(
        self, writer: asyncio.StreamWriter, status: int, payload: dict
    ) -> None:
        body = json.dumps(payload).encode()
        status_text = {200: "OK", 400: "Bad Request", 404: "Not Found", 500: "Internal Server Error"}.get(status, "OK")
        writer.write(
            f"HTTP/1.1 {status} {status_text}\r\n".encode()
            + b"Content-Type: application/json\r\n"
            + f"Content-Length: {len(body)}\r\n".encode()
            + b"Connection: close\r\n\r\n"
            + body
        )
        await writer.drain()

    async def serve(self, *, vsock_port: int = 1024) -> None:
        # The daemon runs inside a Firecracker microVM, so the control channel is
        # vsock (the host connects in via Firecracker's UDS, see
        # docs/STAGE3_DESIGN.md §4.1). Apart from "which kind of socket to listen
        # on", handle / dispatch / backend are all unchanged -- exactly the
        # embodiment of "stable protocol, swappable transport".
        #
        # Inside a microVM lo defaults to down, and the kernel backend relies on it
        # for ZMQ; bring it up best-effort, warn only on failure.
        try:
            _ensure_loopback_up()
        except OSError as exc:
            logger.warning("could not bring up loopback (kernel backend may be unavailable): %s", exc)
        # VMADDR_CID_ANY: listen on this VM's own vsock; the port is fixed, the client side CONNECTs to it.
        sock = socket.socket(socket.AF_VSOCK, socket.SOCK_STREAM)
        sock.bind((socket.VMADDR_CID_ANY, vsock_port))
        server = await asyncio.start_server(self.handle, sock=sock)
        logger.info("sandbox daemon listening on vsock:cid=ANY,port=%d", vsock_port)
        async with server:
            await server.serve_forever()


def main() -> None:
    parser = argparse.ArgumentParser(
        description="microsandbox daemon (runs inside the Firecracker microVM)"
    )
    parser.add_argument("--log-level", default="INFO")
    # The daemon now only ever runs inside the microVM: the control channel is
    # vsock and the execution backend is the stateful Jupyter kernel. These flags
    # are kept single-valued (rather than dropped) so the rootfs /init invocation
    # stays stable and self-documenting -- see scripts/build-rootfs.sh.
    parser.add_argument(
        "--transport", choices=["vsock"], default="vsock",
        help="control channel (fixed: HTTP over vsock)",
    )
    parser.add_argument(
        "--backend", choices=["kernel"], default="kernel",
        help="execution backend (fixed: resident Jupyter kernel, stateful REPL)",
    )
    parser.add_argument(
        "--vsock-port", type=int, default=1024,
        help="the vsock port the daemon listens on (the client side CONNECTs to it)",
    )
    args = parser.parse_args()

    logging.basicConfig(
        level=args.log_level,
        format="%(asctime)s %(levelname)s %(name)s %(message)s",
    )

    # Instantiation only validates dependencies (ipykernel/jupyter_client, which the
    # agent image preinstalls); the kernel itself is started lazily on first execute.
    try:
        backend: ExecutionBackend = JupyterKernelBackend()
    except RuntimeError as exc:
        parser.error(str(exc))
        return  # unreachable (parser.error raises SystemExit), but reassures type checkers

    server = SandboxServer(backend)
    try:
        asyncio.run(server.serve(vsock_port=args.vsock_port))
    except KeyboardInterrupt:
        logger.info("shutting down")


if __name__ == "__main__":
    main()
