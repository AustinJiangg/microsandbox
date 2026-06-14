"""Stateful REPL tests: the Jupyter kernel running inside the microVM.

The core acceptance is "variables persist across run_code calls." Plus a few
cases that exercise the kernel backend's semantics: expression echoing, exceptions
marking failure, and "after a timeout interrupt the kernel is still alive and the
earlier variables remain" -- that last one best shows why the backend uses
interrupt rather than killing the process.
"""

from microsandbox import Sandbox


def test_state_persists_across_calls(sandbox: Sandbox) -> None:
    """Stage 2's core promise: the second execution can use a variable defined in the first."""
    first = sandbox.run_code("x = 41")
    assert first.success
    second = sandbox.run_code("print(x + 1)")
    assert second.success
    assert second.stdout.strip() == "42"


def test_function_and_import_persist(sandbox: Sandbox) -> None:
    """Not just variables: function definitions and imports also persist in the same namespace."""
    sandbox.run_code("import math\ndef area(r):\n    return math.pi * r * r")
    ex = sandbox.run_code("print(round(area(2), 2))")
    assert ex.success
    assert ex.stdout.strip() == "12.57"


def test_expression_result_echoed(sandbox: Sandbox) -> None:
    """REPL echo: writing a bare expression (rather than print) also yields its value (execute_result -> stdout)."""
    ex = sandbox.run_code("21 * 2")
    assert ex.success
    assert ex.stdout.strip() == "42"


def test_exception_marks_failure(sandbox: Sandbox) -> None:
    """Exception: the traceback goes to stderr and the exit code is non-zero (following the Stage 0/1 failure semantics)."""
    ex = sandbox.run_code("raise RuntimeError('boom')")
    assert not ex.success
    assert ex.exit_code != 0
    assert "RuntimeError" in ex.stderr


def test_stderr_captured(sandbox: Sandbox) -> None:
    """General contract: writes to stderr are captured and do not count as failure (consistent with Stage 0/1).

    The kernel backend translates iopub's stream(name=stderr) into a STDERR event --
    merely writing to stderr is not an exception, so the exit code is still 0."""
    ex = sandbox.run_code("import sys; sys.stderr.write('oops\\n')")
    assert ex.success
    assert "oops" in ex.stderr


def test_streaming_callback(sandbox: Sandbox) -> None:
    """General contract: the streaming callback receives the kernel's stdout chunk by chunk (iopub stream -> STDOUT event)."""
    chunks: list[str] = []
    sandbox.run_code("for i in range(3): print(i)", on_stdout=chunks.append)
    joined = "".join(chunks)
    assert "0" in joined and "1" in joined and "2" in joined


def test_timeout_interrupts_but_kernel_survives(sandbox: Sandbox) -> None:
    """Timeout uses interrupt rather than killing the process: the interrupted execution fails, but the kernel and its state remain.

    This is the biggest semantic difference between 2b and Stage 0/1 -- in Stage 0/1
    a timeout kills the entire interpreter and all state is lost; here only the
    current cell is interrupted, and variables defined earlier are still usable.
    """
    sandbox.run_code("keep = 'alive'")          # first define a variable

    sandbox.timeout_seconds = 0.5
    slow = sandbox.run_code("import time; time.sleep(30)")
    assert not slow.success
    assert slow.error is not None and "timed out" in slow.error

    sandbox.timeout_seconds = 30                # restore the normal timeout
    after = sandbox.run_code("print(keep)")
    assert after.success
    assert after.stdout.strip() == "alive"             # the kernel didn't die, the old variable is still there
