"""Shared test infrastructure for the Firecracker microVM sandbox.

Every Sandbox is created via the Go control plane, so the tests need the go
toolchain plus firecracker + a guest kernel + rootfs under vendor/ and an
accessible /dev/kvm. When any of those is missing the microVM fixtures skip as a
group, so `pytest` still completes on machines without them. The vsock bridge's
own unit tests now live in Go (services/pkg/proxy/proxy_test.go, run with
`go test ./services/...`) and need none of this.
"""

import functools
import os
import pathlib
import shutil
import subprocess
import time
import urllib.request

import pytest

from microsandbox import Sandbox
from microsandbox.backend import DEFAULT_AGENT_IMAGE


@functools.lru_cache(maxsize=1)
def firecracker_available() -> bool:
    """Run microVM cases only when the firecracker binary + kernel are ready and /dev/kvm is readable/writable.

    The rootfs is not checked here (ensure_rootfs can build it on demand); if any
    of these is missing the whole group is skipped -- on machines / CI without KVM
    the microVM cases skip automatically and pytest stays green.
    """
    vendor = pathlib.Path(__file__).resolve().parents[1] / "vendor"
    if not (vendor / "firecracker").exists() or not (vendor / "vmlinux").exists():
        return False
    return os.path.exists("/dev/kvm") and os.access("/dev/kvm", os.R_OK | os.W_OK)


@functools.lru_cache(maxsize=1)
def go_available() -> bool:
    """The Stage 4 control plane is a Go binary built on demand; skip its cases when go is absent."""
    return shutil.which("go") is not None


@functools.lru_cache(maxsize=1)
def ensure_agent_image() -> None:
    """docker build the agent image once if it isn't local (first build is slow: installs ipykernel).

    docker is only a one-time build tool here -- the microVM's rootfs is exported from
    this image. In normal use the developer pre-builds it with
    docker build -t microsandbox-agent .
    """
    inspect = subprocess.run(
        ["docker", "image", "inspect", DEFAULT_AGENT_IMAGE], capture_output=True
    )
    if inspect.returncode != 0:
        repo_root = pathlib.Path(__file__).resolve().parents[1]
        subprocess.run(
            ["docker", "build", "-t", DEFAULT_AGENT_IMAGE, str(repo_root)], check=True
        )


@functools.lru_cache(maxsize=1)
def ensure_rootfs() -> None:
    """Build rootfs.ext4 on demand if absent (first time is slow: docker export + mkfs.ext4 -d).
    In normal use the developer pre-builds it with scripts/build-rootfs.sh."""
    repo_root = pathlib.Path(__file__).resolve().parents[1]
    if (repo_root / "vendor" / "rootfs.ext4").exists():
        return
    ensure_agent_image()  # the rootfs is exported from the agent image, so it must exist first
    subprocess.run([str(repo_root / "scripts" / "build-rootfs.sh")], check=True)


@functools.lru_cache(maxsize=1)
def ensure_snapshot() -> None:
    """Build the snapshot on demand if absent (boot VM + warm up kernel + snapshot, ~10s, produces a 512MB memfile).
    In normal use the developer pre-runs scripts/build-snapshot.sh."""
    repo_root = pathlib.Path(__file__).resolve().parents[1]
    snap = repo_root / "vendor" / "snapshot"
    if (snap / "vmstate").exists() and (snap / "memfile").exists():
        return
    ensure_rootfs()  # the snapshot is based on the rootfs, so make sure it exists first
    subprocess.run([str(repo_root / "scripts" / "build-snapshot.sh")], check=True)


@functools.lru_cache(maxsize=1)
def ensure_example_template() -> None:
    """Build the 'example' template's rootfs on demand (docker export -> ext4, no snapshot).

    Used by the Stage 6 named-template e2e; in normal use a developer pre-builds it
    with scripts/build-template.sh example. --no-snapshot keeps it cheap and KVM-free
    to build (the test that boots it still needs KVM, and auto-skips without it)."""
    repo_root = pathlib.Path(__file__).resolve().parents[1]
    rootfs = repo_root / "vendor" / "templates" / "example" / "rootfs.ext4"
    if rootfs.exists():
        return
    ensure_agent_image()  # the example template is FROM microsandbox-agent
    subprocess.run(
        [str(repo_root / "scripts" / "build-template.sh"), "example", "--no-snapshot"],
        check=True,
    )


@pytest.fixture(scope="session")
def control_plane(tmp_path_factory):
    """Build and run the Go control plane once for the whole test session.

    As of Stage 4 every Sandbox is created by asking this service (rather than the SDK
    forking firecracker itself), so the microVM tests need it running. Skips the whole
    microVM group when go / firecracker / kvm are unavailable, keeping pytest green on
    machines / CI without them. Yields the control plane's base URL.
    """
    if not firecracker_available():
        pytest.skip("firecracker/kernel/kvm incomplete, skipping microVM cases")
    if not go_available():
        pytest.skip("go toolchain not found, skipping control-plane cases")

    repo_root = pathlib.Path(__file__).resolve().parents[1]
    ensure_rootfs()  # the orchestrator needs the rootfs for any cold start
    subprocess.run([str(repo_root / "scripts" / "build-services.sh")], check=True)

    addr = "127.0.0.1:8099"
    base_url = f"http://{addr}"
    log_path = tmp_path_factory.mktemp("orchestrator") / "orchestrator.log"
    log_fh = open(log_path, "wb")
    proc = subprocess.Popen(
        [str(repo_root / "vendor" / "orchestrator"),
         "--addr", addr, "--vendor-dir", str(repo_root / "vendor")],
        stdout=log_fh, stderr=subprocess.STDOUT,
    )
    try:
        # Wait for the control plane's own /health (a connection refused before it binds
        # raises URLError, which is an OSError subclass).
        deadline = time.time() + 10
        while time.time() < deadline:
            try:
                with urllib.request.urlopen(base_url + "/health", timeout=1) as r:
                    if r.status == 200:
                        break
            except OSError:
                time.sleep(0.05)
        else:
            raise RuntimeError(f"orchestrator did not become healthy; see {log_path}")
        yield base_url
    finally:
        proc.terminate()  # SIGTERM -> the orchestrator destroys any VMs still running
        try:
            proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            proc.kill()
        log_fh.close()


@pytest.fixture
def sandbox(control_plane):
    """A cold-started microVM sandbox -- the common fixture for end-to-end / stateful / files tests."""
    sb = Sandbox(base_url=control_plane)
    yield sb
    sb.close()


@pytest.fixture
def snapshot_ready(control_plane):
    """Ensure the snapshot is ready and yield the control-plane URL, for cases that
    construct (and time) their own Sandbox(from_snapshot=True)."""
    ensure_snapshot()
    return control_plane


@pytest.fixture
def snapshot_sandbox(control_plane):
    """A microVM sandbox restored from a snapshot in milliseconds (the kernel inside the VM is already warm)."""
    ensure_snapshot()
    sb = Sandbox(from_snapshot=True, base_url=control_plane)
    yield sb
    sb.close()


@pytest.fixture
def example_sandbox(control_plane):
    """A sandbox booted from the 'example' template (Stage 6): the stock image plus a
    marker file, cold-started (no snapshot needed)."""
    ensure_example_template()
    sb = Sandbox(template="example", base_url=control_plane)
    yield sb
    sb.close()
