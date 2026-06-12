"""测试公共设施：后端参数化。

核心思路：同一套测试体，分别在 local 和 docker 两个后端下各跑一遍——
这是「协议契约不变、隔离方案可换」承诺的直接证明。
没装 Docker 的机器上 docker 侧用例整组 skip，local 侧照常全绿。
"""

import functools
import shutil
import subprocess

import pytest

from microsandbox import Sandbox
from microsandbox.backend import DEFAULT_DOCKER_IMAGE


@functools.lru_cache(maxsize=1)
def docker_available() -> bool:
    """探测 docker 是否可用。

    做成模块级缓存函数而非 fixture：下面的 skipif 标记在「测试收集」阶段
    就要求值，那时 fixture 体系还没建立；lru_cache 保证整个会话只探测一次。
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
    """镜像不在本地就拉一次（首跑约 30-60 秒），让 pytest 开箱即用。

    正常执行路径上是 --pull never（绝不隐式拉镜像吃掉超时预算），
    测试里例外地预拉，是为了「克隆仓库 → pytest」一步到位。
    """
    inspect = subprocess.run(
        ["docker", "image", "inspect", DEFAULT_DOCKER_IMAGE], capture_output=True
    )
    if inspect.returncode != 0:
        subprocess.run(["docker", "pull", DEFAULT_DOCKER_IMAGE], check=True)


requires_docker = pytest.mark.skipif(
    not docker_available(), reason="docker 不可用，跳过容器后端用例"
)


@pytest.fixture(params=["local", pytest.param("docker", marks=requires_docker)])
def sandbox(request: pytest.FixtureRequest):
    """参数化的沙箱 fixture：每个用它的测试自动变成 [local] / [docker] 两个用例。"""
    if request.param == "docker":
        ensure_image()
    sb = Sandbox(backend=request.param)
    yield sb
    sb.close()


@pytest.fixture
def docker_sandbox():
    """隔离测试专用：只在 docker 后端上跑（隔离断言对 local 后端不成立）。"""
    if not docker_available():
        pytest.skip("docker 不可用，跳过隔离测试")
    ensure_image()
    sb = Sandbox(backend="docker")
    yield sb
    sb.close()
