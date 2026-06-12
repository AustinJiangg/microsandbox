"""沙箱守护进程（daemon / server）。

它在沙箱「内部」运行，监听 HTTP 请求，把代码交给执行后端跑，
再用 SSE 把输出流式返回给客户端。

阶段 0/1：这个进程跑在你本机（阶段 1 的隔离发生在 backend 起的容器里）。
阶段 2+：这个进程会被打包进容器/VM 镜像，作为常驻 agent 运行 ——
        对应 E2B 里的 `envd`。届时 client 连接的 URL 从 localhost
        变成容器/VM 的地址，server 代码本身基本不用改。

只用标准库实现，避免阶段 0 就引入一堆依赖。阶段 1+ 若觉得手写 HTTP
太繁琐，可以换成 FastAPI/uvicorn，接口契约（协议）保持不变即可。
"""

from __future__ import annotations

import argparse
import asyncio
import json
import logging
from collections.abc import AsyncIterator

from .backend import DockerBackend, ExecutionBackend, LocalSubprocessBackend
from .protocol import EventType, ExecuteRequest, OutputEvent

logger = logging.getLogger("microsandbox.server")


class SandboxServer:
    """最小 HTTP 守护进程，基于 asyncio，只暴露两个端点：

        GET  /health        健康检查（阶段 2+ 用来探测沙箱是否就绪）
        POST /execute       执行代码，SSE 流式返回 OutputEvent
    """

    def __init__(self, backend: ExecutionBackend | None = None) -> None:
        # 依赖抽象接口而非具体实现：换隔离方案时只改这一行的默认值。
        self.backend = backend or LocalSubprocessBackend()

    async def handle(
        self, reader: asyncio.StreamReader, writer: asyncio.StreamWriter
    ) -> None:
        try:
            request_line = await reader.readline()
            if not request_line:
                return
            method, path, _ = request_line.decode().split(" ", 2)

            # 读 headers，拿到 Content-Length 以便读 body
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

            if method == "POST" and path == "/execute":
                length = int(headers.get("content-length", "0"))
                body = await reader.readexactly(length) if length else b"{}"
                await self._handle_execute(writer, body.decode())
                return

            await self._write_json(writer, 404, {"error": "not found"})
        except Exception as exc:  # noqa: BLE001 - 守护进程不应因单个请求崩溃
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

        # 先写 SSE 响应头，然后逐事件 flush，实现流式。
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

    async def serve(self, host: str = "127.0.0.1", port: int = 49152) -> None:
        server = await asyncio.start_server(self.handle, host, port)
        addr = ", ".join(str(s.getsockname()) for s in server.sockets)
        logger.info("sandbox daemon listening on %s", addr)
        async with server:
            await server.serve_forever()


def main() -> None:
    parser = argparse.ArgumentParser(description="microsandbox daemon")
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=49152)
    parser.add_argument("--log-level", default="INFO")
    parser.add_argument(
        "--backend",
        choices=["local", "docker"],
        default="local",
        help="执行后端：local=本机子进程（无隔离）；docker=一次性容器（阶段 1）",
    )
    args = parser.parse_args()

    logging.basicConfig(
        level=args.log_level,
        format="%(asctime)s %(levelname)s %(name)s %(message)s",
    )

    if args.backend == "docker":
        # 启动期就把环境问题（docker 没装/没起/镜像缺失）暴露给起 daemon 的人，
        # 并给出可操作的中文指引——而不是等第一次执行才埋进 SSE 流里。
        problem = DockerBackend.check_available()
        if problem:
            parser.error(problem)
        backend: ExecutionBackend = DockerBackend()
    else:
        backend = LocalSubprocessBackend()

    server = SandboxServer(backend)
    try:
        asyncio.run(server.serve(args.host, args.port))
    except KeyboardInterrupt:
        logger.info("shutting down")


if __name__ == "__main__":
    main()
