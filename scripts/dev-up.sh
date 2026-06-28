#!/usr/bin/env bash
# Build and run the host services (orchestrator + client-proxy + api) for local use.
# The SDK talks to the api at http://127.0.0.1:8080 (lifecycle); the api writes each sandbox's
# data route to a shared Redis catalog (Stage 14a) that client-proxy reads to route, keeps its
# metadata in Postgres (Stage 14b), and the orchestrator reads template artifacts from a MinIO
# object store (Stage 15, the default). Ctrl-C stops the three services (Redis + Postgres + MinIO
# are left running for reuse). Any extra args (e.g. --pool-size 2, --pool name=K, --storage
# local-fs) are forwarded to the orchestrator. See docs/STAGE14_DESIGN.md + docs/STAGE15_DESIGN.md.
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

# Bring up the Postgres backing the api metadata store (Stage 14b), same reuse/compose/docker-run
# strategy. Postgres opens its port mid-init then restarts, so the docker-run fallback waits on
# pg_isready before continuing (docker compose --wait does this via the healthcheck).
if ! (exec 3<>/dev/tcp/127.0.0.1/5432) 2>/dev/null; then
	if ! docker compose -f "$repo_root/docker-compose.yml" up -d --wait postgres 2>/dev/null; then
		docker rm -f microsandbox-postgres >/dev/null 2>&1 || true
		docker run -d --name microsandbox-postgres \
			-e POSTGRES_DB=microsandbox -e POSTGRES_HOST_AUTH_METHOD=trust \
			-p 127.0.0.1:5432:5432 postgres:16-alpine >/dev/null
		echo "waiting for postgres to be ready..."
		for _ in $(seq 1 60); do
			docker exec microsandbox-postgres pg_isready -U postgres -d microsandbox >/dev/null 2>&1 && break
			sleep 0.5
		done
	fi
fi

# Bring up the MinIO object store holding template artifacts (Stage 15, the orchestrator's default
# --storage s3), same reuse/compose/docker-run strategy. The minio image has no curl/mc, so we just
# wait for the port; then seed the locally-built templates into the bucket via the Go seeder, so the
# orchestrator can materialize/stream from it (in a real deployment the build pipeline is the writer).
if ! (exec 3<>/dev/tcp/127.0.0.1/9000) 2>/dev/null; then
	if ! docker compose -f "$repo_root/docker-compose.yml" up -d minio 2>/dev/null; then
		docker rm -f microsandbox-minio >/dev/null 2>&1 || true
		docker run -d --name microsandbox-minio \
			-e MINIO_ROOT_USER=minioadmin -e MINIO_ROOT_PASSWORD=minioadmin \
			-p 127.0.0.1:9000:9000 -p 127.0.0.1:9001:9001 \
			minio/minio:latest server /data --console-address ":9001" >/dev/null
	fi
	echo "waiting for minio to be ready..."
	for _ in $(seq 1 60); do (exec 3<>/dev/tcp/127.0.0.1/9000) 2>/dev/null && break; sleep 0.5; done
fi
( cd "$repo_root" && go run ./services/cmd/msb-seed --vendor-dir "$repo_root/vendor" --name default ) \
	|| echo "warning: seeding 'default' failed (build vendor/rootfs.ext4 + snapshot first, or pass --storage local-fs)"
if [ -f "$repo_root/vendor/templates/example/rootfs.ext4" ]; then
	( cd "$repo_root" && go run ./services/cmd/msb-seed --vendor-dir "$repo_root/vendor" --name example ) || true
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
	--store-dsn "postgres://postgres@127.0.0.1:5432/microsandbox?sslmode=disable" &
api=$!

# Stop all three on Ctrl-C / exit; the orchestrator destroys any running VMs as it goes down.
trap 'kill "$api" "$cp" "$orch" 2>/dev/null' INT TERM EXIT
echo "services up: api on http://127.0.0.1:8080 (SDK base_url); client-proxy data :8081 (catalog on redis :6379, store on postgres :5432); orchestrator gRPC :9090 proxy :5007 (artifacts on minio :9000)"
wait
