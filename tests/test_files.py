"""File / shell API tests (sandbox.files.* and sandbox.commands.*).

The daemon lives inside the microVM, so files/commands act on the VM's own
filesystem. These operations are performed directly by the daemon, independent of
the execution backend.

Note: the VM has a --read-only root with only /tmp writable, so all write
operations land in /tmp.
"""

import pytest

from microsandbox import Sandbox


def test_write_then_read(sandbox: Sandbox) -> None:
    """The core acceptance for 2c: write it in then read it back, round-trip consistent."""
    sandbox.files.write("/tmp/hello.txt", "hello\nworld")
    assert sandbox.files.read("/tmp/hello.txt") == "hello\nworld"


def test_file_visible_to_executed_code(sandbox: Sandbox) -> None:
    """The file API and code execution share the same sandbox filesystem: a file written here is readable by run_code."""
    sandbox.files.write("/tmp/data.txt", "42")
    ex = sandbox.run_code("print(open('/tmp/data.txt').read())")
    assert ex.success
    assert ex.stdout.strip() == "42"


def test_list_dir(sandbox: Sandbox) -> None:
    """Listing the directory shows the file just written."""
    sandbox.files.write("/tmp/listme.txt", "x")
    names = [e["name"] for e in sandbox.files.list("/tmp")]
    assert "listme.txt" in names


def test_write_to_readonly_root_fails(sandbox: Sandbox) -> None:
    """Writing outside /tmp: the resident container's root is --read-only, so it should fail faithfully (raise RuntimeError)."""
    with pytest.raises(RuntimeError):
        sandbox.files.write("/etc/nope.txt", "x")


def test_command_run(sandbox: Sandbox) -> None:
    """commands.run runs a shell inside the sandbox, returning stdout/exit_code (returns an Execution)."""
    ex = sandbox.commands.run("echo hello")
    assert ex.success
    assert ex.exit_code == 0
    assert ex.stdout.strip() == "hello"


def test_command_failure_exit_code(sandbox: Sandbox) -> None:
    """A non-zero exit code is reported faithfully and marked as a failure."""
    ex = sandbox.commands.run("exit 3")
    assert not ex.success
    assert ex.exit_code == 3


def test_command_timeout(sandbox: Sandbox) -> None:
    """Command timeout: the daemon kills the process, returns exit -1, and notes the timeout in stderr (covers the timeout-cleanup path)."""
    ex = sandbox.commands.run("sleep 5", timeout_seconds=0.5)
    assert not ex.success
    assert "timed out" in ex.stderr
