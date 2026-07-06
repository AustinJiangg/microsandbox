"""File / shell API tests (sandbox.files.* and sandbox.commands.*).

The daemon lives inside the microVM, so files/commands act on the VM's own
filesystem. These operations are performed directly by the daemon, independent of
the execution backend.

Note: since Stage 22b every sandbox boots a private writable rootfs overlay (an
NBD copy-on-write overlay over the shared read-only base), so writes outside /tmp
succeed into that VM's own cache and are discarded on destroy.
"""

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


def test_write_to_root_succeeds_on_writable_overlay(sandbox: Sandbox) -> None:
    """Writing outside /tmp now succeeds. Since Stage 22b every sandbox boots a private writable rootfs
    overlay (an NBD block.Overlay over the shared read-only base), so a write to /etc lands in this VM's
    own copy-on-write cache -- invisible to other sandboxes and discarded on destroy. (Before Stage 22 the
    microVM root was read-only and this write raised RuntimeError.)"""
    sandbox.files.write("/etc/nope.txt", "x")
    assert sandbox.files.read("/etc/nope.txt") == "x"


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
