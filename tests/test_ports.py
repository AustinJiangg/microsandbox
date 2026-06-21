"""Stage 12c: user-port exposure. A server the sandbox code starts on a guest port is reachable
from the host at <port>-<id> through client-proxy -- the same hostname mechanism that routes
envd (49983) and the code-interpreter (49999), now pointed at an arbitrary user port.

The microVM cases auto-skip without firecracker / KVM / the networking privilege (see conftest)."""

import urllib.request

from microsandbox import Sandbox


def _get(url: str, host: str, timeout: float = 10.0) -> bytes:
    """GET url but send a chosen Host header, bypassing any env proxy (the WSL autoproxy would
    otherwise grab the loopback / 10.x hops). Mirrors the SDK's own proxy-free transport."""
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    req = urllib.request.Request(url, headers={"Host": host})
    return opener.open(req, timeout=timeout).read()


def test_user_port_is_reachable(sandbox: Sandbox) -> None:
    """Start a tiny HTTP server inside the sandbox on :8000, then reach it from the host at
    get_host(8000) through client-proxy -- proving the <port>-<id> route exposes any guest port,
    not just the daemon's own ports."""
    # Run the server in a kernel background thread so run_code returns while it keeps serving.
    sandbox.run_code(
        "import http.server, threading\n"
        "class H(http.server.BaseHTTPRequestHandler):\n"
        "    def do_GET(self):\n"
        "        self.send_response(200); self.end_headers(); self.wfile.write(b'hello-from-sandbox')\n"
        "    def log_message(self, *a):\n"
        "        pass\n"
        "srv = http.server.HTTPServer(('0.0.0.0', 8000), H)\n"
        "threading.Thread(target=srv.serve_forever, daemon=True).start()\n"
    )

    # Reach it through client-proxy: connect to the data plane's address, but carry the route in
    # the Host header (8000-<id>) -- exactly what sandbox.get_host(8000) returns.
    host = sandbox.get_host(8000)
    assert host == f"8000-{sandbox._sandbox_id}"

    body = b""
    last_err: Exception | None = None
    for _ in range(20):  # the server thread + first connect can take a beat to settle
        try:
            body = _get(sandbox._data_url + "/", host)
            break
        except Exception as exc:  # noqa: BLE001 -- retry connection refused / reset while it warms
            last_err = exc
            import time

            time.sleep(0.25)
    else:
        raise AssertionError(f"user server never became reachable at {host}: {last_err}")

    assert body == b"hello-from-sandbox", f"got {body!r} via {host}"
