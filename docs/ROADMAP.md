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

## ⬜ 阶段 2：容器内常驻 agent + 有状态 REPL

**学习目标**：对齐 E2B 核心架构 —— 沙箱内常驻一个 agent（envd），支持跨调用保留状态。

要做的事：
1. 把 `server.py` 打包进容器镜像，作为容器入口常驻运行（而非每次 `docker exec`）。
2. backend 改用**持久解释器**（如内嵌 IPython/Jupyter kernel），让变量在多次
   `run_code` 之间保留 —— 实现真正的 REPL 体验。
3. 新增文件系统 API（读/写/列目录）和 shell 执行 API。
4. client 通过容器网络地址连 agent；增加健康探测与重连。

**完成标准**：连续两次 `run_code`，第二次能用第一次定义的变量；文件 API 往返成功。

---

## ⬜ 阶段 3：Firecracker microVM 隔离

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
