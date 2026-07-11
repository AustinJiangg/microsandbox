"""Snapshot restore tests: Firecracker snapshot restore (millisecond cold start).

Restore a microVM from a pre-warmed snapshot (containing an already-ready Jupyter
kernel) -- skipping kernel boot and kernel cold start. Requires
vendor/snapshot/{vmstate,memfile} (produced by scripts/build-snapshot.sh, built on
demand if absent) + an accessible /dev/kvm; if firecracker/kernel are missing the
whole group is skipped.

As of Stage 5a several sandboxes can be restored from the one snapshot at once: the
control plane gives each restored VM its own data channel. Stage 5a did this with a
per-VM vsock uds (vsock_override); Stage 12 retired vsock and instead gives each VM its
own network slot (netns/TAP/veth/DNAT, services/pkg/network), so they reach distinct
host addresses. test_concurrent_restores_are_isolated exercises that -- the prerequisite
for the warm pool (Stage 5b). See docs/STAGE5_DESIGN.md + docs/STAGE12_DESIGN.md.
"""

import os
import time
from concurrent.futures import ThreadPoolExecutor

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
    """Restore-to-ready skips the guest kernel boot that a cold start pays.

    Time it ourselves: elapsed from constructing Sandbox(from_snapshot=True) until ready.
    Since Stage 12 every sandbox also gets its own network inline (netns + TAP + veth + DNAT,
    services/pkg/network) -- ~0.5-1s of sequential `ip` calls on WSL2 -- so this *unpooled*
    restore is no longer the ~30-40ms it once was; the bound below is deliberately loose. The
    real ms-latency path is the warm pool, which pre-allocates the slot in the background (so
    Get() pays neither the kernel boot nor the network setup). This case only proves restore
    still avoids the guest kernel boot; see the Stage 12 perf note for the slot-setup cost.

    The bound is a sanity check that restore completes (well under the ~10s health timeout), not a
    latency SLA: on WSL2 the per-sandbox `ip` setup alone varies ~0.5-1.5s under load, so the plain
    bound is 2.5s. Stage 21 (--nbd) serves the rootfs lazily over NBD from object storage, so an
    unpooled restore additionally pays the guest faulting its working set over NBD on first access
    (vs reading a local materialized rootfs) -- measured ~3.5s -- hence a looser 6s bound in --nbd
    mode. The warm pool hides both (a pooled VM is pre-allocated and pre-faulted).
    """
    # --nbd is the orchestrator default since Stage 22b, so an empty MSB_ORCH_FLAGS still runs over NBD;
    # only an explicit --nbd=false turns it off (matching test_layered_snapshot_via_api's detection).
    nbd = "--nbd=false" not in os.environ.get("MSB_ORCH_FLAGS", "")
    bound = 6.0 if nbd else 2.5
    t0 = time.time()
    sb = Sandbox(from_snapshot=True, base_url=snapshot_ready)
    ready = time.time() - t0
    try:
        assert sb.run_code("print(1)").stdout.strip() == "1"
    finally:
        sb.close()
    assert ready < bound, (
        f"restore-to-ready took {ready * 1000:.0f}ms (bound {bound}s; per-sandbox net setup"
        f"{' + lazy NBD rootfs streaming' if nbd else ''}; the warm pool is the ms-latency path)"
    )


def test_concurrent_restores_are_isolated(snapshot_ready) -> None:
    """Restore several sandboxes from the one snapshot *concurrently*, give each a
    distinct variable, then read them all back -- proving the restores coexist as
    independent VMs (own kernel, own network slot) with no cross-talk. Before Stage 5a
    the snapshot's baked-in data channel was shared, so this raced and could not be done.
    """
    n = 3
    base_url = snapshot_ready

    def restore(_: int) -> Sandbox:
        return Sandbox(from_snapshot=True, base_url=base_url)

    # Restore concurrently -- the path that collided on the shared baked socket pre-5a.
    with ThreadPoolExecutor(max_workers=n) as pool:
        boxes = list(pool.map(restore, range(n)))
    try:
        # Distinct control-plane ids => genuinely distinct VMs.
        assert len({sb._sandbox_id for sb in boxes}) == n

        # Set a unique value in each, then read them all back in a second pass: a shared
        # kernel would surface as one VM's write leaking into the others.
        for i, sb in enumerate(boxes):
            assert sb.run_code(f"x = {i * 100 + 7}").success
        for i, sb in enumerate(boxes):
            ex = sb.run_code("print(x)")
            assert ex.success
            assert ex.stdout.strip() == str(i * 100 + 7), f"box {i} saw {ex.stdout!r} -- cross-talk?"
    finally:
        for sb in boxes:
            sb.close()
