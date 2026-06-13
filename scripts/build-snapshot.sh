#!/usr/bin/env bash
# 阶段 3c：生成「热快照」——boot 一台 microVM、预热 Jupyter kernel、暂停、Full snapshot。
#
# 产物 vendor/snapshot/{vmstate,memfile}，供 Sandbox(backend="microvm", from_snapshot=True)
# 毫秒级恢复：跳过内核引导 + Jupyter kernel 冷启动（见 docs/STAGE3_DESIGN.md §9）。
#
# 快照只存「内存 + 设备/CPU 状态」，磁盘内容仍由宿主的 rootfs.ext4 提供——所以恢复时
# rootfs.ext4 必须还在原路径。vsock 的 uds 固定为 vendor/snapshot/fc.vsock（恢复时重建）。
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VENDOR="$REPO_ROOT/vendor"
SNAP="$VENDOR/snapshot"
FC="$VENDOR/firecracker"; KERNEL="$VENDOR/vmlinux"; ROOTFS="$VENDOR/rootfs.ext4"
UDS="$SNAP/fc.vsock"

command -v curl >/dev/null || { echo "需要 curl 驱动 Firecracker API" >&2; exit 1; }
for f in "$FC" "$KERNEL" "$ROOTFS"; do
  [ -e "$f" ] || { echo "缺素材 $f（见 docs/STAGE3_DESIGN.md §6；rootfs 先跑 build-rootfs.sh）" >&2; exit 1; }
done
{ [ -r /dev/kvm ] && [ -w /dev/kvm ]; } || { echo "无权访问 /dev/kvm（加入 kvm 组后重启 WSL）" >&2; exit 1; }

mkdir -p "$SNAP"; rm -f "$SNAP/vmstate" "$SNAP/memfile" "$UDS"
BASE="$(mktemp -d)"

cat > "$BASE/config.json" <<EOF
{ "boot-source": { "kernel_image_path": "$KERNEL",
    "boot_args": "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda ro init=/init MSBACKEND=kernel" },
  "drives": [ { "drive_id": "rootfs", "path_on_host": "$ROOTFS", "is_root_device": true, "is_read_only": true } ],
  "machine-config": { "vcpu_count": 1, "mem_size_mib": 512 },
  "vsock": { "guest_cid": 3, "uds_path": "$UDS" } }
EOF

echo "[build-snapshot] 启动 base VM ..."
"$FC" --api-sock "$BASE/api.sock" --config-file "$BASE/config.json" > "$BASE/console.log" 2>&1 &
FCPID=$!
trap 'kill $FCPID 2>/dev/null || true; rm -rf "$BASE"' EXIT

echo "[build-snapshot] 预热 Jupyter kernel（health + 跑一段 pass 强制起 kernel）..."
PYTHONPATH="$REPO_ROOT/src" python3 - "$UDS" <<'PY'
import sys, time, json
from microsandbox.client import _VsockTransport
t = _VsockTransport(sys.argv[1], 1024)
for _ in range(300):
    try:
        with t.request("GET", "/health", timeout=2) as r:
            if r.status == 200:
                break
    except Exception:
        pass
    time.sleep(0.1)
else:
    sys.exit("health 失败：daemon 未就绪")
body = json.dumps({"code": "pass", "language": "python", "timeout_seconds": 60}).encode()
with t.request("POST", "/execute", body=body, headers={"Content-Type": "application/json"}) as r:
    for _ in r:
        pass
print("[build-snapshot] kernel 已就绪")
PY

api() {  # method path body -> 打印 HTTP 码
  curl -fsS --unix-socket "$BASE/api.sock" -X "$1" "http://localhost$2" \
    -H 'Content-Type: application/json' -d "$3" -o /dev/null -w '%{http_code}'
}
echo "[build-snapshot] 暂停 VM ..."
[ "$(api PATCH /vm '{"state":"Paused"}')" = "204" ] || { echo "暂停失败" >&2; exit 1; }
echo "[build-snapshot] 创建 Full 快照 ..."
code="$(api PUT /snapshot/create "{\"snapshot_type\":\"Full\",\"snapshot_path\":\"$SNAP/vmstate\",\"mem_file_path\":\"$SNAP/memfile\"}")"
[ "$code" = "204" ] || { echo "快照失败 HTTP $code" >&2; exit 1; }

kill $FCPID 2>/dev/null || true; wait $FCPID 2>/dev/null || true
rm -f "$UDS"  # base 留下的 socket 没用，删掉（恢复时 firecracker 会在该路径重建）
echo "[build-snapshot] 完成：$SNAP/{vmstate,memfile}（memfile $(du -h "$SNAP/memfile" | cut -f1)）"
