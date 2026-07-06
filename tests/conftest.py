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
import socket
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


def _orch_flags() -> str:
    """The extra orchestrator flags for this run (MSB_ORCH_FLAGS). --nbd + --storage s3 are the defaults."""
    return os.environ.get("MSB_ORCH_FLAGS", "")


def _s3_mode() -> bool:
    """True when the orchestrator reads artifacts from object storage (the default); local-fs is the escape hatch."""
    return "--storage local-fs" not in _orch_flags()


def _nbd_mode() -> bool:
    """True when the rootfs is served over NBD -- the orchestrator default since Stage 22b, disabled only by
    --nbd=false, and an s3-mode feature (NBD streams the rootfs from the bucket). In this mode the default's
    warm snapshot is created in the bucket over the NBD stack (orchestrator --make-snapshot), so no local
    vendor/snapshot is built: build-snapshot.sh's file-backed boot -> NBD resume is the transition that
    triggered the re-snapshot panic (docs/STAGE22_DESIGN.md §12)."""
    return _s3_mode() and "--nbd=false" not in _orch_flags()


@functools.lru_cache(maxsize=1)
def ensure_snapshot() -> None:
    """Build the snapshot on demand if absent (boot VM + warm up kernel + snapshot, ~10s, produces a 512MB memfile).
    In normal use the developer pre-runs scripts/build-snapshot.sh."""
    if _nbd_mode():
        return  # Stage 22 E2: the snapshot lives in the bucket, created over NBD by the control_plane fixture
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


def _port_open(host: str, port: int) -> bool:
    """True if a TCP connect to host:port succeeds (used to detect / wait for Redis)."""
    try:
        with socket.create_connection((host, port), timeout=0.3):
            return True
    except OSError:
        return False


@functools.lru_cache(maxsize=1)
def ensure_redis() -> str:
    """Bring up the Redis that backs the sandbox routing catalog (Stage 14a), and return its
    address. Reuses one already listening on :6379 (a dev instance or a prior session).

    docker is already required (the microVM rootfs is exported from a docker image), so this
    adds no new *class* of dependency; a failure here is loud, not a skip -- that is the
    meaning of "flip the default" (docs/STAGE14_DESIGN.md, Decision 5). docker-compose.yml is
    the canonical spec, so we prefer `docker compose`; on engines whose docker has no compose
    plugin we fall back to a plain `docker run` of the same image, so the suite still runs.
    """
    host, port = "127.0.0.1", 6379
    addr = f"{host}:{port}"
    if _port_open(host, port):
        return addr  # reuse a Redis already up
    repo_root = pathlib.Path(__file__).resolve().parents[1]
    compose = ["docker", "compose", "-f", str(repo_root / "docker-compose.yml"),
               "up", "-d", "--wait", "redis"]
    if subprocess.run(compose, capture_output=True).returncode != 0:
        # No compose plugin: provision the same image directly via the engine.
        subprocess.run(["docker", "rm", "-f", "microsandbox-redis"], capture_output=True)
        subprocess.run(
            ["docker", "run", "-d", "--name", "microsandbox-redis",
             "-p", f"{host}:{port}:6379", "redis:7-alpine"],
            check=True,
        )
    deadline = time.time() + 30
    while time.time() < deadline:
        if _port_open(host, port):
            return addr
        time.sleep(0.1)
    raise RuntimeError(f"redis at {addr} did not come up within 30s")


@functools.lru_cache(maxsize=1)
def ensure_postgres() -> str:
    """Bring up the Postgres that backs the api metadata store (Stage 14b), and return its DSN.
    Reuses one already listening on :5432 (a dev instance or a prior session).

    Same rationale as ensure_redis: docker is already required, so this adds no new *class* of
    dependency, and a failure is loud, not a skip -- that is "flip the default" (Decision 5).
    Unlike Redis, Postgres opens its port mid-init and then restarts, so the docker-run
    fallback waits on `pg_isready` (not just an open port) before returning, so the api never
    connects before the database is accepting queries. docker-compose's --wait does the same
    via the healthcheck.
    """
    host, port = "127.0.0.1", 5432
    dsn = f"postgres://postgres@{host}:{port}/microsandbox?sslmode=disable"
    if _port_open(host, port):
        return dsn  # reuse a Postgres already up (assumed initialized)
    repo_root = pathlib.Path(__file__).resolve().parents[1]
    compose = ["docker", "compose", "-f", str(repo_root / "docker-compose.yml"),
               "up", "-d", "--wait", "postgres"]
    if subprocess.run(compose, capture_output=True).returncode == 0:
        return dsn  # compose --wait blocks until the healthcheck (pg_isready) passes
    # No compose plugin: provision the same image directly, then poll pg_isready for readiness.
    name = "microsandbox-postgres"
    subprocess.run(["docker", "rm", "-f", name], capture_output=True)
    subprocess.run(
        ["docker", "run", "-d", "--name", name,
         "-e", "POSTGRES_DB=microsandbox", "-e", "POSTGRES_HOST_AUTH_METHOD=trust",
         "-p", f"{host}:{port}:5432", "postgres:16-alpine"],
        check=True,
    )
    deadline = time.time() + 60
    while time.time() < deadline:
        ready = subprocess.run(
            ["docker", "exec", name, "pg_isready", "-U", "postgres", "-d", "microsandbox"],
            capture_output=True)
        if ready.returncode == 0 and _port_open(host, port):
            return dsn
        time.sleep(0.2)
    raise RuntimeError(f"postgres at {dsn} did not come up within 60s")


