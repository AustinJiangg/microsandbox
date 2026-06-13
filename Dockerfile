# Stage 2b: agent image (microsandbox-agent).
#
# On top of the official python:3.12-slim, install the Jupyter kernel runtime
# (ipykernel + jupyter_client) so the in-container daemon can host a resident
# Python kernel, giving a stateful REPL across run_code calls (the equivalent of
# E2B's code interpreter).
#
# Note: the source code is not COPYied into the image; instead the host's src/ is
# bind-mounted read-only at docker run time (see client._spawn_resident_container)
# -- so you can edit code during development without rebuilding the image. The
# image only holds the "rarely-changing, slow-to-install" dependencies. The source
# will only be baked into the image once Stage 4 productionizes things.
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

# No ENTRYPOINT/CMD: the actual startup command is supplied by the client at
# docker run time (python -m microsandbox.server --host 0.0.0.0 --port ... --backend kernel),
# sharing the same docker run invocation style as the container backend.
