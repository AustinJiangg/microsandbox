#!/usr/bin/env bash
# Regenerate the gRPC Go stubs from services/proto/**/*.proto (Stage 8b).
#
# Generated *.pb.go files are committed, so a normal build/test needs only the Go
# toolchain -- you only run this when a .proto changes. It needs three external tools:
#
#   protoc                  the protobuf compiler -- NOT a Go tool. Install one of:
#                             - apt-get install -y protobuf-compiler
#                             - a release from https://github.com/protocolbuffers/protobuf/releases
#                               (unzip and put its bin/protoc on PATH)
#   protoc-gen-go           go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
#   protoc-gen-go-grpc      go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
#
# protoc itself shells out to the two protoc-gen-* plugins by finding them on PATH, so
# we add the Go bin dir to PATH below.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root/services"

command -v protoc >/dev/null || {
	echo "protoc not found on PATH; see the install notes at the top of this script" >&2
	exit 1
}
# protoc invokes the code generators as plugins named protoc-gen-go / protoc-gen-go-grpc,
# which `go install` drops into $(go env GOPATH)/bin -- make sure that is on PATH.
export PATH="$(go env GOPATH)/bin:$PATH"
for plugin in protoc-gen-go protoc-gen-go-grpc; do
	command -v "$plugin" >/dev/null || {
		echo "$plugin not found on PATH; see the install notes at the top of this script" >&2
		exit 1
	}
done

# --go_out / --go-grpc_out: where to write; --*_opt=module=... strips that prefix from
# each file's go_package option to compute its path (so they land under pkg/grpc/...).
protoc -I proto \
	--go_out=. --go_opt=module=microsandbox/services \
	--go-grpc_out=. --go-grpc_opt=module=microsandbox/services \
	proto/orchestrator/orchestrator.proto

echo "generated: services/pkg/grpc/orchestrator/*.pb.go"
