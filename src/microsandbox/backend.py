"""代码执行后端（execution backend）。

【这是整个项目里未来会被反复替换的部分】

阶段 0：用本机子进程执行 —— 几乎没有隔离，只为跑通骨架。
阶段 1（当前）：Docker 容器执行 —— 第一次真正的隔离。
阶段 2：把执行逻辑做成容器内常驻 agent。
阶段 3：换成 Firecracker microVM。

为了让未来替换不影响其它代码，这里定义一个抽象基类 `ExecutionBackend`，
其它模块只依赖这个接口。后续阶段新增 `DockerBackend`、`FirecrackerBackend`
等实现即可，server 代码不用动。
"""

from __future__ import annotations

import asyncio
import re
import shutil
import subprocess
import sys
import uuid
from abc import ABC, abstractmethod
from collections.abc import AsyncIterator

from .protocol import EventType, ExecuteRequest, OutputEvent


class ExecutionBackend(ABC):
    """执行后端的抽象接口。所有隔离方案都实现它。"""

    @abstractmethod
    def execute(self, request: ExecuteRequest) -> AsyncIterator[OutputEvent]:
        """执行一段代码，以异步流的形式产出输出事件。

        注意返回类型是 AsyncIterator：调用方用 `async for` 消费，
        从而实现「边跑边返回」的流式输出，而不是等全部跑完。
        """
        raise NotImplementedError


async def _merge_output_streams(
    proc: asyncio.subprocess.Process,
) -> AsyncIterator[OutputEvent]:
    """把子进程的 stdout / stderr 两路输出按「到达顺序」合并成一条事件流。

    为什么抽成公共函数：对比 LocalSubprocessBackend 和 DockerBackend 会发现，
    后端之间的差异只在「怎么启动、怎么杀、怎么清理」——而「怎么把两路输出
    交错地流式吐出去」完全相同。抽出来正好凸显这条边界。

    实现：给 stdout / stderr 各开一个 pump 任务，往同一个队列里塞事件
    （用队列才能按到达顺序交错流出，而不是先全部 stdout 再全部 stderr）；
    每路读到 EOF 就塞一个 None 哨兵，主循环数到两个哨兵即结束。
    """
    queue: asyncio.Queue[OutputEvent | None] = asyncio.Queue()

    async def pump(stream: asyncio.StreamReader, etype: EventType) -> None:
        while True:
            line = await stream.readline()
            if not line:
                break
            await queue.put(OutputEvent(type=etype, data=line.decode(errors="replace")))
        await queue.put(None)  # 哨兵：表示这一路读完了

    assert proc.stdout is not None and proc.stderr is not None
    pumps = [
        asyncio.create_task(pump(proc.stdout, EventType.STDOUT)),
        asyncio.create_task(pump(proc.stderr, EventType.STDERR)),
    ]

    finished = 0
    try:
        while finished < len(pumps):
            event = await queue.get()
            if event is None:
                finished += 1
                continue
            yield event
    finally:
        # 正常读完时两个 pump 已自行结束，cancel 是空操作。
        # 但若调用方超时（CancelledError 会从上面的 await 点穿出来）或
        # 客户端中途断连导致本生成器被提前关闭，这里保证 pump 任务不泄漏。
        for task in pumps:
            task.cancel()


class LocalSubprocessBackend(ExecutionBackend):
    """阶段 0 后端：在本机起一个子进程跑代码。

    ⚠️ 安全警告：这里几乎没有隔离。代码能访问本机文件系统、网络、环境变量。
    仅供本地学习使用，绝对不要拿它接收不可信输入。真正的隔离从阶段 1 开始。
    """

    # 不同语言怎么把代码喂给解释器。阶段 0 先只接 python。
    _INTERPRETERS = {
        "python": [sys.executable, "-u", "-c"],  # -u 关闭缓冲，保证流式输出实时
    }

    async def execute(
        self, request: ExecuteRequest
    ) -> AsyncIterator[OutputEvent]:
        interpreter = self._INTERPRETERS.get(request.language)
        if interpreter is None:
            yield OutputEvent(
                type=EventType.ERROR,
                data=f"unsupported language: {request.language}",
            )
            yield OutputEvent(type=EventType.END, exit_code=1)
            return

        cmd = [*interpreter, request.code]

        proc = await asyncio.create_subprocess_exec(
            *cmd,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
        )

        timed_out = False
        try:
            # 给整个执行加超时。超时就杀进程——本机子进程一杀即死。
            # 对比 DockerBackend 的同名段落：杀法和清理方式正是两个后端的差异所在。
            async with asyncio.timeout(request.timeout_seconds):
                async for event in _merge_output_streams(proc):
                    yield event
        except TimeoutError:
            timed_out = True
            proc.kill()
            yield OutputEvent(
                type=EventType.ERROR,
                data=f"execution timed out after {request.timeout_seconds}s",
            )

        # wait() 既拿退出码也回收子进程，避免留下僵尸进程。
        exit_code = await proc.wait()
        yield OutputEvent(type=EventType.END, exit_code=-1 if timed_out else exit_code)


