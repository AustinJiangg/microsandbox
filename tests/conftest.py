"""Shared test infrastructure: backend parametrization.

Core idea: run the same test bodies once under the local backend and once under
the docker backend -- this is the direct proof of the promise that "the protocol
contract stays fixed while the isolation scheme can be swapped."
On machines without Docker the docker-side cases are skipped as a group, while
the local side stays fully green as usual.
"""

import functools
import os
import pathlib
import shutil
import subprocess

import pytest

from microsandbox import Sandbox
from microsandbox.backend import DEFAULT_AGENT_IMAGE, DEFAULT_DOCKER_IMAGE


@functools.lru_cache(maxsize=1)
def docker_available() -> bool:
    """Probe whether docker is available.

    Made a module-level cached function rather than a fixture: the skipif marks
    below need a value at "test collection" time, when the fixture system isn't
    set up yet; lru_cache guarantees the probe runs only once per session.
    """
    if shutil.which("docker") is None:
        return False
    try:
        probe = subprocess.run(["docker", "info"], capture_output=True, timeout=10)
    except subprocess.TimeoutExpired:
        return False
    return probe.returncode == 0


@functools.lru_cache(maxsize=1)
def ensure_image() -> None:
    """Pull the image once if it isn't local (first run ~30-60s), so pytest works out of the box.

    On the normal execution path we use --pull never (never implicitly pull an
    image and eat into the timeout budget); the tests pre-pull as an exception,
    so that "clone repo -> pytest" works in one step.
    """
    inspect = subprocess.run(
        ["docker", "image", "inspect", DEFAULT_DOCKER_IMAGE], capture_output=True
    )
    if inspect.returncode != 0:
        subprocess.run(["docker", "pull", DEFAULT_DOCKER_IMAGE], check=True)


@functools.lru_cache(maxsize=1)
def ensure_agent_image() -> None:
    """docker build the Stage 2b agent image once if it isn't local (first build is slow, has to install ipykernel).

    Like ensure_image, this is for "clone repo -> pytest" out of the box; in
    normal use the image is pre-built by the developer with
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


requires_docker = pytest.mark.skipif(
    not docker_available(), reason="docker unavailable, skipping container backend cases"
)


@pytest.fixture(
    params=[
        "local",
        pytest.param("docker", marks=requires_docker),
        # Stage 2a: after the daemon moves into a resident container, the same
        # end-to-end cases run again under this new topology -- extending the
        # promise of "protocol fixed, isolation/deployment swappable" from
        # "swap the backend" to "swap the entire deployment form."
        pytest.param("container", marks=requires_docker),
    ]
)
def sandbox(request: pytest.FixtureRequest):
    """Parametrized sandbox fixture: every test that uses it automatically becomes three cases [local]/[docker]/[container]."""
    if request.param in ("docker", "container"):
        ensure_image()
    sb = Sandbox(backend=request.param)
    yield sb
    sb.close()


@pytest.fixture
def docker_sandbox():
    """Isolation tests only: runs on the docker backend only (isolation assertions don't hold for the local backend)."""
    if not docker_available():
        pytest.skip("docker unavailable, skipping isolation tests")
    ensure_image()
    sb = Sandbox(backend="docker")
    yield sb
    sb.close()


@pytest.fixture
def resident_sandbox():
    """Stage 2a only: a sandbox whose daemon runs inside a resident container (backend="container")."""
    if not docker_available():
        pytest.skip("docker unavailable, skipping resident-container tests")
    ensure_image()
    sb = Sandbox(backend="container")
    yield sb
    sb.close()  # if the test already closed it explicitly, this is an idempotent no-op


@pytest.fixture
def kernel_sandbox():
    """Stage 2b only: a stateful sandbox whose daemon hosts a Jupyter kernel inside a resident container."""
    if not docker_available():
        pytest.skip("docker unavailable, skipping kernel backend tests")
    ensure_agent_image()
    sb = Sandbox(backend="kernel")
    yield sb
    sb.close()


@pytest.fixture
def docker_env():
    """Only ensures the docker environment is ready (skip if unavailable, and pre-pull the image); does not create a Sandbox for you.

    For tests that need to control the Sandbox construction process themselves --
    e.g. regression tests that deliberately make construction fail to verify
    error-path behavior.
    """
    if not docker_available():
        pytest.skip("docker unavailable, skipping")
    ensure_image()


# ---- Stage 3: Firecracker microVM ----


@functools.lru_cache(maxsize=1)
def firecracker_available() -> bool:
    """Run microVM cases only when the firecracker binary + kernel are ready and /dev/kvm is readable/writable.

    The rootfs is not checked here (ensure_rootfs can build it on demand); if any
    of these three is missing the whole group is skipped -- on other machines / CI
    the microVM cases skip automatically and pytest stays green (same as when
    docker is unavailable).
    """
    vendor = pathlib.Path(__file__).resolve().parents[1] / "vendor"
    if not (vendor / "firecracker").exists() or not (vendor / "vmlinux").exists():
        return False
    return os.path.exists("/dev/kvm") and os.access("/dev/kvm", os.R_OK | os.W_OK)


@functools.lru_cache(maxsize=1)
def ensure_rootfs() -> None:
    """Build rootfs.ext4 on demand if absent (first time is slow: docker export + mkfs.ext4 -d). Lets the
    microVM cases work out of the box on machines with firecracker/kernel ready; in normal use the developer pre-builds it."""
    repo_root = pathlib.Path(__file__).resolve().parents[1]
    if (repo_root / "vendor" / "rootfs.ext4").exists():
        return
    ensure_agent_image()  # the rootfs is exported from the agent image, so it must exist first
    subprocess.run([str(repo_root / "scripts" / "build-rootfs.sh")], check=True)


@pytest.fixture
def microvm_sandbox():
    """Stage 3 only: a sandbox whose daemon runs inside a Firecracker microVM, connected over vsock."""
    if not firecracker_available():
        pytest.skip("firecracker/kernel/kvm incomplete, skipping microVM cases")
    ensure_rootfs()
    sb = Sandbox(backend="microvm")
    yield sb
    sb.close()


@functools.lru_cache(maxsize=1)
def ensure_snapshot() -> None:
    """Build the snapshot on demand if absent (boot VM + warm up kernel + snapshot, ~10s, produces a 512MB memfile).
    Lets the snapshot cases work out of the box on machines with the materials ready; in normal use the developer pre-runs build-snapshot.sh."""
    repo_root = pathlib.Path(__file__).resolve().parents[1]
    snap = repo_root / "vendor" / "snapshot"
    if (snap / "vmstate").exists() and (snap / "memfile").exists():
        return
    ensure_rootfs()  # the snapshot is based on the rootfs, so make sure it exists first
    subprocess.run([str(repo_root / "scripts" / "build-snapshot.sh")], check=True)


@pytest.fixture
def snapshot_ready():
    """Only ensures the snapshot is ready (skip if unavailable); does not construct a Sandbox for you -- for cases that time things themselves."""
    if not firecracker_available():
        pytest.skip("firecracker/kernel/kvm incomplete, skipping snapshot cases")
    ensure_snapshot()


@pytest.fixture
def snapshot_sandbox():
    """Stage 3c only: a microVM sandbox restored from a snapshot in milliseconds (the kernel inside the VM is already warm)."""
    if not firecracker_available():
        pytest.skip("firecracker/kernel/kvm incomplete, skipping snapshot cases")
    ensure_snapshot()
    sb = Sandbox(backend="microvm", from_snapshot=True)
    yield sb
    sb.close()
