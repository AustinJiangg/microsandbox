"""Stage 2a tests: the daemon moves into a resident container (becoming envd-like).

Verifies that Stage 2's "master/slave inversion" really happened:
  - the daemon no longer runs on the host, but inside a container created and
    held long-term by the client -- so host files are invisible inside the
    container (mount namespace isolation);
  - this container is born when the Sandbox is created and dies on close
    (lifecycle + cleanup).

Note: state persistence (a "real REPL" where two consecutive run_code calls
share variables) is a Stage 2b matter and isn't tested here yet -- inside 2a the
container still runs a stateless subprocess per execution.
"""

import pathlib
import subprocess

import pytest

from microsandbox import Sandbox


def _container_ids(name: str, *, include_stopped: bool = False) -> str:
    """Look up a container id by name; with include_stopped=True, stopped ones count too (uses -a)."""
    cmd = ["docker", "ps", "--filter", f"name={name}", "-q"]
    if include_stopped:
        cmd.insert(2, "-a")
    return subprocess.run(cmd, capture_output=True, text=True).stdout.strip()


def test_daemon_runs_inside_container(resident_sandbox: Sandbox) -> None:
    """The daemon really is inside the container: a file that genuinely exists on the host is invisible inside the sandbox.

    For comparison, running the same code under backend="local" (daemon on the
    host) prints True -- this difference is direct evidence that "the daemon moved
    into the container." Note 2a only read-only-mounts src/ into the container;
    this test file is not within the mount, so its host path doesn't exist inside
    the container.
    """
    host_path = str(pathlib.Path(__file__).resolve())
    ex = resident_sandbox.run_code(
        f"import os; print(os.path.exists({host_path!r}))"
    )
    assert ex.success
    assert ex.stdout.strip() == "False"


def test_container_lifecycle(resident_sandbox: Sandbox) -> None:
    """The resident container lives and dies with the Sandbox: running while open, deleted after close (no leftovers)."""
    name = resident_sandbox._container
    assert name is not None
    assert _container_ids(name), "while the Sandbox is open, the resident container should be running"

    resident_sandbox.close()
    assert not _container_ids(name, include_stopped=True), "there should be no leftover container after close"


def test_failed_startup_cleans_up_container(docker_env, monkeypatch) -> None:
    """Regression: when the health check fails during construction, the already-started container must be cleaned up as a safety net and not leak.

    Background: inside __init__, once _spawn_resident_container succeeds the
    container is already running; if _wait_until_healthy then raises, the
    exception propagates out of __init__ -- at this point `with` hasn't entered
    __enter__, so __exit__/close won't fire. If __init__ doesn't clean up itself,
    it leaves an orphaned leftover container. Here we deliberately force the health
    check to fail, verifying the safety-net close really deletes the container.
    """
    captured: dict[str, str] = {}
    real_spawn = Sandbox._spawn_resident_container

    def spy_spawn(self: Sandbox) -> None:
        real_spawn(self)                   # actually bring the container up
        captured["name"] = self._container  # record the container name to later confirm it was deleted

    def boom(self: Sandbox) -> None:
        raise RuntimeError("forced health-check failure")

    monkeypatch.setattr(Sandbox, "_spawn_resident_container", spy_spawn)
    monkeypatch.setattr(Sandbox, "_wait_until_healthy", boom)

    with pytest.raises(RuntimeError, match="forced health-check failure"):
        Sandbox(backend="container")

    assert "name" in captured, "the container should have come up (otherwise the leak path wasn't exercised)"
    assert not _container_ids(captured["name"], include_stopped=True), \
        "after a failed construction the already-started container should be cleaned up as a safety net, leaving no leftover"
