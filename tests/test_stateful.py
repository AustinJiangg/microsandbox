"""阶段 2b 测试：常驻 Jupyter kernel 的有状态 REPL。

核心验收是「变量跨 run_code 留存」。外加几条体现 kernel 后端语义的用例：
表达式回显、异常标失败，以及「超时 interrupt 后 kernel 仍活着、之前的变量还在」
——最后一条最能说明 2b 为什么用 interrupt 而不是杀进程。
"""

from microsandbox import Sandbox


def test_state_persists_across_calls(kernel_sandbox: Sandbox) -> None:
    """阶段 2 的核心承诺：第二次执行能用第一次定义的变量。"""
    first = kernel_sandbox.run_code("x = 41")
    assert first.success
    second = kernel_sandbox.run_code("print(x + 1)")
    assert second.success
    assert second.stdout.strip() == "42"


def test_function_and_import_persist(kernel_sandbox: Sandbox) -> None:
    """不只是变量：函数定义、import 也都留在同一个命名空间里。"""
    kernel_sandbox.run_code("import math\ndef area(r):\n    return math.pi * r * r")
    ex = kernel_sandbox.run_code("print(round(area(2), 2))")
    assert ex.success
    assert ex.stdout.strip() == "12.57"


def test_expression_result_echoed(kernel_sandbox: Sandbox) -> None:
    """REPL 回显：直接写表达式（而非 print）也能拿到值（execute_result→stdout）。"""
    ex = kernel_sandbox.run_code("21 * 2")
    assert ex.success
    assert ex.stdout.strip() == "42"


def test_exception_marks_failure(kernel_sandbox: Sandbox) -> None:
    """异常：traceback 进 stderr、退出码非 0（沿用阶段 0/1 的失败语义）。"""
    ex = kernel_sandbox.run_code("raise RuntimeError('boom')")
    assert not ex.success
    assert ex.exit_code != 0
    assert "RuntimeError" in ex.stderr


def test_stderr_captured(kernel_sandbox: Sandbox) -> None:
    """通用契约：写 stderr 能被捕获，且不算失败（与阶段 0/1 一致）。

    kernel 后端把 iopub 的 stream(name=stderr) 翻译成 STDERR 事件——单纯写 stderr
    没有异常，退出码仍为 0。"""
    ex = kernel_sandbox.run_code("import sys; sys.stderr.write('oops\\n')")
    assert ex.success
    assert "oops" in ex.stderr


def test_streaming_callback(kernel_sandbox: Sandbox) -> None:
    """通用契约：流式回调能逐段拿到 kernel 的 stdout（iopub stream→STDOUT 事件）。"""
    chunks: list[str] = []
    kernel_sandbox.run_code("for i in range(3): print(i)", on_stdout=chunks.append)
    joined = "".join(chunks)
    assert "0" in joined and "1" in joined and "2" in joined


def test_timeout_interrupts_but_kernel_survives(kernel_sandbox: Sandbox) -> None:
    """超时用 interrupt 而非杀进程：被打断的执行失败，但 kernel 和它的状态都还在。

    这是 2b 与阶段 0/1 最大的语义差别——阶段 0/1 超时会杀掉整个解释器、状态全丢；
    这里只打断当前 cell，之前定义的变量依然可用。
    """
    kernel_sandbox.run_code("keep = 'alive'")          # 先定义一个变量

    kernel_sandbox.timeout_seconds = 0.5
    slow = kernel_sandbox.run_code("import time; time.sleep(30)")
    assert not slow.success
    assert slow.error is not None and "timed out" in slow.error

    kernel_sandbox.timeout_seconds = 30                # 恢复正常超时
    after = kernel_sandbox.run_code("print(keep)")
    assert after.success
    assert after.stdout.strip() == "alive"             # kernel 没死，旧变量还在
