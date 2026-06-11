# 架构（ARCHITECTURE）

本文件解释 `microsandbox` 的设计，以及为什么这样设计能支撑「分阶段逼近 E2B」。

## 全局数据流

```
   你的程序
      │  sandbox.run_code("print(1)")
      ▼
┌───────────────┐   HTTP POST /execute        ┌────────────────────────┐
│  client (SDK) │ ───────────────────────────▶│   daemon (守护进程)      │
│  client.py    │                              │   server.py            │
│               │ ◀─────────────────────────── │                        │
└───────────────┘   SSE 流: OutputEvent...      │   ┌──────────────────┐ │
      ▲                                         │   │ backend (执行后端) │ │
      │ Execution(stdout/stderr/exit_code)      │   │ backend.py        │ │
   聚合返回                                       │   └──────────────────┘ │
                                                └────────────────────────┘
                                                  ↑ 隔离边界在这里逐阶段加强
```

## 三个组件的职责

### 1. protocol.py —— 契约（最重要）

定义 client 与 daemon 之间传什么：
- `ExecuteRequest`：要跑的代码、语言、超时。
- `OutputEvent`：一条流式输出（stdout / stderr / error / end）。
- `Execution`：client 把多条 event 聚合成的最终结果对象。

**为什么单独抽出来**：隔离层在阶段 0→3 会彻底换三次，但只要这份协议稳定，
client 几乎不用改。这是整个项目能「渐进演进」的支点。E2B 也是靠稳定的
client↔envd 协议来解耦 SDK 与底层运行时。

### 2. client.py —— SDK

用户唯一需要直接接触的层。提供 `Sandbox` 类、`run_code()`、流式回调。

- 阶段 0：`Sandbox` 构造时在本机拉起一个 daemon 子进程（`spawn_local`）。
- 阶段 1+：`spawn_local` 的逻辑替换为「向控制面申请一个沙箱（容器/VM），
  拿到地址再连上去」。但 `run_code` 这层 API 对用户保持不变。

### 3. server.py + backend.py —— daemon 与隔离层

- `server.py`（daemon）：在沙箱**内部**运行的常驻进程，监听 HTTP，
  把请求转交 backend，再把输出 SSE 流式回吐。对应 E2B 的 `envd`。
- `backend.py`（backend）：真正执行代码的地方，也是**隔离强度所在**。
  通过抽象基类 `ExecutionBackend` 解耦：

```
ExecutionBackend (抽象)
├── LocalSubprocessBackend   # 阶段 0：本机子进程，几乎无隔离
├── DockerBackend            # 阶段 1：容器（待实现）
└── FirecrackerBackend       # 阶段 3：microVM（待实现）
```

## 跨阶段演进策略

| 阶段 | 隔离层 | 主要改动文件 | client 是否需改 |
|------|--------|--------------|------------------|
| 0 | 本机子进程 | backend.py | — |
| 1 | Docker 容器 | 新增 DockerBackend；daemon 在容器内 | `_spawn_daemon` 改为建容器 |
| 2 | 容器内常驻 agent + 有状态 REPL | backend 改用持久 kernel | 基本不变 |
| 3 | Firecracker microVM | 新增 FirecrackerBackend + vsock | 连接方式改为 vsock/网络 |
| 4 | 产品化外围 | 新增控制面、池化、鉴权 | 新增 create/connect 流程 |

每次演进都遵循同一纪律：**新增 backend 实现，保持 protocol 稳定，
把改动尽量挡在 client 之外。** `tests/` 作为安全网，演进后必须全绿。

## 与 E2B 的对应关系（学完可对照阅读其源码）

| 本项目 | E2B 对应 | 说明 |
|--------|----------|------|
| client.py | E2B SDK（python/js） | 用户接口 |
| protocol.py | envd 的 gRPC/HTTP 协议 | 通信契约 |
| server.py (daemon) | `envd` | 沙箱内常驻 agent |
| backend.py | Firecracker 编排 + 沙箱运行时 | 隔离与执行 |
| 阶段 4 控制面 | orchestrator / API | 沙箱生命周期管理 |
