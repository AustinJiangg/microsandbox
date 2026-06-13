"""End-to-end tests (written in Stage 0, reused across both backends from Stage 1).

Covers: basic execution, stdout/stderr separation, error capture, timeout.
The sandbox fixture in conftest.py is already parametrized over the local /
docker backends -- the same test bodies run once each, verifying that "the
protocol contract stays fixed while the isolation scheme can be swapped."
"""

from microsandbox import Sandbox


def test_basic_stdout(sandbox: Sandbox) -> None:
    ex = sandbox.run_code("print('hello')")
    assert ex.stdout.strip() == "hello"
    assert ex.exit_code == 0
    assert ex.success


def test_computation(sandbox: Sandbox) -> None:
    ex = sandbox.run_code("print(2 ** 10)")
    assert ex.stdout.strip() == "1024"


def test_stderr_captured(sandbox: Sandbox) -> None:
    ex = sandbox.run_code(
        "import sys; sys.stderr.write('oops\\n')"
    )
    assert "oops" in ex.stderr


def test_exception_marks_failure(sandbox: Sandbox) -> None:
    ex = sandbox.run_code("raise RuntimeError('boom')")
    assert not ex.success
    assert ex.exit_code != 0
    assert "RuntimeError" in ex.stderr


def test_timeout(sandbox: Sandbox) -> None:
    sandbox.timeout_seconds = 0.5
    ex = sandbox.run_code("import time; time.sleep(5)")
    assert not ex.success
    assert ex.error is not None
    assert "timed out" in ex.error


def test_streaming_callback(sandbox: Sandbox) -> None:
    chunks: list[str] = []
    sandbox.run_code(
        "for i in range(3): print(i)",
        on_stdout=chunks.append,
    )
    joined = "".join(chunks)
    assert "0" in joined and "1" in joined and "2" in joined


def test_unsupported_language(sandbox: Sandbox) -> None:
    ex = sandbox.run_code("console.log(1)", language="javascript")
    assert not ex.success
    assert ex.error is not None
    assert "unsupported" in ex.error
