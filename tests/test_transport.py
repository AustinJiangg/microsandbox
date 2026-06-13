"""阶段 3a 单元测试：vsock 传输层的握手 + HTTP 帧编解码（不依赖 microVM）。

_VsockTransport 要做两件容易错的事：① Firecracker 的 `CONNECT <port>` 文本握手；
② 在裸 socket 上手写/解析最小 HTTP/1.1（含 SSE 流式）。这里用一个本地 AF_UNIX
服务器**假扮 Firecracker 的 vsock UDS**，喂固定字节、断言 transport 的行为，从而在
没有 KVM/VM 的机器上也能验证这层逻辑——缺 firecracker 的环境照样能跑这组测试。

这正是把传输从 client 里抽出来的红利：不必起一整台 VM，就能给最易错的那段字节处理
上单元测试的安全网，跟阶段 3b「真起 microVM」的端到端测试互补。
"""

import json
import socket
import threading

import pytest

from microsandbox.client import _VsockTransport


def _serve_once(uds_path: str, handler) -> threading.Thread:
    """起一个只接一条连接的 AF_UNIX 服务器，模拟 Firecracker 的 vsock UDS。

    handler(conn) 负责完成握手与响应；在后台 daemon 线程里跑，返回该线程，
    测试结束用 join 等它收尾、好对其内部断言负责。
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
    """读完一个 HTTP 请求：请求行 + 头 + 按 Content-Length 读 body。

    返回 (start_line, headers_dict, body_bytes)，供 handler 断言 client 发出的字节。
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
    """单发 JSON：CONNECT 握手 → 发请求 → 收 200 + JSON body，且请求字节原样送达。"""
    uds = str(tmp_path / "fc.vsock")
    seen: dict[str, object] = {}

    def handler(conn: socket.socket) -> None:
        rfile = conn.makefile("rb")
        seen["connect"] = rfile.readline()          # 期望 b"CONNECT 1024\n"
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

    assert seen["connect"] == b"CONNECT 1024\n"            # 握手端口正确
    assert seen["start"] == "POST /files/read HTTP/1.1"     # 请求行正确
    assert json.loads(seen["body"]) == {"path": "/tmp/x"}   # body 原样送达


def test_vsock_sse_streaming(tmp_path) -> None:
    """流式 SSE：transport 把响应体逐行交给上层，能解析出多个事件（对齐 /execute）。"""
    uds = str(tmp_path / "fc.vsock")

    def handler(conn: socket.socket) -> None:
        rfile = conn.makefile("rb")
        rfile.readline()                 # 读掉 CONNECT
        conn.sendall(b"OK 1024\n")
        _read_request(rfile)             # 读掉请求（body 是 b"{}"）
        conn.sendall(
            b"HTTP/1.1 200 OK\r\n"
            b"Content-Type: text/event-stream\r\n"
            b"Connection: close\r\n\r\n"
        )
        conn.sendall(b'data: {"type": "stdout", "data": "hello\\n"}\n\n')
        conn.sendall(b'data: {"type": "end", "exit_code": 0}\n\n')
        # handler 返回 → with conn 关闭 → client 读到 EOF 结束迭代

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
    """握手失败（Firecracker 回非 OK，如 guest 端口没人监听）：应抛错而非默默挂起。"""
    uds = str(tmp_path / "fc.vsock")

    def handler(conn: socket.socket) -> None:
        conn.makefile("rb").readline()   # 读掉 CONNECT
        conn.sendall(b"FAILED\n")         # 模拟连接被拒

    t = _serve_once(uds, handler)
    transport = _VsockTransport(uds, 1024)
    with pytest.raises(ConnectionError):
        transport.request("GET", "/health", timeout=2)
    t.join(timeout=5)
