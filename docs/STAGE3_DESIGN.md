# 阶段 3 设计：Firecracker microVM 隔离

> 本文是阶段 3 的设计与实现计划。阶段 3 是整个项目**隔离强度第一次发生质变**的一关——
> 从「与宿主共享内核的容器」升级到「拥有独立 guest 内核的 microVM」，逃逸面骤降。
> 建议配合 `docs/ARCHITECTURE.md` 与 `docs/STAGE2_DESIGN.md` 一起读：阶段 3 在架构上
> 与阶段 2 高度同构（又一次「主从关系反转」），唯一真正的新边界是**传输从 TCP 变成 vsock**。

ROADMAP 给阶段 3 写了一句话：**「本阶段慢下来手动理解，别全靠 vibe。」** 本文就是为了
让每一步都能被理解而非复制。

---

## 1. 这一关在解决什么：隔离强度的质变

阶段 1/2 的容器与宿主**共享同一个 Linux 内核**。容器的隔离全靠内核的 namespace +
cgroups——一旦内核本身有漏洞（容器逃逸 CVE 年年有），不可信代码就能捅穿到宿主。
所以 CLAUDE.md 的安全红线一直写着：**阶段 0/1/2 的隔离都不足以跑完全不可信代码。**

阶段 3 用 **Firecracker microVM** 换掉容器：

```
        阶段 2（容器）                         阶段 3（microVM）
┌─────────────────────────┐         ┌─────────────────────────────┐
│  沙箱代码                 │         │  沙箱代码                     │
│  ───────────────         │         │  ───────────────             │
│  容器 = namespace+cgroup │         │  guest 用户态                 │
│         ↓ 共享内核        │         │  ───────────────             │
│  宿主 Linux 内核 ◄── 逃逸 │         │  guest 独立 Linux 内核        │ ← 沙箱有自己的内核
│        面大               │         │  ───────────────             │
└─────────────────────────┘         │  KVM (/dev/kvm) 硬件虚拟化边界 │ ← 逃逸要先攻破 KVM
                                     │  ───────────────             │
                                     │  宿主 Linux 内核              │
                                     └─────────────────────────────┘
```

- 沙箱里跑的是**一个真正的、独立的 Linux 内核**，由 Firecracker 用 KVM 在硬件虚拟化
  边界内拉起。不可信代码要逃逸，得先攻破 guest 内核、再攻破 KVM——比攻破一个共享内核
  的 namespace 难一个数量级。这就是 E2B / AWS Lambda 选 Firecracker 的原因。
- Firecracker 是「microVM」：砍掉了传统 VM 的 BIOS、PCI、USB 等一大堆模拟设备，只留
  virtio-net / virtio-block / virtio-vsock 等极少数，所以**冷启动可达上百毫秒级**、
  内存开销极小。这正是它能做「每个请求一个 VM」的本钱。

> 安全红线仍然成立但语气可以变了：阶段 3 是本项目**第一次**拿到「强到可以认真讨论
> 跑不可信代码」的隔离。但这是**学习实现、未经安全审计**，文档仍须如实标注「不要直接
> 拿它对外接收任意输入」——真这么用得上 E2B/Fly.io 级别的纵深防御（seccomp-bpf、
> jailer、网络策略、限速、逃逸监控……），那些属于产品化，不在本阶段。

---

## 2. 与阶段 2 同构：又一次「主从关系反转」，只是 VM 取代容器

阶段 2 的灵魂是「daemon 从宿主搬进**常驻容器**，创建隔离环境的职责从 backend 上移到
client」。**阶段 3 把这句话里的「容器」换成「microVM」，几乎原样复用。**

