#!/usr/bin/env bash
# Stage 3c: produce a "warm snapshot" -- boot a microVM, warm up the Jupyter kernel, pause, then take a Full snapshot.
#
# The outputs vendor/snapshot/{vmstate,memfile} let Sandbox(backend="microvm", from_snapshot=True)
# restore in milliseconds: skipping the kernel boot + the Jupyter kernel cold start (see docs/MICROVM_DESIGN.md §8).
#
# The snapshot stores only "memory + device/CPU state"; the disk contents are still provided by the host's rootfs.ext4 -- so on restore
# rootfs.ext4 must still be at its original path. The vsock uds is fixed at vendor/snapshot/fc.vsock (recreated on restore).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VENDOR="$REPO_ROOT/vendor"
SNAP="$VENDOR/snapshot"
FC="$VENDOR/firecracker"; KERNEL="$VENDOR/vmlinux"; ROOTFS="$VENDOR/rootfs.ext4"
UDS="$SNAP/fc.vsock"

command -v curl >/dev/null || { echo "curl is required to drive the Firecracker API" >&2; exit 1; }
for f in "$FC" "$KERNEL" "$ROOTFS"; do
  [ -e "$f" ] || { echo "missing artifact $f (see docs/MICROVM_DESIGN.md §7; for rootfs run build-rootfs.sh first)" >&2; exit 1; }
done
{ [ -r /dev/kvm ] && [ -w /dev/kvm ]; } || { echo "no access to /dev/kvm (join the kvm group, then restart WSL)" >&2; exit 1; }

mkdir -p "$SNAP"; rm -f "$SNAP/vmstate" "$SNAP/memfile" "$UDS"
BASE="$(mktemp -d)"

cat > "$BASE/config.json" <<EOF
{ "boot-source": { "kernel_image_path": "$KERNEL",
    "boot_args": "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda ro init=/init" },
  "drives": [ { "drive_id": "rootfs", "path_on_host": "$ROOTFS", "is_root_device": true, "is_read_only": true } ],
  "machine-config": { "vcpu_count": 1, "mem_size_mib": 512 },
  "vsock": { "guest_cid": 3, "uds_path": "$UDS" } }
EOF

echo "[build-snapshot] starting base VM ..."
"$FC" --api-sock "$BASE/api.sock" --config-file "$BASE/config.json" > "$BASE/console.log" 2>&1 &
FCPID=$!
trap 'kill $FCPID 2>/dev/null || true; rm -rf "$BASE"' EXIT

echo "[build-snapshot] warming up the Jupyter kernel (health + running a 'pass' to force the kernel to start) ..."
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
    sys.exit("health failed: daemon not ready")
body = json.dumps({"code": "pass", "language": "python", "timeout_seconds": 60}).encode()
with t.request("POST", "/execute", body=body, headers={"Content-Type": "application/json"}) as r:
    for _ in r:
        pass
print("[build-snapshot] kernel is ready")
PY

api() {  # method path body -> prints the HTTP code
  curl -fsS --unix-socket "$BASE/api.sock" -X "$1" "http://localhost$2" \
    -H 'Content-Type: application/json' -d "$3" -o /dev/null -w '%{http_code}'
}
echo "[build-snapshot] pausing VM ..."
[ "$(api PATCH /vm '{"state":"Paused"}')" = "204" ] || { echo "pause failed" >&2; exit 1; }
echo "[build-snapshot] creating Full snapshot ..."
code="$(api PUT /snapshot/create "{\"snapshot_type\":\"Full\",\"snapshot_path\":\"$SNAP/vmstate\",\"mem_file_path\":\"$SNAP/memfile\"}")"
[ "$code" = "204" ] || { echo "snapshot failed HTTP $code" >&2; exit 1; }

kill $FCPID 2>/dev/null || true; wait $FCPID 2>/dev/null || true
rm -f "$UDS"  # the socket left by base is useless, delete it (firecracker recreates it at that path on restore)
echo "[build-snapshot] done: $SNAP/{vmstate,memfile} (memfile $(du -h "$SNAP/memfile" | cut -f1))"
