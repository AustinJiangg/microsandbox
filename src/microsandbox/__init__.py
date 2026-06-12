"""microsandbox —— 一个从零实现、逐步逼近 E2B 的学习用代码沙箱。

公开 API 只暴露 SDK 层，底层 server / backend 视为实现细节。
"""

from .client import Sandbox
from .protocol import EventType, Execution, ExecuteRequest, OutputEvent

__all__ = [
    "Sandbox",
    "Execution",
    "ExecuteRequest",
    "OutputEvent",
    "EventType",
]

__version__ = "0.1.0"  # 阶段 1（版本语义：0.阶段号.补丁，与 pyproject.toml 手动同步）