```
宿主机                              沙箱 microVM（长期存活，一个 Sandbox 一个）
┌──────────────────┐               ┌──────────────────────────────────┐
│ client           │   HTTP/SSE    │  daemon (server.py) ← 还是 envd     │
│  启动 firecracker │   over vsock  │     │  --transport vsock           │
│  连 vsock UDS     │ ────────────► │     ▼                              │
│  health 探测      │ ◄──────────── │  JupyterKernelBackend（有状态）     │
│  kill firecracker │               │     └── 变量跨 run_code 留存         │
└──────────────────┘               └──────────────────────────────────┘
   ▲ guest 独立内核 + KVM 边界就在这条竖线上
```

对照阶段 2 的职责迁移表（`STAGE2_DESIGN.md` §2），阶段 3 改的还是同样三个地方，
**`server.py` 的业务逻辑和 `protocol.py` 的线缆字节仍然一行不改**：

| 代码位置 | 阶段 2 现在做什么 | 阶段 3 改成什么 |
|----------|------------------|----------------|
| `client.py:_spawn_resident_container` | `docker run -d` 起常驻容器 | 新增 `_spawn_microvm`：起一个 Firecracker microVM（见 §4.4） |
| `client.py:_wait_until_healthy` | 轮询 `http://127.0.0.1:port/health` | 轮询 `/health`，但走 **vsock 传输**（见 §4.1） |
| `client.py:close` | `docker rm -f <容器名>` | kill firecracker 进程 + 清理工作目录 |
| `client.py` 传输层 | 全程 `urllib` over TCP | **新增 `Transport` 抽象**：TCP 路径不变，新增 vsock 路径（见 §4.1、§5） |
| `server.py:serve` | `asyncio.start_server(host, port)`（TCP） | 新增 `--transport vsock`：用 `AF_VSOCK` 监听 socket（见 §4.1） |
| `protocol.py` | `/execute` `/files/*` `/commands` | **字节不变**——HTTP/SSE 的内容原样跑在 vsock 上 |
| rootfs / 内核 | 用 docker 镜像（`microsandbox-agent`） | 把 agent 镜像 **导出成 ext4 rootfs** + 配一个 guest 内核（见 §4.2/4.3） |

**关键观察**：阶段 3 的新东西只有两类——①**传输换成 vsock**（动 client 的传输层 +
server 的监听方式，但协议字节不变）；②**把「容器镜像」换成「内核 + rootfs」这套 VM
要的素材**。daemon 和 backend 的执行逻辑、protocol 的契约，全都复用阶段 2 的成果。

---

## 3. 拆成三小步（每步都保持既有 42 个测试全绿）

阶段 3 比阶段 2 还重（多了 VM 素材构建 + 特权环境），更要拆小、慢走。

### 3a —— vsock 传输抽象（不需要 VM，纯重构 + 新增传输实现）← 第一步，最安全

**目标**：把 client 与 server 里「写死 TCP」的地方抽出一层 `Transport`，让协议字节与
传输方式解耦。这一步**完全不碰 Firecracker、不需要 /dev/kvm**，因此能在当前环境直接
跑、且保证既有 42 个测试一字不动地全绿。

- `client.py`：引入 `Transport` 抽象，两个实现：
  - `TcpTransport`——把现在的 `urllib`/`_DIRECT_OPENER` 逻辑原样包进去，
    **行为字节级不变**（local/docker/container/kernel 四个拓扑的测试因此照旧全绿）。
  - `VsockTransport`——连 Firecracker 的 vsock UDS、做 `CONNECT <port>` 握手、
    然后在裸 socket 上手写极小 HTTP/1.1 客户端（见 §4.1）。本步只写好、先不接 VM。
  - `_stream` / `_post_json` / `_wait_until_healthy` 改为经 `transport` 收发。
- `server.py`：`serve()` 增加 `--transport {tcp,vsock}`。`tcp` 走现在的
  `asyncio.start_server(host, port)`；`vsock` 用 `socket.AF_VSOCK` 建监听 socket 再
  `asyncio.start_server(sock=...)`。**handle/分发/backend 全不动。**

