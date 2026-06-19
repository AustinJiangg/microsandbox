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
#   protoc-gen-connect-go   go install connectrpc.com/connect/cmd/protoc-gen-connect-go@latest
#                             (Stage 11: the in-VM daemon's envd / code-interpreter services)
#
# protoc itself shells out to the protoc-gen-* plugins by finding them on PATH, so we add
# the Go bin dir to PATH below.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

command -v protoc >/dev/null || {
	echo "protoc not found on PATH; see the install notes at the top of this script" >&2
	exit 1
}
# protoc invokes the code generators as plugins named protoc-gen-*, which `go install`
# drops into $(go env GOPATH)/bin -- make sure that is on PATH.
export PATH="$(go env GOPATH)/bin:$PATH"
for plugin in protoc-gen-go protoc-gen-go-grpc protoc-gen-connect-go; do
	command -v "$plugin" >/dev/null || {
		echo "$plugin not found on PATH; see the install notes at the top of this script" >&2
		exit 1
	}
done

# (1) Host services (gRPC): orchestrator + template-manager -> services/pkg/grpc/...
# --go_out / --go-grpc_out: where to write; --*_opt=module=... strips that prefix from each
# file's go_package option to compute its path.
( cd "$repo_root/services" && protoc -I proto \
	--go_out=. --go_opt=module=microsandbox/services \
	--go-grpc_out=. --go-grpc_opt=module=microsandbox/services \
	proto/orchestrator/orchestrator.proto \
	proto/templatemanager/template-manager.proto )
echo "generated: services/pkg/grpc/{orchestrator,templatemanager}/*.pb.go"

# (2) In-VM daemon (ConnectRPC, Stage 11): envd + code-interpreter -> daemon/genpb/...
# --connect-go_out emits the *.connect.go service stubs alongside the *.pb.go messages.
( cd "$repo_root/daemon" && protoc -I proto \
	--go_out=. --go_opt=module=microsandbox/daemon \
	--connect-go_out=. --connect-go_opt=module=microsandbox/daemon \
	proto/envd/envd.proto \
	proto/codeinterpreter/codeinterpreter.proto )
echo "generated: daemon/genpb/{envd,codeinterpreter}/*.{pb,connect}.go"
