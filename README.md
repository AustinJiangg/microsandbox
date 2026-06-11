# microsandbox

一个**从零实现、逐步逼近 [E2B](https://github.com/e2b-dev/E2B) 的学习用代码沙箱**。

目标不是做产品，而是搞懂「AI 代码沙箱到底是怎么实现的」。项目分阶段演进，
从最简单的本机子进程，一路做到 Firecracker microVM。当前在 **阶段 0**。

## 快速开始

```bash
pip install -e ".[dev]"
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
└── tests/test_sandbox.py
```

## 演进路线

| 阶段 | 隔离方式 | 状态 |
|------|----------|------|
| 0 | 本机子进程 | ✅ 当前 |
| 1 | Docker 容器 | ⬜ |
| 2 | 容器内常驻 agent + 有状态 REPL | ⬜ |
| 3 | Firecracker microVM | ⬜ |
| 4 | 产品化外围（池化/模板/鉴权） | ⬜ |

详见 `docs/ROADMAP.md`。

## ⚠️ 安全须知

阶段 0 的子进程后端**几乎没有隔离**，代码能访问你本机的文件、网络、环境变量。
**仅供本地学习**，切勿用它执行不可信代码或对外提供服务。真正的隔离从阶段 1 起、
强隔离从阶段 3（microVM）起。
