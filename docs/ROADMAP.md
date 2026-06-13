# 路线图（ROADMAP）

每个阶段都列出：**学习目标**、**要做的事**、**完成标准**。
建议每完成一项就在这里勾选，并同步更新 `CLAUDE.md` 顶部的「当前在这里」。

---

## ✅ 阶段 0：进程级骨架（已完成）

**学习目标**：理解 client↔daemon 的 RPC / 流式通信模型，把三层骨架立起来。

- [x] 定义协议（ExecuteRequest / OutputEvent / Execution）
- [x] LocalSubprocessBackend：子进程执行 + 超时 + stdout/stderr 分离
- [x] daemon：HTTP + SSE 流式
- [x] client SDK：run_code + 流式回调 + 自动起停 daemon
- [x] 测试与示例

**完成标准**：`python examples/quickstart.py` 跑通；`pytest` 全绿。

---

## ✅ 阶段 1：Docker 容器隔离（已完成）

**学习目标**：第一次真正的隔离。理解文件系统/网络隔离、cgroups 资源限制、容器生命周期。

- [x] 新增 `DockerBackend(ExecutionBackend)`：直接用 `docker` CLI + asyncio 子进程
      （不引入 docker-py——保持零运行时依赖，且 docker-py 是同步库，进 asyncio daemon 反而绕）。
- [x] 每次执行对应一个一次性容器（per-Sandbox 容器复用属于阶段 2 的有状态化）：
   - 镜像：官方 `python:3.12-slim`，执行路径 `--pull never`，需先手动 `docker pull`。
   - 限制：`--memory`/`--memory-swap`、`--cpus`、`--network none`、`--pids-limit`、
     `--read-only` + `--tmpfs /tmp`（只读根 + 临时可写层）。
   - 执行：代码经 argv 传入（`python -u -c <code>`，与阶段 0 同构；argv 上限 ~2MB 阶段 1 够用）。
- [x] daemon 加 `--backend {local,docker}` 开关 + 启动期 Docker 可用性检查；
      client 加 `Sandbox(backend=...)` 透传。**daemon 本阶段仍留在宿主机**——
      「daemon 搬进容器」是阶段 2 的事（对应 E2B envd）；本阶段验证的正是
      「换隔离 = 换 backend，client 与 protocol 不动」。
- [x] 超时与清理：超时 `docker rm -f` 杀容器（杀 docker run 客户端进程杀不死容器！），
      finally 幂等兜底；容器统一命名 `microsandbox-exec-*` 便于兜底清理。

**完成标准（已达成）**：原有 7 个测试在 local/docker 双后端参数化下全绿（7×2）；
新增 4 个隔离测试：宿主文件不可见、断网、只读根 + /tmp 可写、超时后无残留容器。

**注意**：容器隔离仍不足以跑完全不可信代码（容器逃逸面较大），文档需如实说明。

---

## ✅ 阶段 2：容器内常驻 agent + 有状态 REPL（已完成）

**学习目标**：对齐 E2B 核心架构 —— 沙箱内常驻一个 agent（envd），支持跨调用保留状态。

**核心是一次「主从关系反转」**：daemon 从宿主搬进**长期存活**的容器里常驻，
「创建隔离环境」的职责从 backend 上移到 client。详细设计见 `docs/STAGE2_DESIGN.md`。

拆成三小步，每步都保持既有测试全绿：

- [x] **2a envd 化（搬家，状态先不留存）**
   - 开发期不建镜像：把 `src/` 只读挂载进 `python:3.12-slim`，
     `python -m microsandbox.server` 跑在容器里。
   - `client` 新增 `backend="container"`：`docker run -d` 起常驻容器、映射端口、
     健康探测、`close` 时 `docker rm -f`。容器内 daemon 仍用无状态的
     `LocalSubprocessBackend`。**`server.py` 一行不改**（已支持 `--host/--port/--backend`）。
   - 验收达成：端到端用例在 `local`/`docker`/`container` 三拓扑下参数化全绿（7×3）。
- [x] **2b 有状态 REPL** —— 持久解释器用 **Jupyter / IPython kernel**（对齐 E2B）。
   - 新增 `JupyterKernelBackend`：daemon 托管常驻 kernel，用 ZMQ 走 Jupyter 消息协议，
     把 iopub 的 stream/execute_result/error/idle 翻译回 `OutputEvent`，`/execute` 协议不变。
   - **首次引入运行时依赖** `ipykernel` + `jupyter_client`（`[kernel]` 可选 extra，
     backend 内懒导入）；新增 `Dockerfile` 构建 agent 镜像；`client` 新增 `backend="kernel"`。
   - 超时用 **interrupt（SIGINT）而非杀进程**：打断当前 cell 但 kernel 与命名空间存活。
   - 验收达成：变量/函数/import 跨 `run_code` 留存；超时后 kernel 不死、旧变量仍可用。
- [x] **2c 文件 / shell API** —— `protocol.py` **向后兼容新增** `/files/{read,write,list}`、
   `/commands`（`/execute` 与既有 dataclass 不动）；client 加 `sandbox.files.*` /
   `sandbox.commands.*`，对齐 E2B 手感。
   - 关键设计：文件/命令由 **daemon 直接在自身 FS 上完成、不经 ExecutionBackend**——
     对齐 E2B envd（文件/进程服务与跑代码的 kernel 是分开的）。
   - 验收达成：写读往返、文件对 `run_code` 可见、列目录、`commands.run` 拿到 shell 输出
     （含非零退出码）。常驻容器 `--read-only` 根，写仅限 `/tmp`，写别处如实报错。

**注意（网络/安全弱化）**：阶段 2 容器要开管理端口给 client，故**不能再 `--network none`**，
容器内代码的对外网络随之放开——这一点**隔离反而弱于阶段 1**。强隔离仍要等阶段 3。

**完成标准**：连续两次 `run_code`，第二次能用第一次定义的变量；文件 API 往返成功；全程 `pytest` 全绿。

---

## ⬜ 阶段 3：Firecracker microVM 隔离 ← 当前在这里（下一步）

**学习目标**：理解强隔离原理、microVM、vsock 通信、快照实现毫秒级冷启动。**本阶段慢下来手动理解，别全靠 vibe。**

要做的事：
1. 准备 kernel 镜像 + rootfs（可从 Docker 镜像导出 rootfs）。
2. 用 Firecracker REST API 启动 microVM；把阶段 2 的 agent 放进 rootfs。
3. 宿主↔VM 用 vsock 通信，daemon 监听 vsock 而非 TCP。
4. 资源限制走 Firecracker 配置（vCPU、内存）。
5. 进阶：快照/恢复，实现快速冷启动；预热池。

**完成标准**：能在 microVM 里跑代码并拿回结果；测量并记录冷启动时间。

---

## ⬜ 阶段 4：产品化外围（按兴趣选做）

**学习目标**：把「一个沙箱」做成「沙箱服务」。

候选项：
- [ ] 控制面 API：`POST /sandboxes` 创建、`DELETE` 销毁、列表。
- [ ] 沙箱池预热，降低冷启动。
- [ ] 自定义模板/镜像（预装依赖）。
- [ ] 鉴权与配额、超时回收、用量统计。
- [ ] 多语言 backend（node、bash）。

---

## 对照学习建议

完成阶段 2 后，回头读 E2B 源码的 `envd` 与编排部分，会非常有共鸣。
跳过 SDK 多语言绑定、dashboard、billing —— 那些是产品外围，不是核心机制。
