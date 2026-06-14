#!/usr/bin/env bash
# Stage 3b: export the agent image into an ext4 rootfs for the Firecracker microVM (entirely without root).
#
# Approach (see docs/MICROVM_DESIGN.md §4):
#   1. docker export reuses the entire filesystem of the microsandbox-agent image built in Stage 2
#      (which already contains Python + ipykernel + jupyter_client);
#   2. inject our src/ (Stage 2 mounts it at runtime, but there is no such mount inside the VM, so it must live in the rootfs);
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

echo "[build-rootfs] injecting microsandbox source into /opt/microsandbox/src ..."
mkdir -p "$STAGING/opt/microsandbox"
cp -r "$REPO_ROOT/src" "$STAGING/opt/microsandbox/src"

echo "[build-rootfs] writing /init (PID 1) ..."
cat > "$STAGING/init" <<'INIT'
#!/bin/sh
# The microVM's PID 1: after mounting the pseudo-filesystems, exec the daemon (exec lets the daemon take over PID 1 directly).
# On failure (e.g. a python import error) PID 1 exits -> kernel panic=1 -> the Firecracker process exits too,
# so the host-side health check notices immediately, and the traceback is left in console.log -- this is a deliberate diagnostic path.
mount -t proc     proc /proc 2>/dev/null
mount -t sysfs    sys  /sys  2>/dev/null
mount -t devtmpfs dev  /dev  2>/dev/null   # the kernel most likely already mounted it (DEVTMPFS_MOUNT=y); failure is harmless
mount -t tmpfs    tmp  /tmp  2>/dev/null    # the only writable area (the root is a read-only rootfs)

# PATH must be set explicitly: under a minimal init PATH may be empty, so sh cannot find commands, and python cannot compute
# sys.executable (commands.run's shell and the Jupyter kernel both need a real PATH).
export PATH=/usr/local/bin:/usr/bin:/bin
export HOME=/tmp PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1 PYTHONPATH=/opt/microsandbox/src
echo "[init] microsandbox daemon: vsock port 1024, kernel backend"
# exec with an absolute path: ensures python's sys.executable is a real path rather than an empty string
# (otherwise subprocess spawning from the daemon raises PermissionError).
PY="$(command -v python3)"
exec "$PY" -m microsandbox.server --vsock-port 1024
INIT
chmod +x "$STAGING/init"

size_mb=$(( $(du -sm "$STAGING" | cut -f1) + MARGIN_MB ))
echo "[build-rootfs] packing ext4 (mkfs.ext4 -d, no mount / without root), size ${size_mb}MB ..."
rm -f "$OUT"
mkdir -p "$(dirname "$OUT")"
mkfs.ext4 -q -L rootfs -d "$STAGING" "$OUT" "${size_mb}m"

echo "[build-rootfs] done: $OUT ($(du -h "$OUT" | cut -f1))"
