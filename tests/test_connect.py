"""Stage 11 unit tests for the hand-rolled Connect server-streaming client (connect.py).

A tiny local HTTP server emits Connect enveloped frames; we assert the client parses the
message frames and the end frame (including an error end frame). Pure stdlib -- no VM,
no daemon -- so it runs everywhere, isolating the fiddly framing from the real kernel path.
"""

import json
import struct
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest

from microsandbox.connect import server_stream


def _frame(payload: bytes, end: bool = False) -> bytes:
    """One Connect envelope: [flags][4-byte big-endian length][payload]; flags 0x02 = end."""
    return struct.pack(">BI", 0x02 if end else 0x00, len(payload)) + payload


def _serve(frames: bytes) -> str:
    """Start a one-shot HTTP server returning `frames` as the (streamed) response body."""

    class Handler(BaseHTTPRequestHandler):
        def do_POST(self):
            self.rfile.read(int(self.headers.get("Content-Length", 0)))  # drain the request
            self.send_response(200)
            self.send_header("Content-Type", "application/connect+json")
            self.end_headers()
            self.wfile.write(frames)

        def log_message(self, *args):
            pass

    srv = HTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=srv.handle_request, daemon=True).start()
    return f"http://127.0.0.1:{srv.server_address[1]}/x"


def test_server_stream_yields_messages_then_end():
    frames = (
        _frame(json.dumps({"type": "stdout", "data": "hi"}).encode())
        + _frame(json.dumps({"type": "end", "exitCode": 0}).encode())
        + _frame(b"{}", end=True)
    )
    got = list(server_stream(_serve(frames), {"code": "x"}))
    assert got == [{"type": "stdout", "data": "hi"}, {"type": "end", "exitCode": 0}]


def test_server_stream_raises_on_error_end_frame():
    frames = _frame(
        json.dumps({"error": {"code": "internal", "message": "boom"}}).encode(), end=True
    )
    with pytest.raises(RuntimeError, match="boom"):
        list(server_stream(_serve(frames), {"code": "x"}))
