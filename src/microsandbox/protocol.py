"""The client↔daemon data shapes: the SDK's result types, and (now reference) the
original SSE wire protocol.

History & why this module exists:
    Through Stage 10 this *was* the wire protocol -- the single most important boundary in
    the project. The daemon's isolation changed many times (subprocess -> Docker ->
    Firecracker microVM) but this stayed byte-stable, so the SDK barely changed; that
    decoupling of "what to execute" from "where to execute it" is E2B's core philosophy,
    and it let the project evolve with a byte-for-byte e2e parity oracle.

    Stage 11 deliberately ended that: the client↔daemon wire is now **ConnectRPC** (see
    src/microsandbox/connect.py + daemon/proto/*.proto), so the e2e oracle became
    *behavioral* parity, not byte parity. What stays live here are the SDK's result types
    -- Execution, EventType, OutputEvent -- which run_code / commands still build and
    return. ExecuteRequest and the SSE serialization (to_sse / from_sse_payload / to_json)
    are retired, kept as reference for the pre-Stage-11 protocol (like server.py / backend.py).
"""

from __future__ import annotations

import json
from dataclasses import asdict, dataclass, field
from enum import Enum


class EventType(str, Enum):
    """The kinds of events the daemon streams back to the client."""

    STDOUT = "stdout"      # a chunk of standard output
    STDERR = "stderr"      # a chunk of standard error
    ERROR = "error"        # an execution-level error (e.g. code raised an exception, or timeout)
    END = "end"            # this execution finished, carries the exit code


@dataclass
class ExecuteRequest:
    """The client's request to execute a piece of code.

    `language` currently only supports python, but the field is reserved so it
    can be extended to javascript / bash / etc. in Stage 1+.
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
    """A single streamed event, daemon -> client.

    With SSE, each event is serialized into a single line of JSON following `data: `.
    """

    type: EventType
    data: str = ""                      # the text content of stdout/stderr/error
    exit_code: int | None = None        # only carried by the END event

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
    """The aggregated result of one execution, assembled by the client from the
    collected streamed events.

    This is the object the SDK returns to the user, similar to E2B's Execution result.
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


# ---- Stage 2c: file / shell API (backward-compatible additions; the three /execute types above stay untouched) ----
#
# Design point: these operations are performed by the daemon **directly on its
# own filesystem**, bypassing the ExecutionBackend -- for the container/kernel
# backends that means inside the container (= inside the sandbox), and for the
# local backend it means the host. This aligns with E2B's envd: the file service,
# the process service, and the "code-running kernel" are three separate things,
# rather than all being stuffed into the code-execution channel.
#
# Endpoints and response shapes (responses follow the project's existing "simple
# JSON dict" style; we don't build a dataclass for each one):
#   POST /files/read   <- {"path"}                       -> {"content": str}
#   POST /files/write  <- {"path","content"}             -> {"ok": true}
#   POST /files/list   <- {"path"}                       -> {"entries":[{"name":str,"is_dir":bool},...]}
#   POST /commands     <- {"command","timeout_seconds"}  -> {"stdout":str,"stderr":str,"exit_code":int}
# On error, all uniformly return {"error": str} plus a non-200 status code; on
# receiving it the client raises RuntimeError.
#
# Note (consistent with the isolation design): the resident container has a
# --read-only root and only /tmp is writable, so files.write can only write to
# /tmp; writing elsewhere yields an OSError.


@dataclass
class WriteFileRequest:
    path: str
    content: str = ""

    def to_json(self) -> str:
        return json.dumps(asdict(self))

    @classmethod
    def from_json(cls, raw: str) -> "WriteFileRequest":
        data = json.loads(raw)
        return cls(path=data["path"], content=data.get("content", ""))


@dataclass
class PathRequest:
    """Shared by read / list: only a path is needed."""

    path: str

    def to_json(self) -> str:
        return json.dumps(asdict(self))

    @classmethod
    def from_json(cls, raw: str) -> "PathRequest":
        return cls(path=json.loads(raw)["path"])


@dataclass
class CommandRequest:
    command: str
    timeout_seconds: float = 30.0

    def to_json(self) -> str:
        return json.dumps(asdict(self))

    @classmethod
    def from_json(cls, raw: str) -> "CommandRequest":
        data = json.loads(raw)
        return cls(
            command=data["command"],
            timeout_seconds=float(data.get("timeout_seconds", 30.0)),
        )