**验收**：既有 42 个测试全绿（证明 TCP 路径行为没变）；新增针对 `VsockTransport`
HTTP 帧编解码的**纯单元测试**（不依赖 VM，喂字节验证 request 拼装 / response+SSE 解析）。

> 为什么先做这步：它把阶段 3 风险最高的「改动最神圣的 client/protocol 边界」与「跑通
> Firecracker」两件事**解耦**。3a 是一次有测试兜底的安全重构，做完再去碰 VM，心里有底。
> 这正是阶段 2「2a 先证明搬家不改代码」的同款打法。

### 3b —— 构建 microVM 素材 + 启动 Firecracker，端到端 run_code

**目标**：真正在一个 Firecracker microVM 里把 daemon 跑起来，client 经 vsock 连进去
`run_code` 拿回结果。

- **素材构建**（`scripts/build-rootfs.sh`，见 §4.2/4.3）：
  1. 内核：下载一个 Firecracker 兼容的 `vmlinux`（含 virtio-vsock 驱动）。
  2. rootfs：`docker export` agent 镜像的文件系统 → 注入我们的 `src/microsandbox` 和
     一个极小 `init` → 用 `mkfs.ext4 -d` 打包成 ext4 镜像（**免 root**，见 §6）。
- `client.py` 新增 `backend="microvm"`：起 firecracker（`--config-file`）、轮询 vsock
  `/health`、`close` 时 kill 进程 + 清工作目录（对照 `_spawn_resident_container`）。
- 容器内 daemon 用 `--transport vsock`。**先用 `--backend local`（无状态子进程）跑通**
  最短路径，确认 VM+vsock+daemon 链路 OK，再切 `--backend kernel`（有状态，agent 镜像
  里已有 ipykernel）。
- **此步需要你先完成一次性特权设置**（加 kvm 组 + 下载 firecracker/vmlinux，见 §7）。

**验收**：`Sandbox(backend="microvm").run_code("print(1+1)")` 从一个**真 VM**里拿回
`2`；切到 kernel 后端后变量跨 `run_code` 留存；`close` 后无残留 firecracker 进程。

### 3c —— 冷启动测量、资源限制，（拉伸）快照/预热

- **冷启动测量**（ROADMAP 的明确验收项）：记录从 `firecracker` 起进程到 `/health`
  就绪的耗时，写进文档；与阶段 1/2 的容器启动做个对照。
- **资源限制**走 Firecracker `machine-config`（`vcpu_count` / `mem_size_mib`），对照
  阶段 2 的 `--memory/--cpus`，理解「VM 配额」与「cgroup 配额」的差别。
- **网络**：MVP 用 vsock 做控制通道，**guest 完全不配网卡**——比阶段 2「必须开管理
  端口、连带放开出网」更干净（见 §5 的安全反超）。需要出网是后续/阶段 4 的事。
- **（拉伸，可滑到阶段 4）快照 / 恢复**：Firecracker 的 snapshot 能把「已启动到就绪」
  的 VM 状态存盘，恢复时跳过内核引导，做到**毫秒级冷启动**；再叠一个预热池。这部分与
  阶段 4「沙箱池」重叠，按兴趣选做。

---

## 4. 关键技术点详解（带真实代码锚点）

### 4.1 vsock：宿主与 VM 怎么说话

vsock（virtio-vsock）是为「宿主↔VM」设计的 socket，地址是 `(CID, port)` 而非 `(IP,
port)`：guest 的 CID 我们设成 3，宿主固定是 2。Firecracker 把 vsock **多路复用到宿主
的一个 Unix domain socket（UDS）**上，协议是文本握手：

- **宿主 → guest（我们 client 要的方向）**：client 连宿主上的 UDS（如
  `/tmp/microsandbox-vm-xxxx/fc.vsock`），发一行 `CONNECT <port>\n`（如 `CONNECT 1024`），
  Firecracker 回 `OK <hostport>\n`，之后这条字节流就接到了 guest 里**监听 `AF_VSOCK`
  端口 1024** 的进程——也就是我们的 daemon。握手之后，**双方说的就是原来那套 HTTP/SSE**。
