# 阶段 2 设计：容器内常驻 agent + 有状态 REPL

> 本文是阶段 2 的设计与实现计划。阶段 2 是整个项目**改动最大**的一关——
> 它把「隔离环境」从「每次执行临时创建」变成「沙箱级别长期存活」，
> 对应 E2B 的核心架构 `envd`。建议配合 `docs/ARCHITECTURE.md` 一起读。

---

## 1. 这一关在解决什么：一次「主从关系反转」

阶段 0/1 和阶段 2 的根本区别**不是「换了个 backend」**，而是 daemon 与
容器的主从关系彻底反过来了。

**阶段 1（现状）—— 宿主 daemon 当家，容器是一次性打工仔：**

```
宿主机
┌─────────────────────────────────────────────┐
│  client (Popen 起 daemon)                     │
│     │                                          │
│     ▼                                          │
│  daemon (server.py，常驻宿主)                   │
│     │ 每次 run_code                             │
│     ▼                                          │
│  DockerBackend.execute()                       │
│     └── docker run 一个临时容器 ──► 跑完即死     │
│         (--rm, --network none)                 │
└─────────────────────────────────────────────┘
```

- daemon 在**宿主机**（`client.py` 的 `_spawn_daemon` 用 `subprocess.Popen`）。
- 每次 `run_code` → `backend.py` 的 `DockerBackend.execute` **临时起一个容器**，
  跑完即删（`--rm`）。容器 `--network none` 彻底断网，因为它不需要跟谁通信。

**阶段 2（E2B 的 envd 模型）—— 容器当家，daemon 住进容器里常驻：**

```
宿主机                          沙箱容器（长期存活，一个 Sandbox 一个）
┌──────────────┐               ┌──────────────────────────────────┐
│ client       │  HTTP/SSE     │  daemon (server.py) ← 就是 envd    │
│  docker run  │ ────────────► │     │                              │
│  -d 一次      │  映射端口      │     ▼                              │
│  连进去       │ ◄──────────── │  PersistentBackend（持久解释器）    │
│  docker rm -f│               │     └── 变量跨 run_code 留存         │
└──────────────┘               └──────────────────────────────────┘
```

- **容器一次创建、长期存活**：一个 `Sandbox` 对应一个容器，不再是「一次执行一个」。
- **`server.py` 整个搬进容器**，作为容器入口常驻运行——这就是 E2B 的 `envd`。
- **谁来创建容器？** 不能再是 backend 了——daemon 自己住在容器里，没法创建
  自己所在的容器（鸡生蛋）。所以**创建容器的职责上移到 client**。

一句话总结：

> **阶段 2 = 把 `server.py` 原样搬进容器，把「创建隔离环境」的活从 backend 挪到 client。**

`server.py` 的 `SandboxServer` 几乎不用改——这正是「协议稳定、隔离可换」
这条主线最漂亮的一次验证。

---

## 2. 职责迁移表（对照真实代码看改了哪里）

| 代码位置 | 阶段 1 现在做什么 | 阶段 2 改成什么 |
|----------|------------------|----------------|
| `client.py:_spawn_daemon` | 宿主 `subprocess.Popen` 起 daemon | `docker run -d` 一个常驻容器，把 daemon 跑在里面 |
| `client.py:_wait_until_healthy` | 轮询 `/health`，失败读 Popen 的 stderr | 轮询 `/health`，失败读 `docker logs` |
| `client.py:close` | `proc.terminate()` | `docker rm -f <容器名>` |
| `server.py` | 监听 `127.0.0.1`，在宿主跑 | **代码不动**；容器内用 `--host 0.0.0.0` 启动即可被宿主连到 |
| `backend.py` | `DockerBackend` 每次起临时容器 | 新增持久解释器后端，跑在容器内（见 §4） |
| `protocol.py` | `/execute` 一个端点 | **向后兼容新增** `/files/*`、`/commands`（见 §4 的 2c） |

**关键观察**：阶段 2 几乎不碰 `server.py` 和 `protocol.py` 的既有部分。
改动集中在 client（接管容器生命周期）和 backend（换成持久解释器）。
这就是三层解耦的红利。

---

## 3. 拆成三小步（每步都保持既有测试全绿）

阶段 2 太大，拆成三个可独立验证的小步，符合「一步一个概念」的节奏：

### 2a —— envd 化（搬家，状态先不留存）← 本轮要做

把 daemon 搬进常驻容器，证明「daemon 代码没动，只是换了运行的地方」。

- 开发期不建镜像：直接把宿主的 `src/` **只读挂载**进 `python:3.12-slim`，
  设 `PYTHONPATH` 后 `python -m microsandbox.server`。零依赖、改代码免重建。
- `client.py` 新增 `backend="container"`：`docker run -d` 起常驻容器、映射端口、
  健康探测、`close` 时 `docker rm -f`。
- 容器内 daemon 仍用 `LocalSubprocessBackend`（每次 exec 一个子进程）——
  **此刻它又安全了**，因为整个 daemon 已经在容器里。状态还不留存，
  但「反转」这个架构论点已经成立。
- **`server.py` 一行都不用改**（它早就支持 `--host/--port/--backend`），
  这是 2a 最想证明的事。

**验收**：`backend="container"` 跑通 `run_code`；既有 7 个端到端用例在
`local`/`docker`/`container` 三拓扑下参数化全绿（7×3）。

### 2b —— 有状态 REPL（这一步才是「真 REPL」）

用**持久解释器**替换「每次 fork 子进程」，让变量跨 `run_code` 留存。

- **机制：Jupyter / IPython kernel**（决策见 §5）。daemon 托管一个常驻
  Jupyter kernel，`run_code` 经 ZMQ 发 `execute_request`，从 iopub 收
  stdout/stderr/execute_result/error，映射回我们既有的 `OutputEvent`。
