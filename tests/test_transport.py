"""Stage 3a unit tests: the vsock transport's handshake + HTTP frame encode/decode (no microVM dependency).

_VsockTransport does two error-prone things: (1) Firecracker's `CONNECT <port>`
text handshake; (2) hand-writing/parsing minimal HTTP/1.1 over a raw socket
(including SSE streaming). Here a local AF_UNIX server **impersonates Firecracker's
vsock UDS**, feeding fixed bytes and asserting the transport's behavior, so this
logic can be verified even on machines without KVM/VM -- this group of tests still
runs in environments lacking firecracker.

This is exactly the payoff of extracting the transport out of the client: without
booting a whole VM, you can put a unit-test safety net on the most error-prone
byte-handling stretch, complementing the Stage 3b "really boot a microVM"
end-to-end tests.
"""

import json
import socket
import threading

import pytest

from microsandbox.client import _VsockTransport


def _serve_once(uds_path: str, handler) -> threading.Thread:
    """Start an AF_UNIX server that accepts exactly one connection, simulating Firecracker's vsock UDS.

    handler(conn) is responsible for completing the handshake and response; it runs
    in a background daemon thread, and the thread is returned so the test can join
    it at the end to wait for it to wind down and account for its internal assertions.
    """
    srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    srv.bind(uds_path)
    srv.listen(1)

    def run() -> None:
        try:
            conn, _ = srv.accept()
            with conn:
                handler(conn)
        finally:
            srv.close()

    t = threading.Thread(target=run, daemon=True)
    t.start()
    return t


def _read_request(rfile):
    """Read one full HTTP request: request line + headers + body per Content-Length.

    Returns (start_line, headers_dict, body_bytes), for the handler to assert on the
    bytes the client sent.
    """
    start = rfile.readline().decode().rstrip("\r\n")
    headers = {}
    while True:
        line = rfile.readline()
        if line in (b"\r\n", b"\n", b""):
            break
        key, _, value = line.decode().partition(":")
        headers[key.strip().lower()] = value.strip()
    length = int(headers.get("content-length", "0"))
    body = rfile.read(length) if length else b""
    return start, headers, body


def test_vsock_handshake_and_json(tmp_path) -> None:
    """Single-shot JSON: CONNECT handshake -> send request -> receive 200 + JSON body, with the request bytes delivered verbatim."""
    uds = str(tmp_path / "fc.vsock")
    seen: dict[str, object] = {}

    def handler(conn: socket.socket) -> None:
        rfile = conn.makefile("rb")
        seen["connect"] = rfile.readline()          # expect b"CONNECT 1024\n"
        conn.sendall(b"OK 1024\n")
        start, _headers, body = _read_request(rfile)
        seen["start"] = start
        seen["body"] = body
        payload = b'{"content": "hi"}'
        conn.sendall(
            b"HTTP/1.1 200 OK\r\n"
            b"Content-Type: application/json\r\n"
            b"Content-Length: %d\r\n"
            b"Connection: close\r\n\r\n" % len(payload)
            + payload
        )

    t = _serve_once(uds, handler)
    transport = _VsockTransport(uds, 1024)
    with transport.request(
        "POST",
        "/files/read",
        body=b'{"path": "/tmp/x"}',
        headers={"Content-Type": "application/json"},
    ) as resp:
        assert resp.status == 200
        assert json.loads(resp.read()) == {"content": "hi"}
    t.join(timeout=5)

    assert seen["connect"] == b"CONNECT 1024\n"            # handshake port is correct
    assert seen["start"] == "POST /files/read HTTP/1.1"     # request line is correct
    assert json.loads(seen["body"]) == {"path": "/tmp/x"}   # body delivered verbatim


def test_vsock_sse_streaming(tmp_path) -> None:
    """Streaming SSE: the transport hands the response body to the upper layer line by line, parsing out multiple events (aligned with /execute)."""
    uds = str(tmp_path / "fc.vsock")

    def handler(conn: socket.socket) -> None:
        rfile = conn.makefile("rb")
        rfile.readline()                 # consume CONNECT
        conn.sendall(b"OK 1024\n")
        _read_request(rfile)             # consume the request (body is b"{}")
        conn.sendall(
            b"HTTP/1.1 200 OK\r\n"
            b"Content-Type: text/event-stream\r\n"
            b"Connection: close\r\n\r\n"
        )
        conn.sendall(b'data: {"type": "stdout", "data": "hello\\n"}\n\n')
        conn.sendall(b'data: {"type": "end", "exit_code": 0}\n\n')
        # handler returns -> with conn closes -> client reads EOF and ends iteration

    t = _serve_once(uds, handler)
    transport = _VsockTransport(uds, 1024)
    lines: list[str] = []
    with transport.request("POST", "/execute", body=b"{}") as resp:
        assert resp.status == 200
        for raw in resp:
            lines.append(raw.decode().rstrip("\n"))
    t.join(timeout=5)

    payloads = [l[len("data: "):] for l in lines if l.startswith("data: ")]
    events = [json.loads(p) for p in payloads]
    assert {"type": "stdout", "data": "hello\n"} in events
    assert {"type": "end", "exit_code": 0} in events


def test_vsock_connect_rejected(tmp_path) -> None:
    """Handshake failure (Firecracker returns non-OK, e.g. nothing listening on the guest port): should raise rather than silently hang."""
    uds = str(tmp_path / "fc.vsock")

    def handler(conn: socket.socket) -> None:
        conn.makefile("rb").readline()   # consume CONNECT
        conn.sendall(b"FAILED\n")         # simulate the connection being refused

    t = _serve_once(uds, handler)
    transport = _VsockTransport(uds, 1024)
    with pytest.raises(ConnectionError):
        transport.request("GET", "/health", timeout=2)
    t.join(timeout=5)
