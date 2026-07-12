package main

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	pb "microsandbox/services/pkg/grpc/orchestrator"
	"microsandbox/services/pkg/template"
)

// sandboxService adapts the server's VM fleet to the gRPC SandboxService contract
// (proto/orchestrator/orchestrator.proto). The api calls these methods over gRPC; each
// is a thin translation to a server method, returning a gRPC status code that the api
// maps back to an HTTP status (codes.InvalidArgument -> 400, codes.NotFound -> 404,
// else 500) so the SDK sees exactly the statuses the Stage-8a HTTP control plane gave.
//
// Embedding the generated UnimplementedSandboxServiceServer gives forward
// compatibility: new RPCs added to the .proto won't break this server until we
// implement them (they answer codes.Unimplemented in the meantime).
type sandboxService struct {
	pb.UnimplementedSandboxServiceServer
	srv *server
}

func (g *sandboxService) Create(ctx context.Context, req *pb.SandboxCreateRequest) (*pb.SandboxCreateResponse, error) {
	cfg := req.GetConfig()
	// An unknown/invalid template name is the caller's error -> InvalidArgument (400).
	tmpl, err := template.Resolve(g.srv.vendorDir, cfg.GetTemplate())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	// Stage 8 ignores cfg.Vcpu / cfg.MemMb: fc bakes in 1 vCPU / 512 MiB. Per-template
	// resource limits are a later stage.
	vm, err := g.srv.create(cfg.GetFromSnapshot(), tmpl)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &pb.SandboxCreateResponse{SandboxId: vm.ID}, nil
}

func (g *sandboxService) Delete(ctx context.Context, req *pb.SandboxDeleteRequest) (*emptypb.Empty, error) {
	if !g.srv.destroy(req.GetSandboxId()) {
		return nil, status.Error(codes.NotFound, "no such sandbox: "+req.GetSandboxId())
	}
	return &emptypb.Empty{}, nil
}

func (g *sandboxService) List(ctx context.Context, _ *emptypb.Empty) (*pb.SandboxListResponse, error) {
	return &pb.SandboxListResponse{SandboxIds: g.srv.list()}, nil
}

// Pause / Resume are the RPCs behind sandbox relocation (Stage 26). The api-side scheduling --
// pause a sandbox, then resume it on another node when its origin is draining -- is fully
// implemented and verified in process (cmd/api). The real per-sandbox live snapshot is
// deliberately NOT wired here: the only way to checkpoint a running VM is fc.MicroVM.Snapshot,
// the Stage 20/22 live-VM re-snapshot path (consistent only under v1.10.1 + --nbd + a writable
// overlay, and so far used only for build-time template snapshots). Wiring it per-sandbox is a
// clean follow-on that reuses that producer (fc.MicroVM.Snapshot + storage.PublishMemfileDiff /
// PublishRootfsDiff) behind these same RPCs; until then the real orchestrator answers a clear
// Unimplemented rather than pretend to persist state it hasn't. See docs/STAGE26_DESIGN.md D4.
func (g *sandboxService) Pause(ctx context.Context, req *pb.SandboxPauseRequest) (*emptypb.Empty, error) {
	return nil, status.Error(codes.Unimplemented,
		"per-sandbox live pause is not wired on this orchestrator; it reuses the Stage 20/22 snapshot producer (deferred) -- see docs/STAGE26_DESIGN.md")
}

func (g *sandboxService) Resume(ctx context.Context, req *pb.SandboxResumeRequest) (*pb.SandboxResumeResponse, error) {
	return nil, status.Error(codes.Unimplemented,
		"per-sandbox resume is not wired on this orchestrator; it reuses the Stage 20/22 snapshot producer (deferred) -- see docs/STAGE26_DESIGN.md")
}
