"""客户端 SDK —— 用户实际编写代码时面对的接口。

设计目标：手感尽量贴近 E2B，这样你学完自己的实现后，回头看 E2B 的
SDK 会很有亲切感。典型用法：

    from microsandbox import Sandbox

    with Sandbox() as sandbox:
        execution = sandbox.run_code("print('hello from sandbox')")
        print(execution.stdout)

阶段 0/1：Sandbox 自动在本机拉起 daemon 子进程并连上（spawn_local=True）。
        阶段 1 用 backend="docker" 让 daemon 把代码放进一次性容器里执行——
        一行切换隔离方案，run_code 的用法完全不变。
阶段 2+：Sandbox 的职责会扩展为「向控制面申请一个新沙箱、拿到它的地址、
        再连上去」。但 run_code / 流式消费这套上层 API 对用户保持不变。
"""

from __future__ import annotations

import json
import pathlib
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request
import uuid
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

# ---- 阶段 2：常驻容器拓扑 ----
# 这些 backend 取值代表「主从关系反转」：daemon 不再跑在宿主，而是被 client
# docker run -d 进一个长期存活的容器里常驻（对应 E2B 的 envd）。
# "container"（2a）容器内是无状态子进程；"kernel"（2b）容器内是常驻 Jupyter
# kernel，变量跨 run_code 留存。两者共用同一套「起常驻容器」代码，差别只在
# 用哪个镜像、以及告诉容器内 daemon 用哪个 --backend（见 _spawn_resident_container）。
_RESIDENT_BACKENDS = {"container", "kernel"}
# client 的 backend 取值 → 容器内 daemon 实际用哪个 --backend 启动。
_INNER_BACKEND = {"container": "local", "kernel": "kernel"}
# 容器内 daemon 固定监听这个端口；宿主侧映射到一个随机空闲端口（见 _find_free_port），
# 这样多个沙箱、以及宿主上可能已存在的 daemon 都不会撞端口。
_CONTAINER_PORT = 49152
# 容器内挂载源码的位置（2a 不建镜像，把宿主 src/ 只读挂进来即可跑我们的包）。
_CONTAINER_SRC = "/opt/microsandbox/src"


def _find_free_port() -> int:
    """绑到 :0 让内核分配一个空闲端口，立刻释放，把端口号交给 docker -p 用。

    释放到 docker 真正绑定之间有极小的竞态窗口；阶段 2a 够用，
    更稳的端口/沙箱池管理是阶段 4 的事。
    """
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


