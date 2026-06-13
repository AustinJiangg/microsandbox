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


def test_machine_config_resource_limits(microvm_sandbox: Sandbox) -> None:
    """资源限制由 Firecracker machine-config 强制（vcpu_count=1, mem_size_mib=512）。

    对照阶段 1/2 的 cgroup 限额（--cpus/--memory，作用于共享内核里的容器），这里是
    「VM 配额」：guest 看到的就是一个 1 vCPU、~512MB 的整机——比宿主小得多，证明配额生效。
    """
    cpu = microvm_sandbox.run_code("import os; print(os.cpu_count())").stdout.strip()
    assert cpu == "1"  # 只给 1 个 vCPU（宿主核多得多）

    # /proc/meminfo 首行 MemTotal（KB）→ MB；内核自留一点，故略小于 512。
    mem_mb = int(
        microvm_sandbox.run_code(
            "print(int(open('/proc/meminfo').readline().split()[1]) // 1024)"
        ).stdout.strip()
    )
    assert 300 < mem_mb < 600  # ~512MB，远小于宿主，证明 VM 内存配额生效


def test_vm_lifecycle_cleanup(microvm_sandbox: Sandbox) -> None:
    """VM 随 Sandbox 起灭：open 期间 firecracker 在跑，close 后进程退出且工作目录清理。"""
    proc = microvm_sandbox._proc
    workdir = microvm_sandbox._workdir
    assert proc is not None and proc.poll() is None       # VM 在跑
    assert workdir is not None and workdir.exists()

    microvm_sandbox.close()
    assert proc.poll() is not None                        # firecracker 已退出（VM 销毁）
    assert not workdir.exists()                            # per-VM 工作目录已清理
