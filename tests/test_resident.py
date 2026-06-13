"""阶段 2a 测试：daemon 搬进常驻容器（envd 化）。

验证阶段 2 的「主从关系反转」真的发生了：
  - daemon 不再跑在宿主，而在一个由 client 创建并长期持有的容器里——
    所以容器内看不见宿主文件（mount namespace 隔离）；
  - 这个容器随 Sandbox 创建而起、随 close 而灭（生命周期 + 清理）。

注意：状态留存（连续两次 run_code 共享变量的「真 REPL」）是阶段 2b 的事，
这里还不测——2a 容器内仍是每次执行一个无状态子进程。
"""

import pathlib
import subprocess

import pytest

from microsandbox import Sandbox


def _container_ids(name: str, *, include_stopped: bool = False) -> str:
    """按名字查容器 id；include_stopped=True 时连已停止的也算（用 -a）。"""
    cmd = ["docker", "ps", "--filter", f"name={name}", "-q"]
    if include_stopped:
        cmd.insert(2, "-a")
    return subprocess.run(cmd, capture_output=True, text=True).stdout.strip()


def test_daemon_runs_inside_container(resident_sandbox: Sandbox) -> None:
    """daemon 真的在容器里：宿主上真实存在的文件，在沙箱里看不见。

    对比 backend="local"（daemon 在宿主）跑同样代码会打印 True——这层差异
    就是「daemon 搬进容器」的直接证据。注意 2a 只把 src/ 只读挂进容器，
    本测试文件不在挂载范围内，所以它的宿主路径在容器内不存在。
    """
    host_path = str(pathlib.Path(__file__).resolve())
    ex = resident_sandbox.run_code(
        f"import os; print(os.path.exists({host_path!r}))"
    )
    assert ex.success
    assert ex.stdout.strip() == "False"


def test_container_lifecycle(resident_sandbox: Sandbox) -> None:
    """常驻容器随 Sandbox 起灭：open 期间在跑，close 后被删除（不留残留）。"""
    name = resident_sandbox._container
    assert name is not None
    assert _container_ids(name), "Sandbox 打开期间，常驻容器应当在运行"

    resident_sandbox.close()
    assert not _container_ids(name, include_stopped=True), "close 后不应有残留容器"


def test_failed_startup_cleans_up_container(docker_env, monkeypatch) -> None:
    """回归：构造期健康检查失败时，已起的容器要被兜底清理掉，不能泄漏。

    背景：__init__ 里 _spawn_resident_container 成功后容器就已经在跑了，若紧接着
    _wait_until_healthy 抛异常，异常会从 __init__ 穿出去——此时 with 还没进
    __enter__，__exit__/close 不会触发。若 __init__ 不自己兜底，就会留下没人收的
    残留容器。这里故意让健康检查必失败，验证兜底 close 真的把容器删了。
    """
    captured: dict[str, str] = {}
    real_spawn = Sandbox._spawn_resident_container

    def spy_spawn(self: Sandbox) -> None:
        real_spawn(self)                   # 真的把容器起起来
        captured["name"] = self._container  # 记下容器名，待会儿核对它被删了

    def boom(self: Sandbox) -> None:
        raise RuntimeError("forced health-check failure")

    monkeypatch.setattr(Sandbox, "_spawn_resident_container", spy_spawn)
    monkeypatch.setattr(Sandbox, "_wait_until_healthy", boom)

    with pytest.raises(RuntimeError, match="forced health-check failure"):
        Sandbox(backend="container")

    assert "name" in captured, "容器应当已经起来过（否则没测到泄漏路径）"
    assert not _container_ids(captured["name"], include_stopped=True), \
        "构造失败后应兜底清理掉已起的容器，不留残留"