class Sandbox:
    """一个沙箱会话的客户端句柄。

    参数：
        host / port：守护进程地址。
        spawn_local：便利开关。若为 True，构造时自动在本机拉起一个守护进程
                     子进程并连上它，退出时自动关闭 —— 让你一行代码就能跑起来，
                     不用先手动开 server。阶段 2+ 会用真正的「创建沙箱」逻辑替换它。
        backend：执行/部署策略，不进 wire protocol。
                 - "local"（阶段 0）：宿主子进程，几乎无隔离。
                 - "docker"（阶段 1）：宿主 daemon，每次执行起一个一次性容器。
                 - "container"（阶段 2a）：daemon 搬进一个**常驻容器**里跑（envd 化），
                   client 负责 docker run 起它、退出时 docker rm -f。容器内用无状态
                   子进程后端。
                 - "kernel"（阶段 2b）：同样是常驻容器，但容器内 daemon 托管一个常驻
                   Jupyter kernel——变量跨 run_code 留存，真正的有状态 REPL（对齐 E2B）。
                   需先构建 agent 镜像：docker build -t microsandbox-agent .
    """

    def __init__(
        self,
        host: str = "127.0.0.1",
        port: int = 49152,
        spawn_local: bool = True,
        timeout_seconds: float = 30.0,
        backend: str = "local",
    ) -> None:
        self.host = host
        self.port = port
        self.timeout_seconds = timeout_seconds
        self.backend = backend
        self._proc: subprocess.Popen | None = None   # 宿主 daemon 子进程（阶段 0/1）
        self._container: str | None = None           # 常驻沙箱容器名（阶段 2）

        # 阶段 2c：文件 / shell 两个命名空间，手感对齐 E2B 的 sandbox.files / sandbox.commands。
        # 它们只在被调用时才用到 host/port，所以这里先建好即可（顺序无所谓）。
        self.files = _Files(self)
        self.commands = _Commands(self)

        if spawn_local:
            # 阶段 2 的「反转」就体现在这个分叉：常驻容器后端由 client 亲手
            # docker run 起一个容器把 daemon 装进去；其余后端沿用阶段 0/1 的
            # 宿主子进程。两条路都连同一个 daemon、说同一套协议。
            #
            # 兜底：_spawn_* 成功后容器/子进程就已经起来了，若紧接着的健康检查
            # 失败（daemon 起不来/起太慢），异常会从 __init__ 穿出去——此时 with
            # 语句还没进 __enter__，__exit__ 永远不会触发，已起的容器就成了没人收的
            # 残留。所以这里自己 close 掉再把异常抛上去（close 幂等，安全）。
            try:
                if backend in _RESIDENT_BACKENDS:
                    self._spawn_resident_container()
                else:
                    self._spawn_daemon()
                self._wait_until_healthy()
            except Exception:
                self.close()
                raise

    # ----- 生命周期 -----

    def _spawn_daemon(self) -> None:
        """本机拉起守护进程子进程（阶段 0/1：daemon 留在宿主机）。

        阶段 1 的隔离不发生在这里，而在 daemon 内部的 DockerBackend——
        client 只负责把 --backend 开关带给 daemon，自己完全不懂 docker。
        阶段 2 没有改这条路，而是**新增**了一条 _spawn_resident_container（把
        daemon 装进常驻容器，对应 E2B 的 envd）与它并存；阶段 3 会再加一条
        「通过 orchestrator 启动 microVM」。三条路连的都是同一个 daemon。
        """
        self._proc = subprocess.Popen(
            [sys.executable, "-m", "microsandbox.server",
             "--host", self.host, "--port", str(self.port),
             "--backend", self.backend,
             "--log-level", "WARNING"],
            stdout=subprocess.DEVNULL,
            # stderr 留管道：daemon 若启动即失败（如 docker 不可用），要把原因
            # 带给用户。WARNING 级日志量极小，不会撑满 64KB 的管道缓冲。
            stderr=subprocess.PIPE,
        )

    def _spawn_resident_container(self) -> None:
        """阶段 2a：docker run -d 起一个常驻容器，把 daemon 跑在里面。

        对比上面的 _spawn_daemon（在宿主 Popen 一个进程）：这里隔离环境（容器）
        由 client 亲手创建并长期持有——这就是阶段 2 的「主从关系反转」。
        注意 daemon 的代码（server.py）一行没改，只是换了运行的地方：这正是
        三层解耦想证明的事。

        2a 刻意不建镜像：把宿主的 src/ 只读挂载进官方 python:3.12-slim 即可让
        容器跑我们的包，改代码免重建。等阶段 2b 引入 jupyter 依赖时，才会改用
        Dockerfile 把依赖烘进 agent 镜像。
        """
        # 按后端选镜像：container 用官方 slim（2a，零依赖）；kernel 用 agent 镜像
        # （2b，预装了 ipykernel/jupyter_client，需先 docker build -t microsandbox-agent .）。
        from .backend import DEFAULT_AGENT_IMAGE, DEFAULT_DOCKER_IMAGE, DockerBackend

        image = {
            "container": DEFAULT_DOCKER_IMAGE,
            "kernel": DEFAULT_AGENT_IMAGE,
        }[self.backend]

        # 起容器前先把环境问题（docker 没装/没起/镜像缺失）暴露出来并给可操作指引，
        # 而不是等 docker run 失败后从日志里猜。复用阶段 1 的同一套检查。
        problem = DockerBackend.check_available(image)
        if problem:
            raise RuntimeError(f"无法创建常驻沙箱容器：{problem}")

        # 容器统一前缀命名：close 时按名 docker rm -f，残留时也能按前缀一键清理
        # （docker ps -a --filter name=microsandbox-sandbox -q | xargs -r docker rm -f）。
        self._container = f"microsandbox-sandbox-{uuid.uuid4().hex[:12]}"
        self.host = "127.0.0.1"
        self.port = _find_free_port()  # 宿主侧随机空闲端口，避免多沙箱/与宿主 daemon 撞端口

        src_dir = pathlib.Path(__file__).resolve().parents[1]  # .../src
        inner_backend = _INNER_BACKEND[self.backend]

        cmd = [
            "docker", "run", "-d",
            "--name", self._container,
            "--pull", "never",                 # 镜像缺失立刻报错，绝不在创建路径里隐式拉
            # 只把管理端口发布到本机回环：宿主能连，外网连不到
            "-p", f"127.0.0.1:{self.port}:{_CONTAINER_PORT}",
            # 资源限制作用于整个沙箱容器（daemon + 其内所有执行共享同一 cgroup），
            # 这正是 E2B 的 per-sandbox 限额模型——而非阶段 1 的 per-execution。
            # pids-limit 取 128（比 DockerBackend 的 64 大）：阶段 1 的容器只跑一个
            # python，而这里容器内还常驻着 daemon，再加上每次执行的子进程，pid 占用更高。
            "--memory", "256m", "--memory-swap", "256m",
            "--cpus", "1.0", "--pids-limit", "128",
            "--read-only", "--tmpfs", "/tmp:rw,size=64m",
            "-v", f"{src_dir}:{_CONTAINER_SRC}:ro",       # 只读挂载源码
            "-e", f"PYTHONPATH={_CONTAINER_SRC}",
            "-e", "PYTHONDONTWRITEBYTECODE=1",            # 只读根下别尝试写 .pyc
            "-e", "PYTHONUNBUFFERED=1",
            # HOME 指到可写的 tmpfs：kernel 后端下，Jupyter 要写 kernel 连接文件、
            # IPython 要写历史 sqlite，默认都落在 HOME 下，而根是 --read-only 的。
            # 对 container 后端无害。
            "-e", "HOME=/tmp",
            image,
            # daemon 必须监听 0.0.0.0 才能被宿主经映射端口连到（默认 127.0.0.1 在容器内
            # 等于谁都连不进来）。这是 server 端唯一的「配置」差异，代码本身不变。
            "python", "-m", "microsandbox.server",
            "--host", "0.0.0.0", "--port", str(_CONTAINER_PORT),
            "--backend", inner_backend, "--log-level", "WARNING",
        ]
        proc = subprocess.run(cmd, capture_output=True, text=True)
        if proc.returncode != 0:
            self._container = None  # 没起成功就别在 close 里去删一个不存在的名字
            raise RuntimeError(f"docker run 启动常驻容器失败：{proc.stderr.strip()}")

    def _wait_until_healthy(self, attempts: int = 50, delay: float = 0.1) -> None:
        url = f"http://{self.host}:{self.port}/health"
        for _ in range(attempts):
            # daemon 进程已退出就别傻等满 5 秒了——立刻读出它的 stderr 作为报错。
            # 例如 docker 后端在镜像缺失时，daemon 启动期检查会直接打印原因并退出。
            if self._proc is not None and self._proc.poll() is not None:
                detail = ""
                if self._proc.stderr is not None:
                    detail = self._proc.stderr.read().decode(errors="replace").strip()
                raise RuntimeError(
                    f"sandbox daemon exited at startup: {detail[-500:] or '(no stderr)'}"
                )
            try:
                with _DIRECT_OPENER.open(url, timeout=1) as resp:
                    if resp.status == 200:
                        return
            except Exception:
                time.sleep(delay)
        # 没在限定时间内就绪：常驻容器把日志尾巴补进报错，便于排查（宿主 daemon
        # 走上面 poll 分支提前抛出，到不了这里）。
        detail = self._container_logs()
        raise RuntimeError(
            "sandbox daemon did not become healthy in time"
            + (f": {detail[-500:]}" if detail else "")
        )

    def _container_logs(self) -> str:
        """取常驻容器的日志尾巴，仅用于启动失败时的报错。任何失败都返回空串。"""
        if self._container is None:
            return ""
        try:
            out = subprocess.run(
                ["docker", "logs", "--tail", "20", self._container],
                capture_output=True, text=True, timeout=5,
            )
            return (out.stdout + out.stderr).strip()
        except Exception:
            return ""

    def close(self) -> None:
        if self._proc is not None:
            self._proc.terminate()
            try:
                self._proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self._proc.kill()
            self._proc = None
        if self._container is not None:
            # 杀死并删除整个常驻沙箱容器。对比阶段 1 的超时清理（杀的是一次性执行
            # 容器），这里销毁的是沙箱本身。docker rm -f 一并 stop+rm；容器已不存在
            # 等失败一律忽略（幂等兜底）。
            subprocess.run(
                ["docker", "rm", "-f", self._container],
                stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
            )
            self._container = None

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

    def _post_json(self, path: str, payload: dict) -> dict:
        """POST 一个 JSON 到 daemon 的非流式端点，返回解析后的 JSON dict。

        阶段 2c 的文件/命令端点都走它（区别于 run_code 的 SSE 流式）。
        非 200 响应里的 {"error": ...} 会被抛成 RuntimeError。
        """
        req = urllib.request.Request(
            f"http://{self.host}:{self.port}{path}",
            data=json.dumps(payload).encode(),
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        try:
            with _DIRECT_OPENER.open(req) as resp:
                return json.loads(resp.read().decode())
        except urllib.error.HTTPError as exc:
            detail = exc.read().decode(errors="replace")
            try:
                message = json.loads(detail).get("error", detail)
            except json.JSONDecodeError:
                message = detail
            raise RuntimeError(f"{path} 失败：{message}") from exc


# ----- 阶段 2c：文件 / shell 命名空间（挂在 Sandbox 上，手感对齐 E2B）-----


class _Files:
    """sandbox.files.*：在沙箱文件系统里读写/列目录（对齐 E2B 的 sandbox.files）。

    由 daemon 直接在它所在的 FS 上完成：container/kernel 后端即容器内（沙箱里），
    local 后端即宿主。注意常驻容器是 --read-only 根 + 仅 /tmp 可写——写 /tmp 之外
    会抛 RuntimeError。
    """

    def __init__(self, sandbox: Sandbox) -> None:
        self._sb = sandbox

    def write(self, path: str, content: str) -> None:
        self._sb._post_json("/files/write", {"path": path, "content": content})

    def read(self, path: str) -> str:
        return self._sb._post_json("/files/read", {"path": path})["content"]

    def list(self, path: str) -> list[dict]:
        """列目录，返回 [{"name": str, "is_dir": bool}, ...]。"""
        return self._sb._post_json("/files/list", {"path": path})["entries"]


class _Commands:
    """sandbox.commands.*：在沙箱里跑 shell 命令（对齐 E2B 的 sandbox.commands）。"""

    def __init__(self, sandbox: Sandbox) -> None:
        self._sb = sandbox

    def run(self, command: str, timeout_seconds: float | None = None) -> Execution:
        """跑一条 shell 命令，返回和 run_code 同样的 Execution（stdout/stderr/exit_code）。"""
        payload = {
            "command": command,
            # 用 is None 判断而非 `or`：后者会把显式传入的 0 也当成「没传」而回退默认值。
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
