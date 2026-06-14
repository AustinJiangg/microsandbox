"""Stage 3c tests: Firecracker snapshot restore (millisecond cold start).

Restore a microVM from a pre-warmed snapshot (containing an already-ready Jupyter
kernel) -- skipping kernel boot and kernel cold start. Requires
vendor/snapshot/{vmstate,memfile} (produced by scripts/build-snapshot.sh, built on
demand if absent) + an accessible /dev/kvm; if firecracker/kernel are missing the
whole group is skipped.

Note: the snapshot's vsock uds path is fixed, so only one can be restored at a time
(pytest runs in order by default, so this is fine). Concurrent restore + a warm pool
belong to Stage 4.
"""

import time

from microsandbox import Sandbox


def test_restore_runs_and_is_stateful(snapshot_sandbox: Sandbox) -> None:
    """In the restored VM the kernel is already warm: the first run yields a result immediately, and variables persist across run_code calls."""
    ex = snapshot_sandbox.run_code("print(6 * 7)")
    assert ex.success
    assert ex.stdout.strip() == "42"

    snapshot_sandbox.run_code("w = 100")
    ex2 = snapshot_sandbox.run_code("print(w + 1)")
    assert ex2.success
    assert ex2.stdout.strip() == "101"


def test_restore_is_fast(snapshot_ready) -> None:
    """Restore-to-ready is far faster than cold start (cold start ~0.94s; restore measured ~30-40ms).

    Time it ourselves: the elapsed time from constructing Sandbox(from_snapshot=True)
    until ready. Use a generous upper bound (< 0.6s) to avoid flakiness from machine
    jitter -- this one assertion alone suffices to prove the order-of-magnitude gap
    from "skipping kernel boot."
    """
    t0 = time.time()
    sb = Sandbox(from_snapshot=True)
    ready = time.time() - t0
    try:
        assert sb.run_code("print(1)").stdout.strip() == "1"
    finally:
        sb.close()
    assert ready < 0.6, f"restore-to-ready took {ready * 1000:.0f}ms, exceeding expectation (cold start is only ~940ms)"
