"""阶段 3c 测试：Firecracker 快照恢复（毫秒级冷启动）。

从一份预热好的快照（含已就绪的 Jupyter kernel）恢复 microVM——跳过内核引导与 kernel
冷启动。需要 vendor/snapshot/{vmstate,memfile}（scripts/build-snapshot.sh 生成，缺则现
build）+ 可访问的 /dev/kvm；缺 firecracker/内核则整组 skip。

注意：快照的 vsock uds 路径固定，故同一时刻只能恢复一台（pytest 默认顺序跑，无碍）。
并发恢复 + 预热池属于阶段 4。
"""

import time

from microsandbox import Sandbox


def test_restore_runs_and_is_stateful(snapshot_sandbox: Sandbox) -> None:
    """恢复出来的 VM 里 kernel 已热：首跑即出结果，且变量跨 run_code 留存。"""
    ex = snapshot_sandbox.run_code("print(6 * 7)")
    assert ex.success
    assert ex.stdout.strip() == "42"

    snapshot_sandbox.run_code("w = 100")
    ex2 = snapshot_sandbox.run_code("print(w + 1)")
    assert ex2.success
    assert ex2.stdout.strip() == "101"


def test_restore_is_fast(snapshot_ready) -> None:
    """恢复就绪远快于冷启动（冷启动 ~0.94s；恢复实测 ~30-40ms）。

    自己掐表：从 Sandbox(from_snapshot=True) 构造到就绪的耗时。用宽松上限（< 0.6s）
    避免机器抖动 flaky——光这条就足以证明「跳过内核引导」带来的量级差。
    """
    t0 = time.time()
    sb = Sandbox(backend="microvm", from_snapshot=True)
    ready = time.time() - t0
    try:
        assert sb.run_code("print(1)").stdout.strip() == "1"
    finally:
        sb.close()
    assert ready < 0.6, f"恢复就绪耗时 {ready * 1000:.0f}ms，超出预期（冷启动才 ~940ms）"
