#!/usr/bin/env bash
# Stage 3b: export the agent image into an ext4 rootfs for the Firecracker microVM (entirely without root).
#
# Approach (see docs/MICROVM_DESIGN.md §4):
#   1. docker export reuses the entire filesystem of the microsandbox-agent image built in Stage 2
#      (which already contains Python + ipykernel + jupyter_client);
#   2. build the Go daemon (Stage 7, E2B's envd) as a static binary and inject it (the Python daemon is retired);
#   3. write a minimal /init as PID 1: after mounting the pseudo-filesystems, exec our daemon (listening on vsock);
#   4. use `mkfs.ext4 -d <dir>` to pack the directory straight into an ext4 image -- no mount needed, hence without root.
#
# Why being without root matters: the current user on this host can run docker / mkfs.ext4, but sudo prompts for a password. `mkfs.ext4 -d`
# keeps the whole pipeline unprivileged (files in the image are owned by the build user, but the daemon inside the guest runs as root,
# and root can read everything, so it is fine).
#
# Usage: scripts/build-rootfs.sh [image] [output_path] [extra_margin_MB]
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
IMAGE="${1:-microsandbox-agent:latest}"
OUT="${2:-$REPO_ROOT/vendor/rootfs.ext4}"
MARGIN_MB="${3:-300}"   # writable margin reserved on top of the actual size of the exported content

command -v mkfs.ext4 >/dev/null || { echo "missing mkfs.ext4 (apt install e2fsprogs)" >&2; exit 1; }
docker image inspect "$IMAGE" >/dev/null 2>&1 || {
  echo "missing image $IMAGE, please run docker build -t microsandbox-agent . first" >&2; exit 1; }

STAGING="$(mktemp -d)"
trap 'rm -rf "$STAGING"' EXIT

echo "[build-rootfs] exporting the filesystem of $IMAGE ..."
cid="$(docker create "$IMAGE")"
# Exclude device nodes under /dev: non-root cannot mknod, and the kernel (DEVTMPFS_MOUNT=y) rebuilds /dev automatically.
docker export "$cid" | tar -x -C "$STAGING" --exclude='dev/*' \
  || echo "[build-rootfs] tar emitted non-fatal warnings (mostly special files, ignored)"
docker rm "$cid" >/dev/null

# At a minimum python3 must be present; otherwise the export clearly went wrong, so fail early to ease debugging.
test -x "$STAGING/usr/local/bin/python3" || { echo "exported rootfs has no python3, aborting" >&2; exit 1; }

echo "[build-rootfs] building the Go daemon (static, linux/amd64) and injecting it ..."
# Stage 7: the in-VM daemon is now a static Go binary (E2B's envd), not the Python
# package. CGO-free so it runs in the minimal guest with no libc deps; the Jupyter
# kernel it drives is still Python, launched at runtime via the kernel gateway.
mkdir -p "$STAGING/usr/local/bin"
( cd "$REPO_ROOT/daemon" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o "$STAGING/usr/local/bin/microsandbox-daemon" . )
chmod +x "$STAGING/usr/local/bin/microsandbox-daemon"

echo "[build-rootfs] writing /init (PID 1) ..."
cat > "$STAGING/init" <<'INIT'
#!/bin/sh
# The microVM's PID 1: after mounting the pseudo-filesystems, exec the Go daemon (exec lets it take over PID 1 directly).
# On failure PID 1 exits -> kernel panic=1 -> the Firecracker process exits too, so the host-side health check notices
# immediately, and any error is left in console.log -- a deliberate diagnostic path.
mount -t proc     proc /proc 2>/dev/null
mount -t sysfs    sys  /sys  2>/dev/null
mount -t devtmpfs dev  /dev  2>/dev/null   # the kernel most likely already mounted it (DEVTMPFS_MOUNT=y); failure is harmless
mount -t tmpfs    tmp  /tmp  2>/dev/null    # the only writable area (the root is a read-only rootfs)

# PATH must be explicit under a minimal init: the daemon execs `jupyter` (the kernel gateway), and the gateway + kernel
# need a real PATH; HOME=/tmp is the only writable home (the root is read-only).
export PATH=/usr/local/bin:/usr/bin:/bin
export HOME=/tmp PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1
echo "[init] microsandbox daemon (Go): vsock port 1024, kernel via jupyter gateway"
exec /usr/local/bin/microsandbox-daemon
INIT
chmod +x "$STAGING/init"

size_mb=$(( $(du -sm "$STAGING" | cut -f1) + MARGIN_MB ))
echo "[build-rootfs] packing ext4 (mkfs.ext4 -d, no mount / without root), size ${size_mb}MB ..."
rm -f "$OUT"
mkdir -p "$(dirname "$OUT")"
mkfs.ext4 -q -L rootfs -d "$STAGING" "$OUT" "${size_mb}m"

echo "[build-rootfs] done: $OUT ($(du -h "$OUT" | cut -f1))"