@functools.lru_cache(maxsize=1)
def ensure_minio() -> str:
    """Bring up the MinIO object store that holds template artifacts (Stage 15), and return its S3
    endpoint host:port. Reuses one already listening on :9000 (a dev instance or a prior session).

    Same rationale as ensure_redis/ensure_postgres: docker is already required, so this adds no new
    *class* of dependency, and a failure is loud, not a skip -- the "flip the default" cost. The minio
    server image ships neither curl nor mc, so there is no compose healthcheck; we poll the port and
    let the S3 provider's MakeBucket-if-absent be the real readiness signal (the seeder retries while
    minio finishes coming up).
    """
    host, port = "127.0.0.1", 9000
    endpoint = f"{host}:{port}"
    if _port_open(host, port):
        return endpoint
    repo_root = pathlib.Path(__file__).resolve().parents[1]
    compose = ["docker", "compose", "-f", str(repo_root / "docker-compose.yml"), "up", "-d", "minio"]
    if subprocess.run(compose, capture_output=True).returncode != 0:
        # No compose plugin: provision the same image directly via the engine.
        subprocess.run(["docker", "rm", "-f", "microsandbox-minio"], capture_output=True)
        subprocess.run(
            ["docker", "run", "-d", "--name", "microsandbox-minio",
             "-e", "MINIO_ROOT_USER=minioadmin", "-e", "MINIO_ROOT_PASSWORD=minioadmin",
             "-p", f"{host}:{port}:9000", "-p", "127.0.0.1:9001:9001",
             "minio/minio:latest", "server", "/data", "--console-address", ":9001"],
            check=True,
        )
    deadline = time.time() + 30
    while time.time() < deadline:
        if _port_open(host, port):
            return endpoint
        time.sleep(0.1)
    raise RuntimeError(f"minio at {endpoint} did not come up within 30s")


def _seed_template(repo_root: pathlib.Path, endpoint: str, name: str) -> None:
    """Publish a locally-built template's artifacts into MinIO via the Go seeder (go run msb-seed),
    so the orchestrator can materialize/stream them from the bucket. Retries briefly while minio
    finishes coming up (the port can open a beat before it serves S3 / the seeder's MakeBucket)."""
    cmd = ["go", "run", "./services/cmd/msb-seed",
           "--vendor-dir", str(repo_root / "vendor"), "--name", name, "--s3-endpoint", endpoint]
    deadline = time.time() + 30
    while True:
        r = subprocess.run(cmd, cwd=str(repo_root), capture_output=True, text=True)
        if r.returncode == 0:
            return
        if time.time() > deadline:
            raise RuntimeError(f"seeding template {name} into {endpoint} failed: {r.stderr}")
        time.sleep(0.5)


def _make_snapshot_over_nbd(repo_root: pathlib.Path, vendor: str, name: str) -> None:
    """Create a template's warm snapshot via the orchestrator's one-shot --make-snapshot mode (Stage 22
    E1/E2): cold-start over NBD at a stable rootfs path, warm the kernel, take a Full snapshot, and publish
    snapfile + compacted memfile + baked rootfs path into the bucket. This replaces build-snapshot.sh under
    --nbd so the base snapshot rides the same writable NBD stack it is later resumed on -- build-snapshot.sh
    boots the base over a plain file, and that file-backed -> NBD-resume transition triggered the
    writable-re-snapshot virtio-blk panic (docs/STAGE22_DESIGN.md §12). Needs root (the nbd module + a
    netns/TAP), so it goes through the same passwordless sudo the orchestrator server uses."""
    cmd = [str(repo_root / "vendor" / "orchestrator"),
           "--vendor-dir", vendor, "--make-snapshot", name]
    cmd += shlex.split(os.environ.get("MSB_ORCH_FLAGS", ""))
    if os.geteuid() != 0:
        cmd = ["sudo", "-n", "-E"] + cmd
    r = subprocess.run(cmd, capture_output=True, text=True)
    if r.returncode != 0:
        raise RuntimeError(f"orchestrator --make-snapshot {name} failed (rc={r.returncode}):\n{r.stdout}\n{r.stderr}")


