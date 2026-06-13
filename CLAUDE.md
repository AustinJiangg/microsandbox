# CLAUDE.md

> 本文件是 **Claude Code 的项目记忆**。每次在本仓库里开会话时它都会被自动加载。
> 请把项目的长期约定、架构决策、当前进度写在这里，而不是散落在对话中。

## 这个项目是什么

`microsandbox` 是一个**学习用**的代码执行沙箱，目标是从零开始、分阶段
逐步逼近 [E2B](https://github.com/e2b-dev/E2B) 的核心实现。重点是**理解原理**，
不是做产品。代码追求清晰、注释充分、便于增量演进。

## 核心架构（务必保持稳定）

三层解耦，参见 `docs/ARCHITECTURE.md`：

1. **client（SDK）** — `src/microsandbox/client.py`
   用户写 `Sandbox().run_code(...)` 面对的接口。手感对齐 E2B SDK。
2. **协议（wire protocol）** — `src/microsandbox/protocol.py`
   client 与 daemon 之间的契约。**这是最重要的边界，演进时尽量保持向后兼容。**
3. **daemon（守护进程）+ backend（执行后端）** — `server.py` / `backend.py`
   daemon 在沙箱内监听请求；backend 是真正执行代码的隔离层。

**关键原则**：换隔离方案（子进程 → 容器 → microVM）时，只新增 `ExecutionBackend`
的实现并替换 daemon 默认 backend，**client 与 protocol 尽量不动**。

## 分阶段路线图（当前进度见 docs/ROADMAP.md）

- [x] **阶段 0**：本机子进程后端，跑通 client/protocol/daemon 骨架
- [x] **阶段 1**：Docker 容器后端（真正的隔离起点）
- [x] **阶段 2**：容器内常驻 agent + 有状态 REPL（对齐 E2B 的 envd）
      （见 `docs/STAGE2_DESIGN.md`；2a daemon 搬进常驻容器、2b 常驻 Jupyter kernel
      有状态 REPL、2c 文件/shell API —— 全部完成）
- [ ] **阶段 3**：Firecracker microVM 后端 ← **当前在这里**（下一步）
- [ ] **阶段 4**：产品化外围（沙箱池、模板、鉴权等）

## 开发约定

- Python ≥ 3.11，阶段 0/1 **零运行时依赖**（只用标准库）。新依赖必须在
  对应阶段才引入，并写明理由。
- 阶段 1 决策：调 Docker 走 `docker` CLI + asyncio 子进程，**不引入 docker-py**
  ——保持零依赖，且 docker-py 是同步库，放进 asyncio daemon 必须套线程池，反而更绕。
- 阶段 2 决策：2a 仍零依赖（client 走 `docker` CLI 起常驻容器，源码只读挂载进
  `python:3.12-slim`，免建镜像）；**2b 首次引入运行时依赖** `ipykernel`+`jupyter_client`
  （对齐 E2B 的 Jupyter kernel，做成 `[kernel]` 可选 extra + backend 内懒导入，
  非 kernel 路径仍零依赖；依赖装进 `Dockerfile` 构建的 agent 镜像，源码仍只读挂载）。
- 注释用中文，解释「为什么」而非「是什么」，尤其标注「未来哪个阶段会替换此处」。
- 每个阶段都要保证 `tests/` 全绿。测试是跨阶段重构的安全网。
- 安全红线：阶段 0/1 的隔离不足以运行不可信代码，**严禁**在文档或代码里
  暗示它们可以对外接收任意输入。强隔离从阶段 3 起。

## 常用命令

```bash
# 安装（开发模式）
pip install -e ".[dev]"

# 运行示例（自动起停 daemon）
python examples/quickstart.py

# 阶段 1 前置（一次性）：拉基础镜像
docker pull python:3.12-slim

# 阶段 2b 前置（一次性）：构建 agent 镜像（含 ipykernel/jupyter_client）
docker build -t microsandbox-agent .

# 手动单独起 daemon（--backend docker 用容器隔离，默认 local；kernel 仅在 agent 镜像内有意义）
python -m microsandbox.server --port 49152
python -m microsandbox.server --backend docker

# 跑测试（自动参数化 local/docker/container；kernel 走专属用例。
# 无 docker 时容器/kernel 侧自动 skip；缺 agent 镜像时测试会自动 docker build）
pytest

# 清理残留容器（正常情况不会有，进程被 kill -9 后可能残留）：
# 阶段 1 一次性执行容器 microsandbox-exec-*，阶段 2 常驻沙箱容器 microsandbox-sandbox-*
docker ps -a --filter name=microsandbox- -q | xargs -r docker rm -f

# 阶段 2a：用常驻容器后端（daemon 搬进容器，状态暂不留存）
python -c 'from microsandbox import Sandbox; s=Sandbox(backend="container"); print(s.run_code("print(1+1)").stdout); s.close()'

# 阶段 2b：有状态 REPL（变量跨 run_code 留存；需先 docker build agent 镜像）
python -c 'from microsandbox import Sandbox; s=Sandbox(backend="kernel"); s.run_code("x=41"); print(s.run_code("print(x+1)").stdout); s.close()'

# 跑单个测试
pytest tests/test_sandbox.py::test_timeout -v
```

## 给 Claude 的工作提示

- 改动隔离层时，先读 `docs/ARCHITECTURE.md` 确认边界，再动手。
- 进入新阶段时，先更新 `docs/ROADMAP.md` 勾选进度，并在本文件顶部「当前在这里」标记同步。
- 引入新隔离后端时，遵循 `ExecutionBackend` 抽象，不要把隔离细节泄漏到 client。
