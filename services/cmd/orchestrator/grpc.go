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
	ls, err := g.srv.create(cfg.GetFromSnapshot(), tmpl)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &pb.SandboxCreateResponse{SandboxId: ls.vm.ID}, nil
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

// Pause / Resume are the RPCs behind sandbox relocation (Stage 26). Since Stage 26R the real
// orchestrator implements Pause: a live checkpoint of the running VM to object storage under the
// api-minted build id, via the Stage 20/22 re-snapshot producer (server.pause, relocate.go). It
// works only in --nbd s3 mode -- the checkpoint is COW diffs in the bucket and the rootfs diff
// comes from the per-VM writable NBD overlay -- so other modes answer FailedPrecondition (the RPC
// *is* implemented; the mode can't satisfy it -- design D3, deliberately distinct from
// Unimplemented). Resume (the explicit-build-id restore) lands with 26R-d and answers
// Unimplemented until then. See docs/STAGE26R_DESIGN.md.
func (g *sandboxService) Pause(ctx context.Context, req *pb.SandboxPauseRequest) (*emptypb.Empty, error) {
	if g.srv.storage == nil || !g.srv.useNBD {
		return nil, status.Error(codes.FailedPrecondition,
			"per-sandbox pause requires --nbd s3 mode: the checkpoint is COW diffs in object storage and the rootfs diff needs the per-VM writable NBD overlay -- see docs/STAGE26R_DESIGN.md")
	}
	if req.GetBuildId() == "" {
		return nil, status.Error(codes.InvalidArgument, "pause needs the api-minted build_id to store the checkpoint under")
	}
	ls, ok := g.srv.lookup(req.GetSandboxId())
	if !ok {
		return nil, status.Error(codes.NotFound, "no such sandbox: "+req.GetSandboxId())
	}
	// In --nbd s3 mode every sandbox is created with a writable overlay + base build id
	// (buildRootfsBacking); guard the invariant here rather than panic inside the producer.
	if ls.overlay == nil || ls.baseBuildID == "" {
		return nil, status.Error(codes.Internal, "sandbox has no writable overlay/base build id to checkpoint against")
	}
	if err := g.srv.pause(ctx, ls, req.GetBuildId()); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	// The checkpoint is durable; the paused VM is now redundant, so free the node's slot. A false
	// here means a concurrent delete won the teardown race -- the checkpoint still stands.
	g.srv.destroy(req.GetSandboxId())
	return &emptypb.Empty{}, nil
}

func (g *sandboxService) Resume(ctx context.Context, req *pb.SandboxResumeRequest) (*pb.SandboxResumeResponse, error) {
	return nil, status.Error(codes.Unimplemented,
		"per-sandbox resume is not wired on this orchestrator yet (Stage 26R-d) -- see docs/STAGE26R_DESIGN.md")
}
