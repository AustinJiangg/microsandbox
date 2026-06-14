"""Shared test infrastructure for the Firecracker microVM sandbox.

Every Sandbox is a microVM, so the tests need firecracker + a guest kernel +
rootfs under vendor/ and an accessible /dev/kvm. When any of those is missing the
microVM fixtures skip as a group, so `pytest` still completes on machines without
KVM (the vsock-transport unit tests in test_transport.py have no such dependency
and always run).
"""

import functools
import os
import pathlib
import subprocess

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


@pytest.fixture
def sandbox():
    """A cold-started microVM sandbox -- the common fixture for end-to-end / stateful / files tests."""
    if not firecracker_available():
        pytest.skip("firecracker/kernel/kvm incomplete, skipping microVM cases")
    ensure_rootfs()
    sb = Sandbox()
    yield sb
    sb.close()


@pytest.fixture
def snapshot_ready():
    """Only ensures the snapshot is ready (skip if unavailable); does not construct a Sandbox -- for cases that time things themselves."""
    if not firecracker_available():
        pytest.skip("firecracker/kernel/kvm incomplete, skipping snapshot cases")
    ensure_snapshot()


@pytest.fixture
def snapshot_sandbox():
    """A microVM sandbox restored from a snapshot in milliseconds (the kernel inside the VM is already warm)."""
    if not firecracker_available():
        pytest.skip("firecracker/kernel/kvm incomplete, skipping snapshot cases")
    ensure_snapshot()
    sb = Sandbox(from_snapshot=True)
    yield sb
    sb.close()
