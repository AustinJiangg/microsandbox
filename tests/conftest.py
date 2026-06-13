"""测试公共设施：后端参数化。

核心思路：同一套测试体，分别在 local 和 docker 两个后端下各跑一遍——
这是「协议契约不变、隔离方案可换」承诺的直接证明。
没装 Docker 的机器上 docker 侧用例整组 skip，local 侧照常全绿。
"""

import functools
import pathlib
import shutil
import subprocess

import pytest

from microsandbox import Sandbox
from microsandbox.backend import DEFAULT_AGENT_IMAGE, DEFAULT_DOCKER_IMAGE


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


@functools.lru_cache(maxsize=1)
def ensure_agent_image() -> None:
    """阶段 2b 的 agent 镜像不在本地就 docker build 一次（首次较慢，要装 ipykernel）。

    和 ensure_image 一样，是为了「克隆仓库 → pytest」开箱即用；正常使用时镜像
    由开发者自己 docker build -t microsandbox-agent . 预先构建。
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
    not docker_available(), reason="docker 不可用，跳过容器后端用例"
)


@pytest.fixture(
    params=[
        "local",
        pytest.param("docker", marks=requires_docker),
        # 阶段 2a：daemon 搬进常驻容器后，同一套端到端用例在这个新拓扑下再跑一遍——
        # 把「协议不变、隔离/部署可换」的承诺从「换 backend」扩展到「换整个部署形态」。
        pytest.param("container", marks=requires_docker),
    ]
)
def sandbox(request: pytest.FixtureRequest):
    """参数化的沙箱 fixture：每个用它的测试自动变成 [local]/[docker]/[container] 三个用例。"""
    if request.param in ("docker", "container"):
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


@pytest.fixture
def resident_sandbox():
    """阶段 2a 专用：daemon 跑在常驻容器里的沙箱（backend="container"）。"""
    if not docker_available():
        pytest.skip("docker 不可用，跳过常驻容器测试")
    ensure_image()
    sb = Sandbox(backend="container")
    yield sb
    sb.close()  # 若测试已显式 close 过，这里是幂等空操作


@pytest.fixture
def kernel_sandbox():
    """阶段 2b 专用：daemon 在常驻容器里托管 Jupyter kernel 的有状态沙箱。"""
    if not docker_available():
        pytest.skip("docker 不可用，跳过 kernel 后端测试")
    ensure_agent_image()
    sb = Sandbox(backend="kernel")
    yield sb
    sb.close()


@pytest.fixture
def docker_env():
    """只确保 docker 环境就绪（不可用则 skip、并预拉镜像），不替你创建 Sandbox。

    给需要自己掌控 Sandbox 构造过程的测试用——例如要故意让构造失败、
    验证错误路径行为的回归测试。
    """
    if not docker_available():
        pytest.skip("docker 不可用，跳过")
    ensure_image()
