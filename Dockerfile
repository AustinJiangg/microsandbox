# Agent image (microsandbox-agent): the base the microVM rootfs is exported from.
#
# On top of the official python:3.12-slim, install the Jupyter kernel runtime
# (ipykernel + jupyter_client) so the in-VM daemon can host a resident Python
# kernel, giving a stateful REPL across run_code calls (the equivalent of E2B's
# code interpreter).
#
# This image is never `docker run`; instead scripts/build-rootfs.sh does
# `docker export` on it, injects src/microsandbox + a minimal /init, and packs the
# result into an ext4 rootfs for Firecracker. So docker is only a one-time build
# tool here, not a runtime.
#
# Build: docker build -t microsandbox-agent .
FROM python:3.12-slim

# Install the kernel runtime and register the python3 kernelspec under sys.prefix,
# so that AsyncKernelManager(kernel_name="python3") inside the container can find it.
RUN pip install --no-cache-dir ipykernel jupyter_client \
    && python -m ipykernel install --sys-prefix --name python3

# Don't try to write .pyc under a read-only root; unbuffer output to keep streaming real-time.
ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1

# No ENTRYPOINT/CMD: this image is exported into a rootfs, where the microVM's
# /init (written by scripts/build-rootfs.sh) starts the daemon
# (python -m microsandbox.server --vsock-port 1024).
