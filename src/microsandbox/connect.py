"""A tiny hand-rolled ConnectRPC client (Stage 11), JSON codec, over urllib.

The ConnectRPC protocol (connectrpc.com) runs over plain HTTP/1.1 -- which is why it can
ride the host->VM bridge unchanged (vsock at Stage 11, the VM's NIC/TCP since Stage 12):

  - unary: a normal POST of a JSON body, JSON response.
  - server-streaming: the request is one "enveloped" frame and the response is a stream of
    them. An envelope is [1 flag byte][4-byte big-endian length][payload]; the final frame
    has flag bit 0x02 (an EndStreamResponse carrying trailers / error).

Keeping this hand-rolled (rather than pulling in a protobuf runtime) matches the SDK's
stdlib-only style -- the same way it used to hand-parse the daemon's SSE. The Go services
use the real connect-go library; this lays the wire format bare on the client. See
docs/STAGE11_DESIGN.md.
"""

from __future__ import annotations

import json
import struct
import urllib.error
import urllib.request
from collections.abc import Iterator

_FLAG_END = 0x02


def unary(
    url: str,
    message: dict,
    headers: dict | None = None,
    timeout: float | None = None,
) -> dict:
    """Call a unary Connect RPC: POST the JSON message, return the JSON response (or {}).

    Connect unary with the JSON codec is just a plain POST of the message and a JSON reply;
    a non-2xx carries a Connect error ({code, message}), surfaced as a RuntimeError.
    """
    request = urllib.request.Request(
        url,
        data=json.dumps(message).encode(),
        method="POST",
        headers={
            "Content-Type": "application/json",
            "Connect-Protocol-Version": "1",
            **(headers or {}),
        },
    )
    try:
        with urllib.request.urlopen(request, timeout=timeout) as resp:
            raw = resp.read()
        return json.loads(raw) if raw else {}
    except urllib.error.HTTPError as exc:
        raise RuntimeError(f"{url} failed: {_error_detail(exc)}") from exc
    except urllib.error.URLError as exc:
        raise RuntimeError(
            f"cannot reach {url} ({exc.reason}); "
            "is it running? start the services with scripts/dev-up.sh"
        ) from exc


def server_stream(
    url: str,
    message: dict,
    headers: dict | None = None,
    timeout: float | None = None,
) -> Iterator[dict]:
    """Call a server-streaming Connect RPC, yielding each response message as a dict.

    `message` is the single request message (sent as one envelope). A mid-stream error
    arrives in the end frame and is raised as a RuntimeError; an unreachable / non-2xx
    endpoint is also a RuntimeError.
    """
    body = _envelope(json.dumps(message).encode())
    request = urllib.request.Request(
        url,
        data=body,
        method="POST",
        headers={
            "Content-Type": "application/connect+json",
            "Connect-Protocol-Version": "1",
            **(headers or {}),
        },
    )
    try:
        resp = urllib.request.urlopen(request, timeout=timeout)
    except urllib.error.HTTPError as exc:
        raise RuntimeError(f"{url} failed: {_error_detail(exc)}") from exc
    except urllib.error.URLError as exc:
        raise RuntimeError(
            f"cannot reach {url} ({exc.reason}); "
            "is it running? start the services with scripts/dev-up.sh"
        ) from exc

    with resp:
        while True:
            header = _read_exact(resp, 5)
            if len(header) < 5:
                return  # clean EOF between frames
            flags = header[0]
            (length,) = struct.unpack(">I", header[1:5])
            payload = _read_exact(resp, length)
            if flags & _FLAG_END:
                end = json.loads(payload) if payload else {}
                if end.get("error"):
                    raise RuntimeError("connect stream error: " + json.dumps(end["error"]))
                return
            yield json.loads(payload)


def _envelope(data: bytes) -> bytes:
    """Frame one message: a flags byte (0 = a normal message) + a 4-byte length + payload."""
    return struct.pack(">BI", 0, len(data)) + data


def _read_exact(resp, n: int) -> bytes:
    """Read exactly n bytes (or fewer at EOF). resp.read(k) may return short, so loop."""
    buf = b""
    while len(buf) < n:
        chunk = resp.read(n - len(buf))
        if not chunk:
            break
        buf += chunk
    return buf


def _error_detail(exc: urllib.error.HTTPError) -> str:
    """Pull a human message out of a non-2xx body (Connect unary {code,message}, or the
    control plane's {error})."""
    detail = exc.read().decode(errors="replace")
    try:
        obj = json.loads(detail)
        return obj.get("message") or obj.get("error") or detail
    except json.JSONDecodeError:
        return detail
