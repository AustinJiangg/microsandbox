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
