#!/usr/bin/env bash
# Stage 3c: produce a "warm snapshot" -- boot a microVM, warm up the Jupyter kernel, pause, then take a Full snapshot.
#
# The outputs {snapshot_dir}/{vmstate,memfile} let Sandbox(from_snapshot=True) restore in
# milliseconds: skipping the kernel boot + the Jupyter kernel cold start (see docs/MICROVM_DESIGN.md §8).
#
# The snapshot stores only "memory + device/CPU state"; the disk is still provided by the rootfs -- so on restore
# that rootfs must still be at its original path. The vsock uds is fixed at {snapshot_dir}/fc.vsock (recreated on restore).
#
# Stage 6: parameterized per template. With no args it builds the default snapshot
# (vendor/rootfs.ext4 -> vendor/snapshot); build-template.sh passes a template's own paths.
# Usage: scripts/build-snapshot.sh [rootfs] [out_snapshot_dir]
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
VENDOR="$REPO_ROOT/vendor"
# Stage 6: a snapshot is built per template. Default to the stock top-level paths (so
# the default template is unchanged); build-template.sh passes a template's own rootfs
# + snapshot dir. firecracker/vmlinux are host artifacts, always under vendor/.
ROOTFS="${1:-$VENDOR/rootfs.ext4}"
SNAP="${2:-$VENDOR/snapshot}"
FC="$VENDOR/firecracker"; KERNEL="$VENDOR/vmlinux"
UDS="$SNAP/fc.vsock"

command -v curl >/dev/null || { echo "curl is required to drive the Firecracker API" >&2; exit 1; }
for f in "$FC" "$KERNEL" "$ROOTFS"; do
  [ -e "$f" ] || { echo "missing artifact $f (see docs/MICROVM_DESIGN.md §7; for rootfs run build-rootfs.sh first)" >&2; exit 1; }
done
{ [ -r /dev/kvm ] && [ -w /dev/kvm ]; } || { echo "no access to /dev/kvm (join the kvm group, then restart WSL)" >&2; exit 1; }

mkdir -p "$SNAP"; rm -f "$SNAP/vmstate" "$SNAP/memfile" "$UDS"
BASE="$(mktemp -d)"

# Stage 12b: the snapshot must capture a configured eth0, because the data path now rides the
# VM's NIC (not vsock). So the base VM boots with a virtio-net backed by a host TAP, and the
# guest kernel brings eth0 up from the ip= boot arg (the minimal rootfs has no `ip` binary).
# Creating a TAP needs CAP_NET_ADMIN; on this single box that is the passwordless 'ip' granted in
# /etc/sudoers.d/microsandbox (Stage 12 Decision 7). These constants MUST match services/pkg/network
# (TapDevice / GuestMAC / vmIP / vmGateway / vmNetmask / BootIPArg) -- shell can't import the Go consts.
TAP=tap0
GUEST_MAC="06:00:AC:10:00:15"
VM_IP=169.254.0.21; GW_IP=169.254.0.22; NETMASK=255.255.255.252
IP_ARG="ip=${VM_IP}::${GW_IP}:${NETMASK}::eth0:off"
[ -e /dev/net/tun ] || { echo "missing /dev/net/tun (needed for the snapshot's NIC; see docs/STAGE12_DESIGN.md)" >&2; exit 1; }

# Tear the TAP (and the VM) down on any exit; set before creating the TAP so a mid-setup failure
# still cleans up. The base VM runs as the normal user, so the TAP is created user-owned (firecracker
# below opens it without CAP_NET_ADMIN); only the ip commands themselves need root.
trap 'kill ${FCPID:-} 2>/dev/null || true; sudo -n ip link del "$TAP" 2>/dev/null || true; rm -rf "$BASE"' EXIT

echo "[build-snapshot] setting up host TAP $TAP (needs passwordless 'sudo ip'; see docs/STAGE12_DESIGN.md) ..."
sudo -n ip link del "$TAP" 2>/dev/null || true   # clear any leftover from a previous interrupted build
sudo -n ip tuntap add "$TAP" mode tap user "$USER" \
  || { echo "failed to create TAP $TAP -- add 'NOPASSWD: ip' to /etc/sudoers.d/microsandbox (Stage 12)" >&2; exit 1; }