- 这一步**引入依赖**：`ipykernel` + `jupyter_client`。因此 2b 才引入
  **Dockerfile**（把这俩 pip 装进 agent 镜像），容器内 daemon 用
  `--backend kernel` 启动。
- 超时/崩溃语义：kernel 卡死就中断（SIGINT）或重启 kernel（丢状态但
  daemon 与容器不死）。需保持既有 `test_timeout` 的契约（error 含 "timed out"）。

**验收**：连续两次 `run_code`，第二次能用第一次定义的变量。

### 2c —— 文件 / shell API

给沙箱加「文件读写」和「跑 shell 命令」的能力，对齐 E2B 的
`sandbox.files` / `sandbox.commands` 手感。

- `protocol.py` **向后兼容新增** `/files/{read,write,list}` 和 `/commands`，
  `/execute` 与既有 dataclass 一律不动。
- `client.py` 加 `sandbox.files.*` 和 `sandbox.commands.*` 两个命名空间。

**验收**：写文件→读回往返成功；`sandbox.commands.run("ls /tmp")` 拿到输出。

---

## 4. 协议演进原则（2c）

`protocol.py` 是全项目最重要的稳定边界。阶段 2 的协议演进**只增不改**：

- 既有 `ExecuteRequest` / `OutputEvent` / `Execution` 一个字段都不动。
- 新增的文件/shell 走**新端点 + 新 dataclass**，老 client 看不见也不受影响。
- 这样旧测试无需改动即可全绿——协议的向后兼容由测试兜底证明。

---

## 5. 关键决策：2b 用 Jupyter kernel（而非自建 worker）

持久解释器有三条路，本项目选 **Jupyter / IPython kernel**：

| 方案 | 取舍 |
|------|------|
| **Jupyter / IPython kernel ✅ 选定** | 最贴近 E2B（E2B 的 code interpreter 就是 Jupyter kernel）；富输出、kernel 重启开箱即用。代价：引入 `ipykernel`+`jupyter_client` 重依赖，kernel 内部是黑盒。 |
| 自建常驻 worker | 零依赖、亲手造迷你 kernel、逐行可读。代价：输出分帧/超时/崩溃恢复都得自己处理，代码更多。 |
| 进程内 `exec()` | 代码最少。代价：超时杀不掉线程、代码崩溃拖垮整个 daemon。最简但最脆，不用。 |

> 选 Jupyter 的理由：阶段 2 的学习目标明确写着「对齐 E2B 核心架构」，
> 用 E2B 同款的 Jupyter kernel，学完回头读 E2B 的 `envd` 源码会直接共鸣。
> 这是阶段 0/1「零依赖」之后**第一次按阶段需要引入依赖**，符合开发约定。

---

## 6. 网络与安全取舍（务必如实记录）

阶段 2 容器必须开一个**管理端口**给 client（不然连不上里面的 daemon），
所以**不能再 `--network none`**。后果：

- 容器内**代码的对外网络也跟着放开了**——这一点上**隔离反而弱于阶段 1**
  （阶段 1 每个执行容器是彻底断网的）。
- 「代码能出网但保留管理通道」是更细粒度的活（独立内网 / 防火墙规则），
  本项目留到阶段 3/4。

**安全红线（与 CLAUDE.md 一致）**：阶段 2 的隔离**仍不足以**运行完全不可信
代码，**严禁**对外提供服务或接收任意输入。内核级强隔离从阶段 3（microVM）起。
文档与代码注释都必须如实说明这一点。

仍然保留的隔离手段（容器级，作用于整个沙箱而非单次执行）：
`--memory` / `--cpus` / `--pids-limit` / `--read-only` + `--tmpfs /tmp`。

---

## 7. 兼容性：阶段 0/1 不删，阶段 2 是「新增拓扑」

阶段 2 不替换 `backend="local"/"docker"`，而是**新增**常驻容器拓扑并存。
好处：既有 18 个测试原样全绿（安全网不动），还能新旧对照学习。

`Sandbox(backend=...)` 取值演进为一条「完整策略」枚举：

| `backend` 取值 | 拓扑 | 容器内执行后端 | 对应阶段 |
|----------------|------|----------------|----------|
| `"local"` | 宿主 daemon | 本机子进程（无隔离） | 0 |
| `"docker"` | 宿主 daemon | 每次执行一个临时容器 | 1 |
| `"container"` | **常驻容器**，daemon 在内 | 容器内子进程（无状态） | **2a** |
| `"kernel"` | **常驻容器**，daemon 在内 | Jupyter kernel（有状态） | 2b（最终态） |

`"container"` 与 `"kernel"` 共用 client 端的「起常驻容器」代码，差别只在
告诉容器内 daemon 用哪个 `--backend`（`container`→`local`，`kernel`→`kernel`）。

---

## 8. 阶段 2 总验收标准

- [x] 2a：`backend="container"` 跑通；端到端用例三拓扑参数化全绿（7×3）。
- [x] 2b：连续两次 `run_code`，第二次能用第一次定义的变量（超时 interrupt 后 kernel 存活、旧变量仍可用）。
- [x] 2c：文件读写往返成功、文件对 `run_code` 可见、列目录、`commands.run` 拿到 shell 输出（含非零退出码）。
- [x] 全程 `pytest` 全绿（41 项）；文档如实写明阶段 2 的网络/安全弱化点与「仅 /tmp 可写」约束。

**阶段 2 已整体完成。** 文件/命令采用 daemon 级实现（不经 ExecutionBackend），对齐 E2B
envd 把文件/进程服务与代码 kernel 分开的设计；下一步进入阶段 3（Firecracker microVM）。
