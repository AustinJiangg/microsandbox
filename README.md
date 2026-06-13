# microsandbox

一个**从零实现、逐步逼近 [E2B](https://github.com/e2b-dev/E2B) 的学习用代码沙箱**。

目标不是做产品，而是搞懂「AI 代码沙箱到底是怎么实现的」。项目分阶段演进，
从最简单的本机子进程，一路做到 Firecracker microVM。当前在 **阶段 2**（容器内常驻 agent）。

## 快速开始

```bash
pip install -e ".[dev]"
docker pull python:3.12-slim   # 阶段 1 容器后端的基础镜像（一次性）
python examples/quickstart.py
```

用起来像这样（手感对齐 E2B SDK）：

```python
from microsandbox import Sandbox

with Sandbox() as sandbox:
    ex = sandbox.run_code("print('hello from the sandbox')")
    print(ex.stdout)        # hello from the sandbox
    print(ex.success)       # True

    # 流式拿输出
    sandbox.run_code(
        "for i in range(3): print(i)",
        on_stdout=lambda chunk: print("live:", chunk.strip()),
    )

# 阶段 1：一行切换到 Docker 容器隔离，run_code 用法完全不变
with Sandbox(backend="docker") as sandbox:
    ex = sandbox.run_code("import platform; print(platform.node())")
    print(ex.stdout)        # 容器 ID，而不是你的主机名

# 阶段 2b：常驻 Jupyter kernel，变量跨 run_code 留存（真正的有状态 REPL）
# 需先构建 agent 镜像：docker build -t microsandbox-agent .
with Sandbox(backend="kernel") as sandbox:
    sandbox.run_code("x = 41")
    print(sandbox.run_code("print(x + 1)").stdout)   # 42 —— 第二次能用第一次的变量
```

## 项目结构

```
microsandbox/
├── CLAUDE.md                  # Claude Code 项目记忆（约定、进度、架构）
├── README.md
├── pyproject.toml
├── docs/
│   ├── ARCHITECTURE.md        # 三层解耦设计与跨阶段演进策略
│   └── ROADMAP.md             # 分阶段路线图（每阶段的目标与步骤）
├── src/microsandbox/
│   ├── protocol.py            # client↔daemon 协议（最重要的稳定边界）
│   ├── client.py              # SDK：Sandbox / run_code
│   ├── server.py              # daemon：HTTP + SSE（对应 E2B 的 envd）
│   └── backend.py             # 执行后端（隔离层，逐阶段替换）
├── examples/quickstart.py
└── tests/                     # 双后端参数化 e2e 测试 + 容器隔离测试
```

## 演进路线

| 阶段 | 隔离方式 | 状态 |
|------|----------|------|
| 0 | 本机子进程 | ✅ |
| 1 | Docker 容器 | ✅ |
| 2 | 容器内常驻 agent + 有状态 REPL | 🚧 当前（2a/2b 完成，2c 进行中） |
| 3 | Firecracker microVM | ⬜ |
| 4 | 产品化外围（池化/模板/鉴权） | ⬜ |

详见 `docs/ROADMAP.md`。

## ⚠️ 安全须知

默认的 `local` 后端（本机子进程）**几乎没有隔离**，代码能访问你本机的文件、
网络、环境变量。阶段 1 的 `docker` 后端有了文件系统/网络隔离和资源限制，
但容器与宿主**共享内核、逃逸面不小**，仍不足以执行完全不可信的代码。
**本项目仅供本地学习**，切勿对外提供服务。内核级强隔离从阶段 3（microVM）起。
