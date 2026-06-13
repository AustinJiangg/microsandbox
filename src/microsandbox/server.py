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
import pathlib
import socket
from collections.abc import AsyncIterator

from .backend import (
    DockerBackend,
    ExecutionBackend,
    JupyterKernelBackend,
    LocalSubprocessBackend,
)
from .protocol import (
    CommandRequest,
    EventType,
    ExecuteRequest,
    OutputEvent,
    PathRequest,
    WriteFileRequest,
)

logger = logging.getLogger("microsandbox.server")


def _ensure_loopback_up() -> None:
    """把 loopback 网卡（lo）置为 UP。

    microVM 里 lo 默认是 down 的，而 kernel 后端的 Jupyter kernel 走 ZMQ over 127.0.0.1
    跟 daemon 通信——lo 不 up 就连不上（表现为 kernel 启动 60s 超时）。极简 rootfs 里没装
    ip/ifconfig，所以直接用 SIOCSIFFLAGS ioctl 拉起（等价 `ip link set lo up`）。

    仅在 vsock（= 跑在 microVM 内）路径调用：容器/宿主的 lo 本就 up，无需也不该去动。
    这件事本质是 init/系统初始化的职责，但我们的 rootfs init 是极简 shell（无网络工具），
    而 daemon 是现成的 root 进程，放这里最省事且自带条件守卫。
    """
    import fcntl
    import struct

    IFF_UP = 0x1
    SIOCGIFFLAGS, SIOCSIFFLAGS = 0x8913, 0x8914
    sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    try:
        # struct ifreq：char name[16] + flags(short)，整体补齐到 sizeof(ifreq)=40。
        req = struct.pack("16sH22s", b"lo", 0, b"")
        flags = struct.unpack("16sH22s", fcntl.ioctl(sock.fileno(), SIOCGIFFLAGS, req))[1]
        req = struct.pack("16sH22s", b"lo", flags | IFF_UP, b"")
        fcntl.ioctl(sock.fileno(), SIOCSIFFLAGS, req)
    finally:
        sock.close()


class SandboxServer:
    """最小 HTTP 守护进程，基于 asyncio。端点：

        GET  /health        健康检查（阶段 2+ 用来探测沙箱是否就绪）
        POST /execute       执行代码，SSE 流式返回 OutputEvent
        POST /files/read    读文件        （阶段 2c）
        POST /files/write   写文件        （阶段 2c）
        POST /files/list    列目录        （阶段 2c）
        POST /commands      跑 shell 命令  （阶段 2c）

    阶段 2c 的文件/命令端点由 daemon 直接在自身文件系统上完成（不经 backend）——
    对齐 E2B envd：文件/进程服务与跑代码的 kernel 是分开的。
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

            if method == "POST":
                # 所有 POST 端点都带 JSON body，统一读一次再按 path 分发。
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

    # ----- 阶段 2c：文件 / shell 端点 -----
    # 都由 daemon 直接在自身所在的文件系统/环境里完成（不经 ExecutionBackend）——
    # 对 container/kernel 后端就是容器内（沙箱里），对 local 后端就是宿主。
    #
    # 路径限制：这里**有意不做**任何路径白名单/校验。container/kernel 后端下 daemon
    # 只能看到容器自己的文件，「容器边界」就是天然的 confinement，再加人为限制反而会
    # 挡掉合法读取（如 /etc/os-release）。唯一不安全的是 local 后端（能碰宿主任意文件）
    # ——这与 local 一贯「无隔离、严禁对外」的定位一致，不是这里的新问题。

    async def _handle_file_read(
        self, writer: asyncio.StreamWriter, raw_body: str
    ) -> None:
        try:
            req = PathRequest.from_json(raw_body)
        except (json.JSONDecodeError, KeyError) as exc:
            await self._write_json(writer, 400, {"error": f"bad request: {exc}"})
            return
        try:
            # 小文件直接读，阻塞极短。大文件/高并发再考虑 run_in_executor（阶段 4）。
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
            p.parent.mkdir(parents=True, exist_ok=True)  # 顺手建中间目录（如写 /tmp/a/b.txt）
            p.write_text(req.content)
        except OSError as exc:
            # 常见：常驻容器 --read-only 根，写 /tmp 之外会到这里。如实回吐原因。
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
        # 在 daemon 所在环境跑 shell（container/kernel 后端 = 容器内 = 沙箱里）。
        # 非流式：跑完一次性把 stdout/stderr/exit_code 收回去（流式可作为后续扩展）。
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

    async def serve(
        self,
        host: str = "127.0.0.1",
        port: int = 49152,
        *,
        transport: str = "tcp",
        vsock_port: int = 1024,
    ) -> None:
        # 阶段 3：daemon 跑在 microVM 里时，控制通道走 vsock 而非 TCP（宿主经 Firecracker
        # 的 UDS 连进来，见 docs/STAGE3_DESIGN.md §4.1）。除了「监听哪种 socket」不同，
        # handle / 分发 / backend 全不变——这正是「协议稳定、传输可换」的体现。
        if transport == "vsock":
            # microVM 里 lo 默认 down，kernel 后端要靠它走 ZMQ；best-effort 拉起，失败只告警。
            try:
                _ensure_loopback_up()
            except OSError as exc:
                logger.warning("无法拉起 loopback（kernel 后端可能不可用）：%s", exc)
            sock = socket.socket(socket.AF_VSOCK, socket.SOCK_STREAM)
            # VMADDR_CID_ANY：监听本 VM 自己的 vsock；端口固定，client 侧 CONNECT 它。
            sock.bind((socket.VMADDR_CID_ANY, vsock_port))
            server = await asyncio.start_server(self.handle, sock=sock)
            addr = f"vsock:cid=ANY,port={vsock_port}"
        else:
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
        choices=["local", "docker", "kernel"],
        default="local",
        help=(
            "执行后端：local=本机子进程（无隔离）；docker=一次性容器（阶段 1）；"
            "kernel=常驻 Jupyter kernel，有状态 REPL（阶段 2b，需在 agent 镜像内运行）"
        ),
    )
    # 阶段 3：传输方式与执行后端正交——backend 决定「怎么跑代码」，transport 决定
    # 「client 怎么连进来」。microVM 内用 vsock，容器/宿主仍用 tcp。
    parser.add_argument(
        "--transport",
        choices=["tcp", "vsock"],
        default="tcp",
        help="控制通道：tcp=HTTP over TCP（阶段 0~2）；vsock=HTTP over vsock（阶段 3 microVM 内）",
    )
    parser.add_argument(
        "--vsock-port",
        type=int,
        default=1024,
        help="vsock 传输时 daemon 监听的 vsock 端口（client 侧 CONNECT 它）",
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
    elif args.backend == "kernel":
        # 同理，缺 ipykernel/jupyter_client 时启动期就报清楚（一般只会在非 agent
        # 镜像里误用 --backend kernel 才触发）。实例化只验证依赖，kernel 是懒启动的。
        try:
            backend = JupyterKernelBackend()
        except RuntimeError as exc:
            parser.error(str(exc))
    else:
        backend = LocalSubprocessBackend()

    server = SandboxServer(backend)
    try:
        asyncio.run(
            server.serve(
                args.host,
                args.port,
                transport=args.transport,
                vsock_port=args.vsock_port,
            )
        )
    except KeyboardInterrupt:
        logger.info("shutting down")


if __name__ == "__main__":
    main()
