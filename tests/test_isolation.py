"""Stage 1 isolation tests (docker backend only).

These assertions don't hold for the local backend (it can see host files and
reach the network), so they don't use the parametrized sandbox fixture but
docker_sandbox on its own. Each test corresponds to one of docker run's isolation
flags -- once they pass, "isolation" is no longer an abstract word but a visible
behavioral difference.
"""

import pathlib
import subprocess

from microsandbox import Sandbox


def test_host_filesystem_invisible(docker_sandbox: Sandbox) -> None:
    """The container has its own root filesystem (mount namespace): the host path doesn't exist inside the container.

    For comparison: the exact same code under the local backend prints True --
    that comparison is filesystem isolation.
    """
    host_path = str(pathlib.Path(__file__).resolve())  # a file that genuinely exists within this repo
    ex = docker_sandbox.run_code(f"import os; print(os.path.exists({host_path!r}))")
    assert ex.success
    assert ex.stdout.strip() == "False"


def test_network_disabled(docker_sandbox: Sandbox) -> None:
    """--network none: the container has no NIC or routes, so reaching the internet fails immediately.

    Deliberately connect by raw IP (1.1.1.1) to avoid DNS resolution hanging; with
    no route, connect raises OSError: Network is unreachable -- a fast failure that
    doesn't wait out the full settimeout.
    """
    ex = docker_sandbox.run_code(
        "import socket\n"
        "s = socket.socket()\n"
        "s.settimeout(3)\n"
        "s.connect(('1.1.1.1', 80))\n"
    )
    assert not ex.success
    assert "unreachable" in ex.stderr.lower() or "OSError" in ex.stderr


def test_readonly_root_but_tmp_writable(docker_sandbox: Sandbox) -> None:
    """--read-only makes the root filesystem read-only; --tmpfs /tmp leaves the only writable area (a RAM disk)."""
    ex = docker_sandbox.run_code(
        "try:\n"
        "    open('/etc/hacked', 'w')\n"
        "except OSError:\n"
        "    print('root-blocked')\n"
        "open('/tmp/probe.txt', 'w').write('x')\n"
        "print('tmp-ok')\n"
    )
    assert ex.success
    assert "root-blocked" in ex.stdout
    assert "tmp-ok" in ex.stdout


def test_timeout_cleans_up_container(docker_sandbox: Sandbox) -> None:
    """Lifecycle regression for the timeout path: docker rm -f really does kill and delete the container.

    Killing the docker run client process does not kill the container (see the
    DockerBackend comments); if the cleanup chain fails, you'd see leftover
    microsandbox-exec-* containers here.
    """
    docker_sandbox.timeout_seconds = 0.5
    ex = docker_sandbox.run_code("import time; time.sleep(30)")
    assert not ex.success
    assert ex.error is not None and "timed out" in ex.error

    leftovers = subprocess.run(
        ["docker", "ps", "-a", "--filter", "name=microsandbox-exec", "-q"],
        capture_output=True,
        text=True,
    )
    assert leftovers.stdout.strip() == "", "there should be no leftover container after a timeout"