# 阶段 1 默认镜像：官方 slim 镜像自带 python 解释器，体积小、拉取快。
DEFAULT_DOCKER_IMAGE = "python:3.12-slim"


class DockerBackend(ExecutionBackend):
    """阶段 1 后端：每次执行起一个一次性 Docker 容器跑代码。

    相比阶段 0 的本机子进程，这里第一次拿到了真正的隔离：
      - 文件系统隔离：容器有自己的根文件系统（mount namespace），看不见宿主文件；
      - 网络隔离：--network none 不给网卡（network namespace），容器内彻底断网；
      - 资源限制：--memory / --cpus / --pids-limit 由内核 cgroups 强制执行。

    ⚠️ 但容器与宿主共享内核，逃逸面仍然不小——依旧不要拿它跑完全不可信的代码。
    内核级强隔离要等阶段 3 的 microVM。

    实现方式：直接用 `docker` CLI（asyncio 子进程），不引入 docker-py。理由：
    ① 保持零运行时依赖；② docker-py 是同步库，放进 asyncio daemon 必须套线程池，
    反而更复杂；③ `docker run` 子进程的输出泵法与阶段 0 完全同构——
    「换隔离 = 换启动命令 + 换杀法」，流式管道（_merge_output_streams）原样复用。
    """

    # 容器内的解释器就叫 python（镜像自带），不再是宿主的 sys.executable。
    _INTERPRETERS = {
        "python": ["python", "-u", "-c"],  # -u 关闭缓冲，保证流式输出实时
    }

    def __init__(
        self,
        image: str = DEFAULT_DOCKER_IMAGE,
        memory: str = "256m",
        cpus: str = "1.0",
        pids_limit: int = 64,
    ) -> None:
        self.image = image
        self.memory = memory
        self.cpus = cpus
        self.pids_limit = pids_limit

    @staticmethod
    def check_available(image: str = DEFAULT_DOCKER_IMAGE) -> str | None:
        """检查本机 Docker 是否可用。返回错误描述，一切正常返回 None。

        在 daemon 启动期同步调用（事件循环还没起，所以用 subprocess.run），
        而不是等到第一次 /execute 才在 SSE 流里报错——环境问题应该暴露给
        「起 daemon 的人」，而不是埋进某次执行结果里。
        """
        if shutil.which("docker") is None:
            return "未找到 docker 命令，请先安装 Docker"
        try:
            probe = subprocess.run(["docker", "info"], capture_output=True, timeout=10)
        except subprocess.TimeoutExpired:
            return "docker info 超时，Docker 守护进程可能已卡死"
        if probe.returncode != 0:
            return "Docker 守护进程未运行，请先启动（WSL2 下通常是 sudo service docker start）"
        probe = subprocess.run(
            ["docker", "image", "inspect", image], capture_output=True, timeout=10
        )
        if probe.returncode != 0:
            return f"镜像 {image} 不存在，请先执行：docker pull {image}"
        return None

    async def execute(
        self, request: ExecuteRequest
    ) -> AsyncIterator[OutputEvent]:
        interpreter = self._INTERPRETERS.get(request.language)
        if interpreter is None:
            yield OutputEvent(
                type=EventType.ERROR,
                data=f"unsupported language: {request.language}",
            )
            yield OutputEvent(type=EventType.END, exit_code=1)
            return

        # 给容器起可识别的名字：一是超时要靠名字 docker rm -f；
        # 二是万一 daemon 被 kill -9 留下残留容器，也能按前缀一键清理
        # （docker ps -a --filter name=microsandbox-exec -q | xargs -r docker rm -f）。
        name = f"microsandbox-exec-{uuid.uuid4().hex[:12]}"

        cmd = [
            "docker", "run",
            "--rm",              # 容器退出后由 docker daemon 自动删除（正常路径的清理）
            "--pull", "never",   # 镜像缺失立刻报错（exit 125）。绝不在请求路径里隐式
                                 # 拉镜像——否则首次 pull 的几十秒会吃光 timeout_seconds
            "--name", name,
            "--network", "none",            # 不给网卡：容器内彻底断网
            "--memory", self.memory,        # 内存上限，超限被内核 OOM kill（exit 137）
            "--memory-swap", self.memory,   # 含 swap 的总上限设为同值 = 禁止用 swap 绕过限制
            "--cpus", self.cpus,
            "--pids-limit", str(self.pids_limit),  # 防 fork 炸弹
            "--read-only",                  # 根文件系统只读
            "--tmpfs", "/tmp:rw,size=64m",  # 唯一可写区：内存盘 /tmp
            self.image,
            # 与阶段 0 同构：python -u -c <code>。代码经 argv 传入，Linux 上限
            # 约 2MB，阶段 1 足够；阶段 2 改成容器内常驻 agent 后此限制消失。
            *interpreter, request.code,
        ]

        proc = await asyncio.create_subprocess_exec(
            *cmd,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
        )

        timed_out = False
        try:
            try:
                # 超时计的是整个 docker run，包含 ~0.5-1s 的容器冷启动——用户感知的
                # 「执行时间」本来就包含它（冷启动优化是阶段 4 沙箱池预热的事）。
                async with asyncio.timeout(request.timeout_seconds):
                    async for event in _merge_output_streams(proc):
                        yield event
            except TimeoutError:
                timed_out = True
                # 关键事实：杀掉 docker run 这个客户端进程并不能杀死容器——它只是
                # 连着 docker daemon 的「遥控器」，真正的容器进程由 daemon 托管。
                # 所以必须先 docker rm -f 杀死并删除容器，再顺手收掉客户端进程。
                await self._remove_container(name)
                proc.kill()
                yield OutputEvent(
                    type=EventType.ERROR,
                    data=f"execution timed out after {request.timeout_seconds}s",
                )

            # 退出码：0/1 由容器内 python 原样透传；125/126/127 被 docker 自身占用
            # （依次为 docker 错误 / 命令不可执行 / 命令不存在），docker 的报错文本
            # 会混在 stderr 事件里；137 = 128+SIGKILL，通常是 OOM 触顶或被 rm -f 杀。
            exit_code = await proc.wait()
            yield OutputEvent(type=EventType.END, exit_code=-1 if timed_out else exit_code)
        finally:
            # 幂等兜底：正常路径 --rm 已删容器，这里的 "No such container" 失败会被
            # 忽略；同时覆盖「客户端中途断连导致本生成器被提前关闭」等所有异常路径。
            await self._remove_container(name)

    @staticmethod
    async def _remove_container(name: str) -> None:
        """docker rm -f <name>：强制杀死并删除容器。一切失败（如容器不存在）都忽略。"""
        proc = await asyncio.create_subprocess_exec(
            "docker", "rm", "-f", name,
            stdout=asyncio.subprocess.DEVNULL,
            stderr=asyncio.subprocess.DEVNULL,
        )
        await proc.wait()


