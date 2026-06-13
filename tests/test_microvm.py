"""阶段 3b 测试：Firecracker microVM 后端（端到端经 vsock）。

跑在真 microVM 里：独立 guest 内核 + KVM 边界，控制通道走 vsock，daemon 与 protocol
一行未改。需要 vendor/ 下的 firecracker + vmlinux + rootfs.ext4（见 docs/STAGE3_DESIGN.md
§6/§7）与可访问的 /dev/kvm；缺任一则整组 skip，别的机器 / CI 上照常全绿。
"""

import pathlib

from microsandbox import Sandbox


def test_runs_in_microvm(microvm_sandbox: Sandbox) -> None:
    """最小端到端：在一台真 VM 里 run_code，并经 vsock 把结果拿回来。"""
    ex = microvm_sandbox.run_code("print(1 + 1)")
    assert ex.success
    assert ex.stdout.strip() == "2"


def test_state_persists_across_calls(microvm_sandbox: Sandbox) -> None:
    """microVM 内是 kernel 后端：变量跨 run_code 留存（阶段 2 的有状态语义带进 VM）。"""
    first = microvm_sandbox.run_code("x = 41")
    assert first.success
    second = microvm_sandbox.run_code("print(x + 1)")
    assert second.success
    assert second.stdout.strip() == "42"


def test_independent_guest_filesystem(microvm_sandbox: Sandbox) -> None:
    """真 VM 有自己的内核与根文件系统：宿主上真实存在的本测试文件，在 VM 里看不见。

    对比 backend="local"（同机宿主）跑同样代码会打印 True——这层差异就是 microVM 隔离。
    """
    host_path = str(pathlib.Path(__file__).resolve())
    ex = microvm_sandbox.run_code(f"import os; print(os.path.exists({host_path!r}))")
    assert ex.success
    assert ex.stdout.strip() == "False"


def test_vm_lifecycle_cleanup(microvm_sandbox: Sandbox) -> None:
    """VM 随 Sandbox 起灭：open 期间 firecracker 在跑，close 后进程退出且工作目录清理。"""
    proc = microvm_sandbox._proc
    workdir = microvm_sandbox._workdir
    assert proc is not None and proc.poll() is None       # VM 在跑
    assert workdir is not None and workdir.exists()

    microvm_sandbox.close()
    assert proc.poll() is not None                        # firecracker 已退出（VM 销毁）
    assert not workdir.exists()                            # per-VM 工作目录已清理
