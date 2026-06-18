#!/usr/bin/env bash
# Build and run the host services (orchestrator + client-proxy + api) for local use --
# Stage 9. The SDK talks to the api at http://127.0.0.1:8080 (lifecycle); the api registers
# each sandbox's data route in client-proxy. Ctrl-C stops all three. Any extra args (e.g.
# --pool-size 2, --pool name=K) are forwarded to the orchestrator. See docs/STAGE9_DESIGN.md.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
"$repo_root/scripts/build-services.sh"

"$repo_root/vendor/orchestrator" \
	--grpc-addr 127.0.0.1:9090 --proxy-addr 127.0.0.1:5007 \
	--vendor-dir "$repo_root/vendor" "$@" &
orch=$!
"$repo_root/vendor/client-proxy" \
	--addr 127.0.0.1:8081 --internal-addr 127.0.0.1:5008 &
cp=$!
"$repo_root/vendor/api" \
	--addr 127.0.0.1:8080 \
	--orchestrator-grpc 127.0.0.1:9090 --orchestrator-proxy 127.0.0.1:5007 \
	--client-proxy-internal 127.0.0.1:5008 \
	--db "$repo_root/vendor/microsandbox.db" &
api=$!

# Stop all three on Ctrl-C / exit; the orchestrator destroys any running VMs as it goes down.
trap 'kill "$api" "$cp" "$orch" 2>/dev/null' INT TERM EXIT
echo "services up: api on http://127.0.0.1:8080 (SDK base_url); client-proxy data :8081 internal :5008; orchestrator gRPC :9090 proxy :5007"
wait
