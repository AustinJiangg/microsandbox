#!/usr/bin/env bash
# 阶段 3b：把 agent 镜像导出成 Firecracker microVM 用的 ext4 rootfs（全程免 root）。
#
# 思路（见 docs/STAGE3_DESIGN.md §4.2/4.3）：
#   1. docker export 复用阶段 2 已 build 的 microsandbox-agent 镜像的整个文件系统
#      （里面有 Python + ipykernel + jupyter_client）；
#   2. 注入我们的 src/（阶段 2 是运行时挂载，VM 里没有挂载这回事，得放进 rootfs）；
#   3. 写一个极小 /init 当 PID 1：挂好伪文件系统后 exec 我们的 daemon（--transport vsock）；
#   4. 用 `mkfs.ext4 -d <目录>` 直接把目录打包成 ext4 镜像——不需要 mount、因此免 root。
#
# 为什么免 root 很关键：本机当前用户能用 docker / mkfs.ext4，但 sudo 要密码。`mkfs.ext4 -d`
# 让整条链路无需特权（镜像里文件属主是构建用户，但 guest 内 daemon 以 root 跑，root 能读
# 一切，无碍）。
#
# 用法：scripts/build-rootfs.sh [镜像名] [输出路径] [额外余量MB]
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
IMAGE="${1:-microsandbox-agent:latest}"
OUT="${2:-$REPO_ROOT/vendor/rootfs.ext4}"
MARGIN_MB="${3:-300}"   # 在导出内容实际大小之上预留的可写余量

command -v mkfs.ext4 >/dev/null || { echo "缺 mkfs.ext4（apt install e2fsprogs）" >&2; exit 1; }
docker image inspect "$IMAGE" >/dev/null 2>&1 || {
  echo "缺镜像 $IMAGE，请先 docker build -t microsandbox-agent ." >&2; exit 1; }

STAGING="$(mktemp -d)"
trap 'rm -rf "$STAGING"' EXIT

echo "[build-rootfs] 导出 $IMAGE 的文件系统 ..."
cid="$(docker create "$IMAGE")"
# 排除 /dev 下的设备节点：非 root 无法 mknod，且内核 DEVTMPFS_MOUNT=y 会自动重建 /dev。
docker export "$cid" | tar -x -C "$STAGING" --exclude='dev/*' \
  || echo "[build-rootfs] tar 有非致命告警（多为特殊文件，已忽略）"
docker rm "$cid" >/dev/null

# 起码得有 python3，否则导出明显出问题，早失败早排查。
test -x "$STAGING/usr/local/bin/python3" || { echo "导出的 rootfs 里没有 python3，中止" >&2; exit 1; }

echo "[build-rootfs] 注入 microsandbox 源码到 /opt/microsandbox/src ..."
mkdir -p "$STAGING/opt/microsandbox"
cp -r "$REPO_ROOT/src" "$STAGING/opt/microsandbox/src"

echo "[build-rootfs] 写入 /init（PID 1）..."
cat > "$STAGING/init" <<'INIT'
#!/bin/sh
# microVM 的 PID 1：挂好伪文件系统后 exec daemon（exec 让 daemon 直接接管 PID 1）。
# 失败时（如 python import 出错）PID 1 退出 → 内核 panic=1 → Firecracker 进程随之退出，
# 宿主侧 health 检查能立刻察觉，console.log 里也留有 traceback——这是有意的诊断路径。
mount -t proc     proc /proc 2>/dev/null
mount -t sysfs    sys  /sys  2>/dev/null
mount -t devtmpfs dev  /dev  2>/dev/null   # 内核多半已自动挂（DEVTMPFS_MOUNT=y），失败无妨
mount -t tmpfs    tmp  /tmp  2>/dev/null    # 唯一可写区（根是只读 rootfs），对齐阶段 2 的 /tmp

# 执行后端从内核 cmdline 取（client 经 boot_args 传 MSBACKEND=...）；默认 kernel（有状态）。
backend=kernel
for tok in $(cat /proc/cmdline 2>/dev/null); do
  case "$tok" in MSBACKEND=*) backend="${tok#MSBACKEND=}" ;; esac
done

# PATH 必须显式设：minimal init 下 PATH 可能为空，sh 找不到命令、且 python 算不出
# sys.executable（local 后端要用它 spawn 子进程；commands.run 的 shell 也要 PATH）。
export PATH=/usr/local/bin:/usr/bin:/bin
export HOME=/tmp PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 PYTHONPATH=/opt/microsandbox/src
echo "[init] microsandbox daemon: transport=vsock port=1024 backend=$backend"
# 用绝对路径 exec：确保 python 的 sys.executable 是真实路径而非空串（否则 local 后端
# create_subprocess_exec("") 会 PermissionError）。
PY="$(command -v python3)"
exec "$PY" -m microsandbox.server --transport vsock --vsock-port 1024 --backend "$backend"
INIT
chmod +x "$STAGING/init"

size_mb=$(( $(du -sm "$STAGING" | cut -f1) + MARGIN_MB ))
echo "[build-rootfs] 打包 ext4（mkfs.ext4 -d，免挂载/免 root），大小 ${size_mb}MB ..."
rm -f "$OUT"
mkdir -p "$(dirname "$OUT")"
mkfs.ext4 -q -L rootfs -d "$STAGING" "$OUT" "${size_mb}m"

echo "[build-rootfs] 完成：$OUT ($(du -h "$OUT" | cut -f1))"
