"""阶段 2c 测试：文件 / shell API（sandbox.files.* 与 sandbox.commands.*）。

在 resident_sandbox（container 后端）上跑：daemon 在容器里，文件/命令就作用在
容器（沙箱）的文件系统上。这些操作由 daemon 直接完成、与执行后端无关，所以选
更轻的 container 后端即可代表（不必起 kernel）。

注意：常驻容器是 --read-only 根 + 仅 /tmp 可写，故所有写操作都落在 /tmp。
"""

import pytest

from microsandbox import Sandbox


def test_write_then_read(resident_sandbox: Sandbox) -> None:
    """2c 的核心验收：写进去再读出来，往返一致。"""
    resident_sandbox.files.write("/tmp/hello.txt", "hello\nworld")
    assert resident_sandbox.files.read("/tmp/hello.txt") == "hello\nworld"


def test_file_visible_to_executed_code(resident_sandbox: Sandbox) -> None:
    """文件 API 与代码执行共享同一个沙箱文件系统：写的文件，run_code 能读到。"""
    resident_sandbox.files.write("/tmp/data.txt", "42")
    ex = resident_sandbox.run_code("print(open('/tmp/data.txt').read())")
    assert ex.success
    assert ex.stdout.strip() == "42"


def test_list_dir(resident_sandbox: Sandbox) -> None:
    """列目录能看到刚写进去的文件。"""
    resident_sandbox.files.write("/tmp/listme.txt", "x")
    names = [e["name"] for e in resident_sandbox.files.list("/tmp")]
    assert "listme.txt" in names


def test_write_to_readonly_root_fails(resident_sandbox: Sandbox) -> None:
    """写 /tmp 之外：常驻容器根是 --read-only，应当如实失败（抛 RuntimeError）。"""
    with pytest.raises(RuntimeError):
        resident_sandbox.files.write("/etc/nope.txt", "x")


def test_command_run(resident_sandbox: Sandbox) -> None:
    """commands.run 在沙箱里跑 shell，拿回 stdout/exit_code（返回 Execution）。"""
    ex = resident_sandbox.commands.run("echo hello")
    assert ex.success
    assert ex.exit_code == 0
    assert ex.stdout.strip() == "hello"


def test_command_failure_exit_code(resident_sandbox: Sandbox) -> None:
    """非零退出码如实带回，且标记为失败。"""
    ex = resident_sandbox.commands.run("exit 3")
    assert not ex.success
    assert ex.exit_code == 3


def test_command_timeout(resident_sandbox: Sandbox) -> None:
    """命令超时：daemon 杀掉进程、回 exit -1 并在 stderr 标明超时（覆盖超时清理路径）。"""
    ex = resident_sandbox.commands.run("sleep 5", timeout_seconds=0.5)
    assert not ex.success
    assert "timed out" in ex.stderr