@pytest.fixture(scope="session")
def control_plane(tmp_path_factory):
    """Build and run the Go services (orchestrator + client-proxy + api) once per session.

    Stage 8b split the control plane into orchestrator (gRPC SandboxService + the data
    proxy, TCP over the VM's NIC since Stage 12) and api (public REST). Stage 9 added
    client-proxy (the edge data proxy); Stage 14a moved the routing catalog into a shared
    Redis the api writes on create and client-proxy reads to route; Stage 14b moved the api's
    metadata store onto Postgres. Registration is load-bearing (a create rolls back if the
    Redis write fails), so the trio plus Redis and Postgres must all be up here. The SDK talks
    to the api (lifecycle) so this fixture yields the api base URL. Skips the whole microVM
    group when go / firecracker / kvm are unavailable.
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
    cp_data_addr = "127.0.0.1:8098"
    base_url = f"http://{api_addr}"
    redis_addr = ensure_redis()  # the shared routing catalog the api writes + client-proxy reads
    pg_dsn = ensure_postgres()   # the api's durable metadata store (Stage 14b)

    # Stage 15: template artifacts live in MinIO (the flipped default). When the orchestrator runs in
    # s3 mode (the default; MSB_ORCH_FLAGS can override to "--storage local-fs"), build the local
    # artifacts, bring up MinIO, and seed default + example into the bucket so the orchestrator can
    # materialize rootfs/snapfile and stream the memfile from it.
    orch_flags = os.environ.get("MSB_ORCH_FLAGS", "")
    if _s3_mode():
        ensure_example_template()  # the example template's rootfs (cold-start, no snapshot)
        s3_endpoint = ensure_minio()
        if _nbd_mode():
            # Stage 22 E2: seed default's rootfs, then create its warm snapshot over the SAME writable NBD
            # stack it will be resumed on (orchestrator --make-snapshot), instead of build-snapshot.sh's
            # file-backed boot + seeding it. The file-backed -> NBD transition triggered the re-snapshot
            # virtio-blk panic; see docs/STAGE22_DESIGN.md §12.
            ensure_rootfs()
            _seed_template(repo_root, s3_endpoint, "default")   # rootfs + alias (no local snapshot)
            _make_snapshot_over_nbd(repo_root, vendor, "default")
        else:
            ensure_snapshot()      # default's rootfs + file-backed snapshot (the --nbd=false escape hatch)
            _seed_template(repo_root, s3_endpoint, "default")
        _seed_template(repo_root, s3_endpoint, "example")
        # We deliberately do NOT clear local artifacts to "force" a bucket download here. The ensure_*
        # fixtures rebuild a missing local artifact via docker, and the root orchestrator's own
        # template builds leave a root-owned ~/.docker buildx lock that makes a forced normal-user
        # rebuild flaky -- so forcing a cold cache fights the fixtures. The materialize-download path is
        # covered hermetically by storage.TestMaterialize; the memfile-streaming payoff IS exercised
        # here (every snapshot restore streams default/memfile from MinIO). See docs/STAGE15_DESIGN.md.

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
        # (reads the shared Redis catalog), then the api (writes it); wait on each /health.
        _wait_healthy(f"http://{proxy_addr}/health", orch, logdir / "orchestrator.log")
        cp = subprocess.Popen(
            [str(repo_root / "vendor" / "client-proxy"),
             "--addr", cp_data_addr, "--redis-addr", redis_addr],
            stdout=cp_log, stderr=subprocess.STDOUT,
        )
        _wait_healthy(f"http://{cp_data_addr}/health", cp, logdir / "client-proxy.log")
        api = subprocess.Popen(
            [str(repo_root / "vendor" / "api"), "--addr", api_addr,
             "--orchestrator-grpc", grpc_addr, "--orchestrator-proxy", proxy_addr,
             "--redis-addr", redis_addr,
             "--data-url", f"http://{cp_data_addr}",
             "--store-dsn", pg_dsn,
             # Stage 16: seed the dev key (default team) the whole suite authenticates with,
             # plus a second team's key so test_auth.py can prove cross-team isolation.
             "--seed-api-keys", "msb_dev_key=default,msb_team_b_key=team_b"],
            stdout=api_log, stderr=subprocess.STDOUT,
        )
        _wait_healthy(base_url + "/health", api, logdir / "api.log")
        # Stage 16: every lifecycle call now needs an X-API-Key. Export the seeded dev key so
        # the existing fixtures' plain Sandbox(...) constructions authenticate unchanged; the
        # auth tests override/clear it per case.
        os.environ["MICROSANDBOX_API_KEY"] = "msb_dev_key"
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
