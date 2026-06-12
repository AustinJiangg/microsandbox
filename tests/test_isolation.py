"""阶段 1 隔离测试（docker 后端专属）。

这些断言对 local 后端不成立（它能看见宿主文件、能上网），所以不走参数化的
sandbox fixture，单独用 docker_sandbox。每条测试对应 docker run 的一个隔离
flag——跑通它们，「隔离」就不再是一个抽象词，而是看得见的行为差异。
"""

import pathlib
import subprocess

from microsandbox import Sandbox


def test_host_filesystem_invisible(docker_sandbox: Sandbox) -> None:
    """容器有独立的根文件系统（mount namespace）：宿主路径在容器内不存在。

    对比：一模一样的代码在 local 后端下会打印 True——这层对比就是文件系统隔离。
    """
    host_path = str(pathlib.Path(__file__).resolve())  # 本仓库内真实存在的文件
    ex = docker_sandbox.run_code(f"import os; print(os.path.exists({host_path!r}))")
    assert ex.success
    assert ex.stdout.strip() == "False"


def test_network_disabled(docker_sandbox: Sandbox) -> None:
    """--network none：容器没有网卡和路由，连外网立刻失败。

    故意用 IP 直连（1.1.1.1）避免 DNS 解析挂起；无路由时 connect 抛
    OSError: Network is unreachable——是快速失败，不会等满 settimeout。
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
    """--read-only 让根文件系统只读；--tmpfs /tmp 留出唯一可写区（内存盘）。"""
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
    """超时路径的生命周期回归：docker rm -f 真的把容器杀掉并删除了。

    杀掉 docker run 客户端进程并不能杀死容器（见 DockerBackend 注释），
    若清理链路失效，这里会看到残留的 microsandbox-exec-* 容器。
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
    assert leftovers.stdout.strip() == "", "超时后不应有残留容器"
