"""客户端 SDK —— 用户实际编写代码时面对的接口。

设计目标：手感尽量贴近 E2B，这样你学完自己的实现后，回头看 E2B 的
SDK 会很有亲切感。典型用法：

    from microsandbox import Sandbox

    with Sandbox() as sandbox:
        execution = sandbox.run_code("print('hello from sandbox')")
        print(execution.stdout)

阶段 0：Sandbox 直接连本机守护进程（你得先手动起 server，或用 connect=False
        让它自动拉起一个子进程守护进程，见下）。
阶段 1+：Sandbox 的职责会扩展为「向控制面申请一个新沙箱、拿到它的地址、
        再连上去」。但 run_code / 流式消费这套上层 API 对用户保持不变。
"""

from __future__ import annotations

import json
import subprocess
import sys
import time
import urllib.request
from collections.abc import Callable, Iterator
from typing import Any

from .protocol import EventType, Execution, ExecuteRequest, OutputEvent

# client 与守护进程之间是点对点的内部信道，绝不应被环境代理截胡。
# urllib 默认会读取 http_proxy 等环境变量，而 Python 对 no_proxy 只做
# 精确/后缀匹配，不认 Windows 风格通配符（如 "127.*"、"<local>"）——
# 在 WSL2 这类继承宿主代理配置的环境里，连 127.0.0.1 的请求都会被转给
# 本地代理，代理收尾连接时偶发 RST，导致测试随机 ConnectionResetError。
# 这里显式构造「无代理」opener，client 的所有请求都走它。
_DIRECT_OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))


class Sandbox:
    """一个沙箱会话的客户端句柄。

    参数：
        host / port：守护进程地址。
        spawn_local：阶段 0 便利开关。若为 True，构造时自动在本机拉起一个
                     守护进程子进程并连上它，退出时自动关闭 —— 让你一行代码
                     就能跑起来，不用先手动开 server。阶段 1+ 会用真正的
                     「创建沙箱」逻辑替换它。
    """

    def __init__(
        self,
        host: str = "127.0.0.1",
        port: int = 49152,
        spawn_local: bool = True,
        timeout_seconds: float = 30.0,
    ) -> None:
        self.host = host
        self.port = port
        self.timeout_seconds = timeout_seconds
        self._proc: subprocess.Popen | None = None

        if spawn_local:
            self._spawn_daemon()
            self._wait_until_healthy()

    # ----- 生命周期 -----

    def _spawn_daemon(self) -> None:
        """阶段 0：本机拉起守护进程子进程。

        阶段 1 替换思路：这里改成「调用 Docker SDK 创建容器」；
        阶段 3 改成「通过 orchestrator 启动 microVM」。返回的地址再传给后续请求。
        """
        self._proc = subprocess.Popen(
            [sys.executable, "-m", "microsandbox.server",
             "--host", self.host, "--port", str(self.port),
             "--log-level", "WARNING"],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )

    def _wait_until_healthy(self, attempts: int = 50, delay: float = 0.1) -> None:
        url = f"http://{self.host}:{self.port}/health"
        for _ in range(attempts):
            try:
                with _DIRECT_OPENER.open(url, timeout=1) as resp:
                    if resp.status == 200:
                        return
            except Exception:
                time.sleep(delay)
        raise RuntimeError("sandbox daemon did not become healthy in time")

    def close(self) -> None:
        if self._proc is not None:
            self._proc.terminate()
            try:
                self._proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self._proc.kill()
            self._proc = None

    def __enter__(self) -> "Sandbox":
        return self

    def __exit__(self, *exc: Any) -> None:
        self.close()

    # ----- 核心 API -----

    def run_code(
        self,
        code: str,
        language: str = "python",
        on_stdout: Callable[[str], None] | None = None,
        on_stderr: Callable[[str], None] | None = None,
    ) -> Execution:
        """执行一段代码，返回聚合后的 Execution 结果。

        可选传入 on_stdout / on_stderr 回调，实时拿到流式输出
        （例如边跑边打印），同时最终仍返回完整 Execution。
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
        """向守护进程发起执行请求，逐行解析 SSE 流。"""
        request = ExecuteRequest(
            code=code, language=language, timeout_seconds=self.timeout_seconds
        )
        req = urllib.request.Request(
            f"http://{self.host}:{self.port}/execute",
            data=request.to_json().encode(),
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        with _DIRECT_OPENER.open(req) as resp:
            for raw in resp:
                line = raw.decode(errors="replace").rstrip("\n")
                if not line.startswith("data: "):
                    continue
                payload = line[len("data: "):]
                if not payload:
                    continue
                yield OutputEvent.from_sse_payload(payload)
