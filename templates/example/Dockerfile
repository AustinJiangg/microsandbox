# Example Stage 6 template (see docs/STAGE6_DESIGN.md).
#
# A template's recipe is just a Dockerfile. It should start FROM the agent image
# (which carries python3 + the Jupyter kernel runtime the in-VM daemon needs), then
# add whatever the sandbox should ship with -- pip installs, files, config, etc.
#
# This example stays deliberately tiny: it only drops a marker file the stock image
# lacks, so the e2e that boots it (docker export + mkfs.ext4) stays cheap. Swap the
# marker line for `RUN pip install <pkg>` to make a real custom environment.
#
# Build:  docker build -t microsandbox-agent .     # the base image, once
#         scripts/build-template.sh example         # -> vendor/templates/example/
FROM microsandbox-agent
RUN echo "hello from the example template" > /etc/microsandbox-template
