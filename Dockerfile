# 阶段 2b：agent 镜像（microsandbox-agent）。
#
# 在官方 python:3.12-slim 基础上装入 Jupyter kernel 运行时（ipykernel +
# jupyter_client），让容器内 daemon 能托管一个常驻 Python kernel，实现跨
# run_code 的有状态 REPL（对应 E2B 的 code interpreter）。
#
# 注意：源码不 COPY 进镜像，而是 docker run 时把宿主 src/ 只读挂载进来
# （见 client._spawn_resident_container）——开发期改代码免重建镜像。镜像里
# 只放「不常变、装起来慢」的依赖。等阶段 4 产品化时才会把源码也烘进镜像。
#
# 构建：docker build -t microsandbox-agent .
FROM python:3.12-slim

# 装 kernel 运行时，并把 python3 kernelspec 注册到 sys.prefix，这样容器内
# AsyncKernelManager(kernel_name="python3") 能找到它。
RUN pip install --no-cache-dir ipykernel jupyter_client \
    && python -m ipykernel install --sys-prefix --name python3

# 只读根下别尝试写 .pyc；输出不缓冲，保证流式实时。
ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1

# 不写 ENTRYPOINT/CMD：实际启动命令由 client 在 docker run 时给出
# （python -m microsandbox.server --host 0.0.0.0 --port ... --backend kernel），
# 与 container 后端共用同一套 docker run 调用方式。
