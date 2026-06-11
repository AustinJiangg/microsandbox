"""代码执行后端（execution backend）。

【这是整个项目里未来会被反复替换的部分】

阶段 0（当前）：用本机子进程执行 —— 几乎没有隔离，只为跑通骨架。
阶段 1：换成 Docker 容器执行。
阶段 2：把执行逻辑做成容器内常驻 agent。
阶段 3：换成 Firecracker microVM。

为了让未来替换不影响其它代码，这里定义一个抽象基类 `ExecutionBackend`，
其它模块只依赖这个接口。后续阶段新增 `DockerBackend`、`FirecrackerBackend`
等实现即可，server 代码不用动。
"""

from __future__ import annotations

import asyncio
import sys
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

        # 并发地把 stdout / stderr 两路输出转成事件，塞进一个队列。
        # 用队列是为了让两路输出能按到达顺序交错流出，而不是先全部 stdout 再 stderr。
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
        timed_out = False
        try:
            # 给整个执行加超时。超时就杀进程。
            async with asyncio.timeout(request.timeout_seconds):
                while finished < len(pumps):
                    event = await queue.get()
                    if event is None:
                        finished += 1
                        continue
                    yield event
        except TimeoutError:
            timed_out = True
            proc.kill()
            yield OutputEvent(
                type=EventType.ERROR,
                data=f"execution timed out after {request.timeout_seconds}s",
            )

        for task in pumps:
            task.cancel()

        exit_code = await proc.wait()
        if timed_out:
            yield OutputEvent(type=EventType.END, exit_code=-1)
        else:
            yield OutputEvent(type=EventType.END, exit_code=exit_code)
