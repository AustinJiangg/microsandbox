#!/usr/bin/env bash
# Build and run the host services (orchestrator + client-proxy + api) for local use.
# The SDK talks to the api at http://127.0.0.1:8080 (lifecycle); the api writes each sandbox's
# data route to a shared Redis catalog (Stage 14a) that client-proxy reads to route. Ctrl-C
# stops all three (Redis is left running for reuse). Any extra args (e.g. --pool-size 2,
# --pool name=K, --uffd) are forwarded to the orchestrator. See docs/STAGE14_DESIGN.md.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
"$repo_root/scripts/build-services.sh"

# Bring up the shared Redis catalog the api writes and client-proxy reads. Reuse one already
# listening on :6379; else prefer `docker compose` (docker-compose.yml is the canonical spec),
# falling back to a plain `docker run` of the same image on engines without the compose plugin.
if ! (exec 3<>/dev/tcp/127.0.0.1/6379) 2>/dev/null; then
	if ! docker compose -f "$repo_root/docker-compose.yml" up -d --wait redis 2>/dev/null; then
		docker rm -f microsandbox-redis >/dev/null 2>&1 || true
		docker run -d --name microsandbox-redis -p 127.0.0.1:6379:6379 redis:7-alpine >/dev/null
	fi
fi

"$repo_root/vendor/orchestrator" \
	--grpc-addr 127.0.0.1:9090 --proxy-addr 127.0.0.1:5007 \
	--vendor-dir "$repo_root/vendor" "$@" &
orch=$!
"$repo_root/vendor/client-proxy" \
	--addr 127.0.0.1:8081 --redis-addr 127.0.0.1:6379 &
cp=$!
"$repo_root/vendor/api" \
	--addr 127.0.0.1:8080 \
	--orchestrator-grpc 127.0.0.1:9090 --orchestrator-proxy 127.0.0.1:5007 \
	--redis-addr 127.0.0.1:6379 --data-url http://127.0.0.1:8081 \
	--db "$repo_root/vendor/microsandbox.db" &
api=$!

# Stop all three on Ctrl-C / exit; the orchestrator destroys any running VMs as it goes down.
trap 'kill "$api" "$cp" "$orch" 2>/dev/null' INT TERM EXIT
echo "services up: api on http://127.0.0.1:8080 (SDK base_url); client-proxy data :8081 (catalog on redis :6379); orchestrator gRPC :9090 proxy :5007"
wait
