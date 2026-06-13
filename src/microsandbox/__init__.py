"""microsandbox -- a from-scratch, learning-oriented code sandbox that incrementally approaches E2B.

The public API exposes only the SDK layer; the underlying server / backend are
implementation details.
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

__version__ = "0.1.0"  # version scheme: 0.<stage>.<patch>, kept in sync with pyproject.toml by hand