- **guest → 宿主**：guest 连 `(CID=2, port=N)`，Firecracker 去连宿主 UDS `{uds}_{N}`。
  MVP **用不到这个方向**（daemon 是 server，永远 client 先发起），所以不实现，更简单。

guest 内（`server.py`）监听 vsock，标准库就够：

```python
import socket, asyncio
s = socket.socket(socket.AF_VSOCK, socket.SOCK_STREAM)
s.bind((socket.VMADDR_CID_ANY, 1024))   # 监听本 VM 的 1024 端口
s.listen()
server = await asyncio.start_server(self.handle, sock=s)   # handle 一行不改
```

宿主侧（`client.py` 的 `VsockTransport`）因为 `urllib` 不会说这套握手，要手写一个**极
小 HTTP/1.1 客户端**（我们自己既是 client 又是 server，协议简单，几十行够）：

```python
sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
sock.connect(uds_path)
sock.sendall(b"CONNECT 1024\n")
# 读 "OK <port>\n"，确认握手成功
# 然后把 "POST /execute HTTP/1.1\r\n...\r\n\r\n<body>" 写进去，
# 再按 Content-Length / text/event-stream 流式读回——逐行 yield，复用既有 SSE 解析。
```

> 顺带的红利：3a 把 HTTP 帧收发抽出来后，TCP 路径其实也能脱离 `urllib`。但为了让既有
> 42 个测试**零行为变化**地全绿，3a 里 `TcpTransport` **仍包着现在的 `urllib`**——
> 统一成裸 socket 是可选的后续清理，不在阶段 3 的关键路径上。

### 4.2 rootfs：从 docker 镜像导出，免 root 打包

Firecracker 要一个 **ext4 磁盘镜像**当根文件系统。我们不从零做，而是复用阶段 2 已经
build 好的 `microsandbox-agent` 镜像（里面有 Python + ipykernel + jupyter_client）：

```
docker create microsandbox-agent           # 造一个不启动的容器，拿它的 rootfs
docker export <id> | tar -x -C rootfs/      # 导出整个文件系统树到 rootfs/ 目录
cp -r src/microsandbox rootfs/opt/.../       # 注入我们的包（阶段 2 是运行时挂载，VM 里得放进去）
install init  rootfs/init                    # 放一个极小 init（见 §4.3）
mkfs.ext4 -d rootfs/ microsandbox.ext4 512M  # ★ 用目录直接打包成 ext4，全程不挂载、免 root
```

`mkfs.ext4 -d <dir>` 是关键：它把一个目录树**直接写进新 ext4 镜像**，不需要 `mount`，
因此**整条 rootfs 构建链路无需 root**（`docker`/`tar`/`mkfs.ext4 -d` 当前用户都能跑）。
镜像里文件属主是构建用户（uid 1000），但 guest 内 daemon 以 root 跑，root 能读一切，
无碍。

### 4.3 init：guest 里的 PID 1

内核引导完会执行 `init`（PID 1）。我们放一个**极小的 shell init**，挂好伪文件系统就
`exec` 我们的 daemon（`exec` 让 daemon 接管 PID 1，省一层进程）：

```sh
#!/bin/sh
mount -t proc     proc /proc
mount -t sysfs    sys  /sys
mount -t devtmpfs dev  /dev      2>/dev/null   # 可能内核已自动挂
mount -t tmpfs    tmp  /tmp                    # 唯一可写区，对齐阶段 2 的 --tmpfs /tmp
export HOME=/tmp PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1
exec python3 -m microsandbox.server --transport vsock --vsock-port 1024 --backend kernel
```

内核引导参数（kernel boot args）里用 `init=/init` 指到它；root 设备是
`/dev/vda`（我们挂的那块 ext4），`ro` 只读根（写都去 tmpfs /tmp，和阶段 2 一致）。

