#!/usr/bin/env bash
# Build the Go control plane into vendor/control-plane.
#
# The control plane owns the Firecracker microVM fleet (Stage 4); the Python SDK
# talks to it over HTTP. The binary is a regenerable artifact, so it lives under
# vendor/ alongside the firecracker binary (vendor/ is gitignored).
# See docs/STAGE4_DESIGN.md.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root/control-plane"
go build -o "$repo_root/vendor/control-plane" .
echo "built $repo_root/vendor/control-plane"