sudo -n ip addr add "$GW_IP/30" dev "$TAP"
sudo -n ip link set "$TAP" up

cat > "$BASE/config.json" <<EOF
{ "boot-source": { "kernel_image_path": "$KERNEL",
    "boot_args": "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda ro init=/init $IP_ARG" },
  "drives": [ { "drive_id": "rootfs", "path_on_host": "$ROOTFS", "is_root_device": true, "is_read_only": true } ],
  "machine-config": { "vcpu_count": 1, "mem_size_mib": 512 },
  "network-interfaces": [ { "iface_id": "eth0", "host_dev_name": "$TAP", "guest_mac": "$GUEST_MAC" } ],
  "vsock": { "guest_cid": 3, "uds_path": "$UDS" } }
EOF

echo "[build-snapshot] starting base VM ..."
"$FC" --api-sock "$BASE/api.sock" --config-file "$BASE/config.json" > "$BASE/console.log" 2>&1 &
FCPID=$!

echo "[build-snapshot] warming up the Jupyter kernel (health + running a 'pass' to force the kernel to start) ..."
# A self-contained vsock client: connect to Firecracker's UDS, do the CONNECT
# handshake, speak one HTTP/1.1 request, read to EOF. The SDK used to expose
# _VsockTransport for this, but Stage 4b moved all vsock into the Go control plane, so
# the warm-up carries its own ~20-line client (stdlib only) rather than importing the
# SDK. The daemon answers with `Connection: close`, so reading to EOF is safe.
python3 - "$UDS" <<'PY'
import socket, struct, sys, time, json

uds = sys.argv[1]
ENVD_PORT, CI_PORT = 1024, 1025  # Stage 11: envd (health) vs code-interpreter (kernel)

def vsock_request(port, method, path, body=b"", headers=None):
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    s.settimeout(65)  # the first Execute pays the Jupyter kernel cold start
    s.connect(uds)
    s.sendall(b"CONNECT %d\n" % port)            # Firecracker host->guest vsock handshake
    ack = b""
    while not ack.endswith(b"\n"):
        ch = s.recv(1)
        if not ch:
            raise OSError("vsock CONNECT closed before OK")
        ack += ch
    if not ack.startswith(b"OK"):
        raise OSError("vsock CONNECT rejected: %r" % ack)
    req = "%s %s HTTP/1.1\r\nHost: sandbox\r\nConnection: close\r\n" % (method, path)
    for k, v in (headers or {}).items():
        req += "%s: %s\r\n" % (k, v)
    if body:
        req += "Content-Length: %d\r\n" % len(body)
    req += "\r\n"
    s.sendall(req.encode() + body)
    data = b""                                   # Connection: close -> read to EOF
    while True:
        chunk = s.recv(65536)
        if not chunk:
            break
        data += chunk
    s.close()
    return data

# Wait for envd's /health on the envd port.
for _ in range(300):
    try:
        if vsock_request(ENVD_PORT, "GET", "/health").startswith(b"HTTP/1.1 200"):
            break
    except OSError:
        pass
    time.sleep(0.1)
else:
    sys.exit("health failed: daemon not ready")

# Warm the kernel via the code-interpreter's server-streaming Execute (ConnectRPC). The
# request is one Connect envelope ([flags=0][4-byte big-endian len][json]); reading the
# streamed response to EOF blocks until the cell finishes -- i.e. until the kernel is warm.
msg = json.dumps({"code": "pass", "language": "python", "timeoutSeconds": 60}).encode()
envelope = struct.pack(">BI", 0, len(msg)) + msg
vsock_request(CI_PORT, "POST", "/codeinterpreter.CodeInterpreterService/Execute", body=envelope,
              headers={"Content-Type": "application/connect+json", "Connect-Protocol-Version": "1"})
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