### 4.4 启动 Firecracker：声明式 config-file

起 VM 有两种方式：REST API（先起进程再一条条 PUT 配置）和 `--config-file`（一个 JSON
声明所有东西）。**学习期选 `--config-file`**，因为一个文件就能读懂整台 VM 长什么样：

```json
{
  "boot-source":   { "kernel_image_path": "vmlinux",
                     "boot_args": "console=ttyS0 reboot=k panic=1 init=/init ro" },
  "drives":        [{ "drive_id": "rootfs", "path_on_host": "microsandbox.ext4",
                      "is_root_device": true, "is_read_only": true }],
  "machine-config":{ "vcpu_count": 1, "mem_size_mib": 256 },
  "vsock":         { "guest_cid": 3, "uds_path": "fc.vsock" }
}
```

`client._spawn_microvm` 干的事，和阶段 2 的 `_spawn_resident_container` 一一对应：
建一个 per-VM 工作目录 → 写好上面的 config（uds/路径都在该目录）→
`subprocess.Popen(["firecracker", "--config-file", cfg, "--api-sock", api])` →
`_wait_until_healthy`（走 vsock）→ `close` 时 `proc.terminate()`/`kill()` + 删工作目录。

---

## 5. 协议/传输演进原则 & 安全反超

**协议（protocol.py）字节不变**，这条主线在阶段 3 依旧成立——我们只是把同一串 HTTP/SSE
字节从「TCP 连接」搬到「vsock UDS 连接」上。client 因为传输方式真的变了，**第一次需要
动**（引入 `Transport` 抽象），但变更被关在传输层，`run_code` 等上层 API 对用户不变；
`server.py` 的请求处理逻辑、`backend.py` 的执行逻辑全不动。

**安全：阶段 3 第一次出现「隔离反超」而非「为了管理通道而弱化」。**
阶段 2 为了让 client 连上容器内 daemon，**必须开一个 TCP 管理端口**，连带把 guest 出网
也放开了（`STAGE2_DESIGN.md` §6 如实记了这个倒退）。阶段 3 用 vsock 做控制通道，**根本
不需要给 VM 配网卡**——管理通道走 virtio-vsock，与「有没有网络」正交。于是可以：

- guest **完全不配 virtio-net** → 沙箱代码彻底无网络（回到阶段 1 的断网强度），
  同时**保留**管理通道。阶段 2 做不到的「既能管又断网」，阶段 3 因 vsock 自然达成。
- 叠加 guest 独立内核 → 这是项目至此最强的隔离组合。

---

## 6. 本机（WSL2）可行性与一次性特权设置

环境探测结论（2026-06，本机）：

| 检查项 | 结果 | 影响 |
|--------|------|------|
| `/dev/kvm` | **存在**（`crw-rw---- root kvm`） | WSL2 嵌套虚拟化已开，Firecracker 有戏 |
| 架构 | x86_64 | 下载 x86_64 的 firecracker / vmlinux |
| 当前用户开 `/dev/kvm` | **被拒**（不在 `kvm` 组） | 需加组或 sudo——见下方一次性设置 |
| 免密 sudo | **不可用** | 我（Claude）无法非交互 sudo，特权步骤须你亲自跑 |
| `mkfs.ext4` / `docker` / `curl` | 都在 | rootfs 构建链路齐备，且可**免 root**（`mkfs.ext4 -d`） |
| 磁盘 / 内存 | 919G / 14G 可用 | 充裕 |

**一次性特权设置（请你在终端用 `! <cmd>` 跑，或自己的 shell 里跑）**：

```bash
# 1) 把自己加进 kvm 组，让 firecracker 免 sudo 能开 /dev/kvm（一次性）
sudo usermod -aG kvm $USER
# 2) 重启 WSL 让组生效：在 Windows PowerShell 里 `wsl --shutdown`，再重开终端
#    （重开后 `id` 里应能看到 kvm 组，python 直接 open(/dev/kvm) 不再 PermissionError）
```

