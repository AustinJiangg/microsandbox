"""Stage 3b tests: the Firecracker microVM backend (end-to-end over vsock).

Runs inside a real microVM: a separate guest kernel + KVM boundary, control
channel over vsock, with not a single line changed in the daemon or protocol.
Requires firecracker + vmlinux + rootfs.ext4 under vendor/ (see docs/MICROVM_DESIGN.md
§7) and an accessible /dev/kvm; if any is missing the whole group is skipped,
staying green as usual on other machines / CI.
"""

import pathlib
import urllib.error
import urllib.request

import pytest

from microsandbox import Sandbox


def test_runs_in_microvm(sandbox: Sandbox) -> None:
    """Minimal end-to-end: run_code inside a real VM and pull the result back over vsock."""
    ex = sandbox.run_code("print(1 + 1)")
    assert ex.success
    assert ex.stdout.strip() == "2"


def test_state_persists_across_calls(sandbox: Sandbox) -> None:
    """Inside the microVM it's the kernel backend: variables persist across run_code calls (Stage 2's stateful semantics carried into the VM)."""
    first = sandbox.run_code("x = 41")
    assert first.success
    second = sandbox.run_code("print(x + 1)")
    assert second.success
    assert second.stdout.strip() == "42"


def test_independent_guest_filesystem(sandbox: Sandbox) -> None:
    """A real VM has its own kernel and root filesystem: this test file, which genuinely exists on the host, is invisible inside the VM.

    For comparison, the same code run directly on the host (outside any VM) prints
    True -- this difference is microVM isolation.
    """
    host_path = str(pathlib.Path(__file__).resolve())
    ex = sandbox.run_code(f"import os; print(os.path.exists({host_path!r}))")
    assert ex.success
    assert ex.stdout.strip() == "False"


def test_machine_config_resource_limits(sandbox: Sandbox) -> None:
    """Resource limits are enforced by the Firecracker machine-config (vcpu_count=1, mem_size_mib=512).

    Compared to the Stage 1/2 cgroup quotas (--cpus/--memory, acting on a container
    inside the shared kernel), this is a "VM quota": the guest sees an entire machine
    with 1 vCPU and ~512MB -- far smaller than the host, proving the quota took effect.
    """
    cpu = sandbox.run_code("import os; print(os.cpu_count())").stdout.strip()
    assert cpu == "1"  # only 1 vCPU is granted (the host has many more cores)

    # /proc/meminfo's first line MemTotal (KB) -> MB; the kernel reserves some, so it's slightly under 512.
    mem_mb = int(
        sandbox.run_code(
            "print(int(open('/proc/meminfo').readline().split()[1]) // 1024)"
        ).stdout.strip()
    )
    assert 300 < mem_mb < 600  # ~512MB, far smaller than the host, proving the VM memory quota took effect


def test_vm_lifecycle_cleanup(sandbox: Sandbox) -> None:
    """The VM lives and dies with the Sandbox: it runs code while open, and after close
    the control plane has destroyed it -- deleting it again then 404s.

    As of Stage 4b the SDK is pure HTTP and holds no VM-internal handles, so we assert
    the lifecycle through the control plane's behaviour rather than poking at
    processes / files.
    """
    assert sandbox.run_code("print(1)").stdout.strip() == "1"   # the VM is up and running code

    sandbox_id = sandbox._sandbox_id
    sandbox.close()
    # The control plane destroyed the VM, so it no longer knows this id.
    with pytest.raises(RuntimeError):
        sandbox._control_plane("DELETE", f"/sandboxes/{sandbox_id}")


def test_api_is_lifecycle_only(sandbox: Sandbox) -> None:
    """Stage 9c: the data path lives on client-proxy, not the api. The api's old
    passthrough route (/sandboxes/{id}/...) is gone, so hitting it directly now 404s --
    while the SDK's data calls (which go to client-proxy via the learned data_url) work.
    """
    # The SDK's data path works -- it goes to client-proxy, not the api.
    assert sandbox.run_code("print(1)").stdout.strip() == "1"

    # The api no longer proxies the data path: the old passthrough URL 404s.
    url = f"{sandbox._base_url}/sandboxes/{sandbox._sandbox_id}/execute"
    req = urllib.request.Request(
        url, data=b"{}", method="POST", headers={"Content-Type": "application/json"}
    )
    with pytest.raises(urllib.error.HTTPError) as exc:
        urllib.request.urlopen(req)
    assert exc.value.code == 404
