"""Shared test infrastructure for the Firecracker microVM sandbox.

Every Sandbox is created via the Go control plane, so the tests need the go
toolchain plus firecracker + a guest kernel + rootfs under vendor/ and an
accessible /dev/kvm. When any of those is missing the microVM fixtures skip as a
group, so `pytest` still completes on machines without them. The data proxy's
own unit tests now live in Go (services/pkg/proxy/proxy_test.go -- TCP since Stage
12, run with `go test ./services/...`) and need none of this.
"""

import functools
import os
import pathlib
import shlex
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
def networking_available() -> bool:
    """Stage 12: a VM now gets a per-sandbox netns/TAP/veth (services/pkg/network), so the
    orchestrator must run as root, and (Stage 12b) build-snapshot.sh creates the snapshot's TAP via
    `sudo -n ip`. On this single-box model both are passwordless sudo (a one-time /etc/sudoers.d
    entry -- see docs/STAGE12_DESIGN.md) plus /dev/net/tun. When any is missing the microVM group
    skips, exactly like the /dev/kvm gate, so pytest still completes.
    """
    if not os.path.exists("/dev/net/tun"):
        return False
    if os.geteuid() == 0:
        return True
    # Two passwordless grants from the same drop-in: the orchestrator binary (runs as root for the
    # netns/TAP setup) and `ip` (build-snapshot.sh makes the snapshot's TAP as the normal user).
    # /usr/bin/true stands in for the orchestrator grant; `ip -V` is a harmless probe of the ip one.
    def granted(*cmd: str) -> bool:
        return subprocess.run(["sudo", "-n", *cmd], capture_output=True).returncode == 0

    return granted("/usr/bin/true") and granted("ip", "-V")


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


def _wait_healthy(url: str, proc: subprocess.Popen, log_path: pathlib.Path) -> None:
    """Poll url until it answers 200, failing fast if the process exits early.

    A connection refused before the server binds raises URLError (an OSError subclass),
    so we just retry until the deadline.
    """
    deadline = time.time() + 15
    while time.time() < deadline:
        if proc.poll() is not None:
            raise RuntimeError(
                f"{url}: process exited early (code {proc.returncode}); see {log_path}"
            )
        try:
            with urllib.request.urlopen(url, timeout=1) as r:
                if r.status == 200:
                    return
        except OSError:
            time.sleep(0.05)
    raise RuntimeError(f"{url} did not become healthy; see {log_path}")


@pytest.fixture(scope="session")
def control_plane(tmp_path_factory):
    """Build and run the Go services (orchestrator + client-proxy + api) once per session.

    Stage 8b split the control plane into orchestrator (gRPC SandboxService + the data
    proxy, TCP over the VM's NIC since Stage 12) and api (public REST). Stage 9 adds client-proxy (the edge data proxy that
    owns the routing catalog): the api registers each sandbox's route in client-proxy on
    create, and -- once Stage 9b lands -- the SDK sends the data path through client-proxy.
    Registration is load-bearing (a create rolls back if it fails), so the trio must all be
    up here. The SDK talks to the api (lifecycle) so this fixture yields the api base URL.
    Skips the whole microVM group when go / firecracker / kvm are unavailable.
    """
    if not firecracker_available():
        pytest.skip("firecracker/kernel/kvm incomplete, skipping microVM cases")
    if not go_available():
        pytest.skip("go toolchain not found, skipping services cases")
    if not networking_available():
        pytest.skip("orchestrator needs root for per-sandbox networking (Stage 12a): "
                    "missing /dev/net/tun or passwordless sudo -- see docs/STAGE12_DESIGN.md")

    repo_root = pathlib.Path(__file__).resolve().parents[1]
    ensure_rootfs()  # the orchestrator needs the rootfs for any cold start
    subprocess.run([str(repo_root / "scripts" / "build-services.sh")], check=True)

    vendor = str(repo_root / "vendor")
    api_addr, grpc_addr, proxy_addr = "127.0.0.1:8099", "127.0.0.1:9099", "127.0.0.1:5099"
    cp_data_addr, cp_internal_addr = "127.0.0.1:8098", "127.0.0.1:5098"
    base_url = f"http://{api_addr}"

    logdir = tmp_path_factory.mktemp("services")
    orch_log = open(logdir / "orchestrator.log", "wb")
    cp_log = open(logdir / "client-proxy.log", "wb")
    api_log = open(logdir / "api.log", "wb")
    # Stage 12a: the orchestrator allocates a per-sandbox netns/TAP/veth (pkg/network) on cold
    # start, which needs root. On this single box that is passwordless sudo (the /etc/sudoers.d
    # entry in docs/STAGE12_DESIGN.md); when already root the wrapper is skipped. sudo forwards
    # SIGTERM to the child, so the teardown below still reaches the orchestrator's destroyAll.
    # -E preserves the developer's environment (PATH/HOME/Go caches) so the orchestrator's
    # template-build subprocess (go/docker/mkfs) works just as it did before it ran as root;
    # the sudoers drop-in grants SETENV + !secure_path for exactly this command.
    orch_cmd = [str(repo_root / "vendor" / "orchestrator"),
                "--grpc-addr", grpc_addr, "--proxy-addr", proxy_addr, "--vendor-dir", vendor]
    # Stage 13b: MSB_ORCH_FLAGS passes extra orchestrator flags through to the binary -- set it
    # to "--uffd" to exercise the UFFD snapshot-restore backend with this same e2e suite. Default
    # empty keeps the File backend, so ordinary runs are unchanged. Appended before the sudo wrap
    # so the flags reach the orchestrator, not sudo.
    orch_cmd += shlex.split(os.environ.get("MSB_ORCH_FLAGS", ""))
    if os.geteuid() != 0:
        orch_cmd = ["sudo", "-n", "-E"] + orch_cmd
    orch = subprocess.Popen(orch_cmd, stdout=orch_log, stderr=subprocess.STDOUT)
    cp = api = None
    try:
        # Start order: orchestrator (the api dials it for lifecycle), then client-proxy
        # (the api writes routes to it), then the api; wait on each one's /health.
        _wait_healthy(f"http://{proxy_addr}/health", orch, logdir / "orchestrator.log")
        cp = subprocess.Popen(
            [str(repo_root / "vendor" / "client-proxy"),
             "--addr", cp_data_addr, "--internal-addr", cp_internal_addr],
            stdout=cp_log, stderr=subprocess.STDOUT,
        )
        _wait_healthy(f"http://{cp_data_addr}/health", cp, logdir / "client-proxy.log")
        api = subprocess.Popen(
            [str(repo_root / "vendor" / "api"), "--addr", api_addr,
             "--orchestrator-grpc", grpc_addr, "--orchestrator-proxy", proxy_addr,
             "--client-proxy-internal", cp_internal_addr,
             "--data-url", f"http://{cp_data_addr}",
             "--db", str(logdir / "microsandbox.db")],
            stdout=api_log, stderr=subprocess.STDOUT,
        )
        _wait_healthy(base_url + "/health", api, logdir / "api.log")
        yield base_url
    finally:
        # SIGTERM the api first, then client-proxy, then the orchestrator (which destroys
        # any running VMs).
        for proc in (api, cp, orch):
            if proc is None:
                continue
            proc.terminate()
            try:
                proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                proc.kill()
        orch_log.close()
        cp_log.close()
        api_log.close()


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


@pytest.fixture
def api_template_build(control_plane):
    """Ensure the agent base image exists (template recipes are FROM it) and yield the api
    URL, for the Stage 10 test that builds a template through the api's TemplateService."""
    ensure_agent_image()
    return control_plane
