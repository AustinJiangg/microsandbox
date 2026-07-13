package main

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "microsandbox/services/pkg/grpc/orchestrator"
	"microsandbox/services/pkg/storage"
)

// Stage 26R-c: the real orchestrator's Pause guard discipline, KVM-free. The happy path
// (snapshot a live VM + publish the COW diffs) needs a real VM and is covered by the Stage 26R
// real-VM e2e; what is pinned here is the refusal contract the api relies on: a mode that cannot
// produce a checkpoint refuses with FailedPrecondition (design D3 -- the RPC is implemented, the
// mode can't satisfy it, deliberately not Unimplemented), a missing build id is the caller's
// error, and an unknown sandbox is NotFound.

// TestPauseRefusedOutsideNBDS3: both legacy modes -- local-fs (no object storage) and s3 without
// --nbd (no writable overlay) -- refuse a pause up front, before touching any sandbox.
func TestPauseRefusedOutsideNBDS3(t *testing.T) {
	cases := []struct {
		name string
		srv  *server
	}{
		{"local-fs", &server{sandboxes: map[string]*liveSandbox{}}},
		{"s3 without --nbd", &server{storage: storage.NewLocal(t.TempDir()), sandboxes: map[string]*liveSandbox{}}},
	}
	for _, c := range cases {
		svc := &sandboxService{srv: c.srv}
		_, err := svc.Pause(context.Background(), &pb.SandboxPauseRequest{SandboxId: "sb_x", BuildId: "bld_snap"})
		if status.Code(err) != codes.FailedPrecondition {
			t.Errorf("%s: want FailedPrecondition, got %v", c.name, err)
		}
	}
}

// TestPauseNeedsBuildID: the api owns the checkpoint's identity (D2); an empty build_id means the
// orchestrator would have to name the snapshot itself, so it is rejected as the caller's error.
func TestPauseNeedsBuildID(t *testing.T) {
	srv := &server{storage: storage.NewLocal(t.TempDir()), useNBD: true, sandboxes: map[string]*liveSandbox{}}
	svc := &sandboxService{srv: srv}
	_, err := svc.Pause(context.Background(), &pb.SandboxPauseRequest{SandboxId: "sb_x"})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("want InvalidArgument for a missing build_id, got %v", err)
	}
}

// TestPauseUnknownSandboxNotFound: an id this node doesn't hold maps to NotFound, mirroring
// Delete -- the api turns it into a 404, not a 500.
func TestPauseUnknownSandboxNotFound(t *testing.T) {
	srv := &server{storage: storage.NewLocal(t.TempDir()), useNBD: true, sandboxes: map[string]*liveSandbox{}}
	svc := &sandboxService{srv: srv}
	_, err := svc.Pause(context.Background(), &pb.SandboxPauseRequest{SandboxId: "sb_missing", BuildId: "bld_snap"})
	if status.Code(err) != codes.NotFound {
		t.Errorf("want NotFound for an unknown sandbox, got %v", err)
	}
}