下载素材（**无需 sudo**，我可以代跑，但放这便于你核对来源）：

```bash
# firecracker 静态二进制（GitHub release）
# vmlinux：Firecracker CI 提供的、含 virtio-vsock 的内核镜像
# 具体版本号在 3b 落地时确定并写进 scripts/，避免本文档里的 URL 过期
```

**过了 kvm 组这一关之后，阶段 3 后续我能全程自动化**：rootfs 走 `mkfs.ext4 -d` 免 root，
firecracker 加组后免 sudo，测试也能在本机真跑。仍待 3b 启动时**实测验证**的两件事：
①WSL2 的嵌套 KVM 能被 Firecracker 正常使用；②下载的 vmlinux 里 virtio-vsock 驱动可用。
这俩是「极可能成、但必须开机见真章」的点，本文不打包票。

---

## 7. 兼容性：阶段 3 是「新增拓扑」，不动旧的

和阶段 2 一样，阶段 3 **新增** `backend="microvm"`，与 `local/docker/container/kernel`
并存，旧的一个都不删——既有 42 个测试是安全网，必须原样全绿。

`tests/conftest.py` 加一个 `requires_firecracker` skip 守卫（检查 firecracker 二进制 +
`/dev/kvm` 可读 + 素材就绪），缺任一就像 docker 不可用时那样**整组 skip**——这样别的
机器 / CI 上 `pytest` 仍全绿，本机备齐后才真跑 microVM 用例。`backend` 取值表延伸为：

| `backend` | 拓扑 | guest/容器内执行后端 | 隔离强度 | 阶段 |
|-----------|------|----------------------|----------|------|
| `local` | 宿主 daemon | 子进程（无隔离） | 无 | 0 |
| `docker` | 宿主 daemon | 每次一个临时容器 | 共享内核 | 1 |
| `container` | 常驻容器 | 容器内子进程（无状态） | 共享内核 | 2a |
| `kernel` | 常驻容器 | Jupyter kernel（有状态） | 共享内核 | 2b |
| **`microvm`** | **常驻 microVM** | **VM 内 Jupyter kernel（有状态）** | **独立内核+KVM** | **3** |

---

## 8. 阶段 3 验收标准

- [x] **3a**：传输抽象落地；既有 42 个测试零改动全绿（+3 个 vsock 单测 = 45）；
      `VsockTransport` 的 CONNECT 握手 + HTTP 帧编解码 + SSE 解析有纯单元测试覆盖（不依赖 VM）。
- [x] **3b**：`backend="microvm"` 端到端跑通——从真 Firecracker microVM 里 `run_code`
      拿回结果；VM 内 kernel 后端变量跨 `run_code` 留存；`close` 后无残留进程/工作目录。
      （见 §9 实测记录；`tests/test_microvm.py` 4 项，缺素材自动 skip。）
- [x] **3c**：冷启动 ~0.94s 已记录；资源限制经 `machine-config` 生效（`test_microvm` 已验）；
      guest 不配网卡仍可管理（隔离反超已落实）；**拉伸已做**：快照恢复毫秒级冷启动（见 §9）。
      预热池（一份快照 fork 出 N 台）→ 阶段 4。
- [ ] 全程 `pytest` 全绿（本机真跑 microVM 用例，无 firecracker 的环境自动 skip）。

**与阶段 2 一脉相承的纪律**：新增 backend/传输实现，protocol 字节不变，改动尽量挡在
client 传输层之外；`tests/` 全绿是跨阶段重构的安全网。

---

## 9. 3b 实测记录（本机 WSL2，2026-06）

跑通了，记录关键事实与踩到的坑（对学习最有价值的部分）：

