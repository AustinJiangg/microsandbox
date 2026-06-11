"""客户端 <-> 沙箱守护进程之间的线缆协议（wire protocol）。

为什么单独抽一个模块：
    这是整个项目最重要的「边界」。阶段 0 里守护进程只是本机的一个子进程，
    但从阶段 1 开始它会跑在 Docker 容器里、阶段 3 跑在 Firecracker microVM 里。
    无论底层隔离怎么变，只要这份协议保持稳定，client（SDK）就几乎不用改。
    这正是 E2B 的设计哲学：把「执行什么」和「在哪执行」解耦。

协议本身很简单：
    - 客户端 POST 一段代码到守护进程
    - 守护进程以 SSE（Server-Sent Events）流式返回若干 OutputEvent
    - 每个 event 是一行 JSON，描述一段 stdout / stderr / 或最终的结束状态
"""

from __future__ import annotations

import json
from dataclasses import asdict, dataclass, field
from enum import Enum
from typing import Any


class EventType(str, Enum):
    """守护进程流式返回给客户端的事件种类。"""

    STDOUT = "stdout"      # 一段标准输出
    STDERR = "stderr"      # 一段标准错误
    ERROR = "error"        # 执行层面的错误（例如代码抛异常、超时）
    END = "end"            # 本次执行结束，携带退出码


@dataclass
class ExecuteRequest:
    """客户端请求执行一段代码。

    language 目前只支持 python，但预留字段，方便阶段 1+ 扩展到
    javascript / bash 等。
    """

    code: str
    language: str = "python"
    timeout_seconds: float = 30.0

    def to_json(self) -> str:
        return json.dumps(asdict(self))

    @classmethod
    def from_json(cls, raw: str) -> "ExecuteRequest":
        data = json.loads(raw)
        return cls(
            code=data["code"],
            language=data.get("language", "python"),
            timeout_seconds=float(data.get("timeout_seconds", 30.0)),
        )


@dataclass
class OutputEvent:
    """守护进程 -> 客户端 的单个流式事件。

    用 SSE 时，每个事件序列化成一行 JSON 跟在 `data: ` 后面。
    """

    type: EventType
    data: str = ""                      # stdout/stderr/error 的文本内容
    exit_code: int | None = None        # 仅 END 事件携带

    def to_sse(self) -> str:
        payload = {"type": self.type.value, "data": self.data}
        if self.exit_code is not None:
            payload["exit_code"] = self.exit_code
        return f"data: {json.dumps(payload)}\n\n"

    @classmethod
    def from_sse_payload(cls, raw: str) -> "OutputEvent":
        data = json.loads(raw)
        return cls(
            type=EventType(data["type"]),
            data=data.get("data", ""),
            exit_code=data.get("exit_code"),
        )


@dataclass
class Execution:
    """一次执行的聚合结果，由客户端把流式事件收集汇总而成。

    这是 SDK 返回给用户的对象，类似 E2B 的 Execution 结果。
    """

    stdout: str = ""
    stderr: str = ""
    error: str | None = None
    exit_code: int | None = None
    events: list[OutputEvent] = field(default_factory=list)

    @property
    def success(self) -> bool:
        return self.error is None and (self.exit_code == 0 or self.exit_code is None)

    def __repr__(self) -> str:
        status = "ok" if self.success else "failed"
        return (
            f"Execution(status={status}, exit_code={self.exit_code}, "
            f"stdout={self.stdout!r}, stderr={self.stderr!r}, error={self.error!r})"
        )
