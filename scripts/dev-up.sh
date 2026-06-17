#!/usr/bin/env bash
# Build and run the host services (orchestrator + api) for local use -- Stage 8. The SDK
# then talks to the api at http://127.0.0.1:8080. Ctrl-C stops both. Any extra args (e.g.
# --pool-size 2, --pool name=K) are forwarded to the orchestrator. See docs/STAGE8_DESIGN.md.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
"$repo_root/scripts/build-services.sh"

"$repo_root/vendor/orchestrator" \
	--grpc-addr 127.0.0.1:9090 --proxy-addr 127.0.0.1:5007 \
	--vendor-dir "$repo_root/vendor" "$@" &
orch=$!
"$repo_root/vendor/api" \
	--addr 127.0.0.1:8080 \
	--orchestrator-grpc 127.0.0.1:9090 --orchestrator-proxy 127.0.0.1:5007 \
	--db "$repo_root/vendor/microsandbox.db" &
api=$!

# Stop both on Ctrl-C / exit; the orchestrator destroys any running VMs as it goes down.
trap 'kill "$api" "$orch" 2>/dev/null' INT TERM EXIT
echo "services up: api on http://127.0.0.1:8080 (SDK base_url); orchestrator gRPC :9090, proxy :5007"
wait