# 阶段 2b 默认 agent 镜像：在 slim 基础上预装了 ipykernel + jupyter_client，
# 容器内 daemon 用它托管常驻 kernel。需先 docker build（见仓库根 Dockerfile）。
DEFAULT_AGENT_IMAGE = "microsandbox-agent:latest"

# IPython 的 traceback 自带 ANSI 颜色码，落到我们的纯文本 stderr 前先剥掉。
_ANSI_RE = re.compile(r"\x1b\[[0-9;]*m")


def _strip_ansi(text: str) -> str:
    return _ANSI_RE.sub("", text)


class JupyterKernelBackend(ExecutionBackend):
    """阶段 2b 后端：在 daemon 进程里托管一个**常驻** Jupyter (IPython) kernel。

    这是阶段 2 的灵魂。和阶段 0/1「每次执行起一个全新解释器、跑完即弃」不同，
    这里 kernel 长期存活、持有一个 Python 命名空间——所以多次 run_code 之间定义的
    变量会留存，第二次执行能直接用第一次的结果。这正是 E2B code interpreter 的做法
    （它底层也是 Jupyter kernel）。

    与 client 的配合：client 用 backend="kernel" 起一个常驻容器（agent 镜像里预装了
    ipykernel/jupyter_client），容器内 daemon 用 --backend kernel 启动，实例化的就是
    本类。「换隔离/换执行模型 = 换 backend」，client 与 /execute 协议照旧不动。

    通信机制：通过 jupyter_client 用 ZMQ 跟 kernel 子进程说 Jupyter 消息协议——
      - execute(code) 在 shell 通道发请求，立刻返回该请求的 msg_id；
      - kernel 在 iopub 通道流式回吐：stream(stdout/stderr)、execute_result（表达式的
        值）、error（异常 traceback）、status（busy/idle，idle 表示这次跑完了）。
    我们把这些消息翻译回项目统一的 OutputEvent，于是 /execute 协议毫不变动。
    """

    def __init__(self) -> None:
        # 懒依赖：只有真正用 kernel 后端时才需要 jupyter_client。这样宿主上跑
        # local/docker/container 后端时，import backend.py 不会要求装 jupyter。
        try:
            from jupyter_client.manager import AsyncKernelManager
        except ImportError as exc:  # pragma: no cover - 仅在缺依赖的环境触发
            raise RuntimeError(
                "kernel 后端需要 ipykernel + jupyter_client。请用 agent 镜像"
                "（docker build -t microsandbox-agent .），"
                "或本地安装 pip install 'microsandbox[kernel]'。"
            ) from exc
        self._AsyncKernelManager = AsyncKernelManager
        self._km: object | None = None   # AsyncKernelManager
        self._kc: object | None = None   # AsyncKernelClient
        # kernel 是共享的有状态资源，一次只能跑一个 cell：用锁把并发的 execute 串行化
        # （这也正是有状态 REPL 想要的语义——后一次执行能看见前一次的副作用）。
        self._lock = asyncio.Lock()

    async def _ensure_started(self) -> None:
        """首次执行时懒启动 kernel，之后复用。冷启动那几秒只在第一次付出。"""
        if self._kc is not None:
            return
        km = self._AsyncKernelManager(kernel_name="python3")
        await km.start_kernel()
        kc = km.client()
        kc.start_channels()
        await kc.wait_for_ready(timeout=60)
        self._km, self._kc = km, kc

    async def execute(
        self, request: ExecuteRequest
    ) -> AsyncIterator[OutputEvent]:
        if request.language != "python":
            yield OutputEvent(
                type=EventType.ERROR,
                data=f"unsupported language: {request.language}",
            )
            yield OutputEvent(type=EventType.END, exit_code=1)
            return

        async with self._lock:  # 串行化对共享 kernel 的访问
            try:
                await self._ensure_started()
            except Exception as exc:  # noqa: BLE001 - 启动失败要如实回吐给用户
                yield OutputEvent(
                    type=EventType.ERROR, data=f"kernel failed to start: {exc}"
                )
                yield OutputEvent(type=EventType.END, exit_code=1)
                return

            async for event in self._run_one(request):
                yield event

    async def _run_one(
        self, request: ExecuteRequest
    ) -> AsyncIterator[OutputEvent]:
        kc = self._kc
        # execute() 是同步方法：立刻在 shell 通道发出请求并返回 msg_id。之后所有
        # 属于本次执行的 iopub 消息，其 parent_header.msg_id 都等于这个 msg_id。
        msg_id = kc.execute(request.code, store_history=True, allow_stdin=False)

        had_error = False
        timed_out = False
        try:
            # 超时只罩住「这一个 cell 的执行」，不含上面 kernel 的一次性冷启动。
            async with asyncio.timeout(request.timeout_seconds):
                while True:
                    msg = await kc.get_iopub_msg()
                    if msg.get("parent_header", {}).get("msg_id") != msg_id:
                        continue  # 不是本次执行的消息，跳过（过滤更稳）
                    if msg["header"]["msg_type"] == "error":
                        had_error = True
                    event, done = self._translate(msg)
                    if event is not None:
                        yield event
                    if done:
                        break
        except TimeoutError:
            timed_out = True
            # 关键设计：超时用 interrupt（给 kernel 发 SIGINT）而非杀进程——cell 被
            # 打断（如 time.sleep 抛 KeyboardInterrupt），但 kernel 和它的命名空间都
            # 还活着，之前定义的变量不丢。这正是「有状态」的价值，也是与阶段 0/1
            # 「超时即杀掉整个解释器」最大的语义差别。
            #
            # 已知边界：interrupt 靠 SIGINT→KeyboardInterrupt，挡不住「吞掉
            # KeyboardInterrupt 的代码」或 C 扩展里的硬阻塞——那种情况下 cell 会在
            # 后台继续跑，拖住后续执行。production（如 E2B）会在 interrupt 失败后
            # 升级到「重启 kernel」（必死但丢状态）；本项目有意只做 interrupt 保状态。
            await self._km.interrupt_kernel()
            await self._drain_until_idle(msg_id)  # 排空被打断产生的残余，保证下次干净
            yield OutputEvent(
                type=EventType.ERROR,
                data=f"execution timed out after {request.timeout_seconds}s",
            )

        # 收掉本次执行的 shell execute_reply：iopub 之外 kernel 还会在 shell 通道回一条
        # reply，没人读它就会在通道里越积越多，积到 ZMQ 高水位后 kernel 发 reply 会阻塞。
        # 正常和超时两条路都会产生 reply，放在 try/except 之外正好都覆盖。
        await self._drain_shell_reply(msg_id)

        # 退出码对齐阶段 0/1 的语义：正常 0；异常非 0（→ success False）；超时 -1。
        if timed_out:
            exit_code = -1
        else:
            exit_code = 1 if had_error else 0
        yield OutputEvent(type=EventType.END, exit_code=exit_code)

    @staticmethod
    def _translate(msg: dict) -> tuple[OutputEvent | None, bool]:
        """把一条 iopub 消息翻译成 (OutputEvent | None, 本次执行是否已结束)。"""
        mtype = msg["header"]["msg_type"]
        content = msg["content"]
        if mtype == "stream":
            etype = (
                EventType.STDOUT
                if content.get("name") == "stdout"
                else EventType.STDERR
            )
            return OutputEvent(type=etype, data=content.get("text", "")), False
        if mtype in ("execute_result", "display_data"):
            # 表达式的值（REPL 回显，例如直接写 `x` 而非 print(x)）。并入 stdout，
            # 这样不必给 /execute 协议加新事件类型就能把它带回去。
            text = content.get("data", {}).get("text/plain")
            return (
                (OutputEvent(type=EventType.STDOUT, data=text + "\n"), False)
                if text
                else (None, False)
            )
        if mtype == "error":
            tb = _strip_ansi("\n".join(content.get("traceback", [])))
            return OutputEvent(type=EventType.STDERR, data=tb + "\n"), False
        if mtype == "status" and content.get("execution_state") == "idle":
            return None, True  # kernel 回到空闲：本次执行的输出已全部流完
        return None, False  # execute_input / busy 等无需转发的消息

    async def _drain_until_idle(self, msg_id: str) -> None:
        """interrupt 后把本次执行残余的 iopub 消息排空到 idle，避免污染下次执行。"""
        try:
            async with asyncio.timeout(10):
                while True:
                    msg = await self._kc.get_iopub_msg()
                    if msg.get("parent_header", {}).get("msg_id") != msg_id:
                        continue
                    if (
                        msg["header"]["msg_type"] == "status"
                        and msg["content"].get("execution_state") == "idle"
                    ):
                        return
        except TimeoutError:
            return  # 极端情况放弃排空；下次执行靠 parent_header 过滤也能容忍残余

    async def _drain_shell_reply(self, msg_id: str) -> None:
        """收掉本次执行的 shell execute_reply，避免未读消息在 shell 通道越积越多
        （积到 ZMQ 高水位后 kernel 发 reply 会阻塞）。最多等一小会儿，收不到就算了。"""
        try:
            async with asyncio.timeout(5):
                while True:
                    reply = await self._kc.get_shell_msg()
                    if reply.get("parent_header", {}).get("msg_id") == msg_id:
                        return
        except TimeoutError:
            return
