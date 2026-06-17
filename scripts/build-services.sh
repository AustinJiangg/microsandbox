#!/usr/bin/env bash
# Build the Go host services into vendor/ -- one binary per services/cmd/* directory.
#
# Stage 8 splits the old single control-plane binary into separate E2B-shaped services
# (orchestrator now; api + client-proxy land in 8b/9). They are regenerable artifacts,
# so they live under vendor/ alongside the firecracker binary (vendor/ is gitignored).
# See docs/STAGE8_DESIGN.md.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root/services"
for dir in cmd/*/; do
	name="$(basename "$dir")"
	go build -o "$repo_root/vendor/$name" "./$dir"
	echo "built $repo_root/vendor/$name"
done
