# Agent image (microsandbox-agent): the base the microVM rootfs is exported from.
#
# On top of the official python:3.12-slim, install the Jupyter kernel runtime
# (ipykernel) plus the Jupyter Kernel Gateway. As of Stage 7 the in-VM daemon is a
# Go binary (E2B's envd); it drives a stateful Python kernel by launching the kernel
# gateway and speaking its HTTP + WebSocket API -- so the gateway, not the daemon,
# hosts the kernel here (the equivalent of E2B's code interpreter).
#
# This image is never `docker run`; instead scripts/build-rootfs.sh does
# `docker export` on it, injects the Go daemon binary + a minimal /init, and packs
# the result into an ext4 rootfs for Firecracker. So docker is only a one-time build
# tool here, not a runtime.
#
# Build: docker build -t microsandbox-agent .
FROM python:3.12-slim

# Install the kernel runtime + the kernel gateway, and register the python3
# kernelspec under sys.prefix so the gateway's POST /api/kernels {"name":"python3"}
# can find it.
RUN pip install --no-cache-dir ipykernel jupyter-kernel-gateway \
    && python -m ipykernel install --sys-prefix --name python3

# Don't try to write .pyc under a read-only root; unbuffer output to keep streaming real-time.
ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1

# No ENTRYPOINT/CMD: this image is exported into a rootfs, where the microVM's
# /init (written by scripts/build-rootfs.sh) execs the Go daemon
# (/usr/local/bin/microsandbox-daemon), which drives the kernel via the gateway.