- **素材**：firecracker v1.16.0 静态二进制 + Firecracker CI 的 vmlinux 6.1.155（vsock /
  virtio-blk / ext4 / devtmpfs 全为内建 `=y`，下载前先用 `.config` 验明）。rootfs 由
  `scripts/build-rootfs.sh` 从 agent 镜像 `docker export` + `mkfs.ext4 -d` 免 root 打包（~250MB）。
- **冷启动**：`Sandbox(backend="microvm")` 构造到 daemon 就绪稳定 **~0.94s**（含 firecracker
  起进程 + 内核引导 + python 启动 + daemon 监听 vsock）。多了一整个 guest 内核仍是亚秒级——
  microVM「砍掉传统 VM 的 BIOS/PCI/USB、只留 virtio」的本钱。（快照做到毫秒级冷启动留给 3c。）
- **坑 1 · `sys.executable` 为空**：daemon 作为 PID 1 启动、init 没设 `PATH` 时，Python 算不出
  自己的可执行路径，`local` 后端 `create_subprocess_exec("")` 报 PermissionError。修法：init 里
  显式 `export PATH=...` 并用绝对路径 exec python（见 `scripts/build-rootfs.sh` 的 /init）。
- **坑 2 · loopback 默认 down**：kernel 后端的 Jupyter kernel 走 ZMQ over 127.0.0.1，而 microVM
  的 `lo` 默认 down，导致「kernel 启动 60s 超时」。极简 rootfs 无 ip/ifconfig，故在 daemon 的
  vsock 启动路径里用 `SIOCSIFFLAGS` ioctl 拉起 `lo`（见 `server._ensure_loopback_up`）。
- **隔离反超已落实**：guest 只配了 vsock、**没有 virtio-net**——`/sys/class/net` 里只有 `lo`，
  连 1.1.1.1 直接 OSError（无外网）。即「管理通道走 vsock + 沙箱代码彻底断网」同时成立，
  正是阶段 2「为开管理端口被迫放开出网」做不到的（§5）。

### 快照恢复（3c 拉伸，已实现）

Firecracker 的看家本领：把「已启动并预热」的 VM 状态存盘，恢复时跳过内核引导，毫秒级就绪。

- **怎么建**（`scripts/build-snapshot.sh`）：boot 一台 base VM → 预热 Jupyter kernel（跑一段
  `pass` 强制起 kernel）→ `PATCH /vm {Paused}` → `PUT /snapshot/create {Full}`。产物两份：
  `vendor/snapshot/vmstate`（~13KB 设备/CPU 状态）+ `memfile`（512MB guest 内存，**含热 kernel**）。
- **怎么恢复**（`Sandbox(backend="microvm", from_snapshot=True)` → `_restore_microvm`）：起一台
  空 firecracker → 经 REST API `PUT /snapshot/load` + `resume_vm` 把状态灌回。快照 load/create
  只能走 API（`--config-file` 表达不了），client 用 `_firecracker_api`（HTTP over AF_UNIX）驱动。
- **实测对照**（本机）：

  | 路径 | 就绪 | 首次 run_code | 到首个结果 |
  |------|------|---------------|-----------|
  | 冷启动（`_spawn_microvm`） | ~0.94s | ~0.8s（含 kernel 冷启动） | ~1.77s |
  | 快照恢复（`_restore_microvm`） | **~0.03–0.04s** | ~0.13s（kernel 已热） | **~0.17s** |

  到首个结果约 **10× 加速**；就绪本身约 30×。
- **磁盘**：快照只存内存 + 设备状态，**磁盘内容仍由宿主 `rootfs.ext4` 提供**——恢复时它必须
  还在原路径（恢复路径里 `_check_microvm_available` 仍校验 rootfs 正因如此）。
- **已知限制（单实例）**：快照里 vsock 的 uds 路径是固定的，同一时刻只能恢复一台。**并发恢复
  + 预热池**（一份基准快照 fork 出 N 台秒级沙箱）需 per-VM 的 uds override，留到阶段 4。
</content>
</invoke>
