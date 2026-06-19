package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "microsandbox/services/pkg/grpc/templatemanager"
)

// builder is the subset of *build.Builder the template service needs. It is an interface so
// the async state machine here can be unit-tested with a fake build outcome (no docker/KVM).
type builder interface {
	ValidateName(name string) error
	Build(buildID, name, dockerfile string, withSnapshot bool) error
}

// templateService implements the gRPC TemplateService: it kicks asynchronous template builds
// and reports their status. Like E2B, the builder lives in the orchestrator (it needs the
// same docker + KVM + firecracker the VM fleet does). The build registry is in memory --
// the durable record of builds is the api's job (pkg/store, Stage 10c). See
// docs/STAGE10_DESIGN.md.
type templateService struct {
	pb.UnimplementedTemplateServiceServer
	builder builder

	mu     sync.Mutex
	builds map[string]*buildState // build id -> latest known state
}

type buildState struct {
	state  pb.TemplateBuildStatusResponse_State
	detail string
}

func newTemplateService(b builder) *templateService {
	return &templateService{builder: b, builds: map[string]*buildState{}}
}

// newBuildID mints a unique build id, mirroring fc.NewID's "<prefix>_<hex>" shape.
func newBuildID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "bld_" + hex.EncodeToString(b)
}

// TemplateCreate validates the request, then starts the build in a goroutine and returns the
// id immediately -- the pipeline (docker build -> rootfs -> snapshot) is slow, so the caller
// polls TemplateBuildStatus rather than blocking. A bad name is a synchronous
// InvalidArgument (not an async build failure).
func (t *templateService) TemplateCreate(ctx context.Context, req *pb.TemplateCreateRequest) (*pb.TemplateCreateResponse, error) {
	if err := t.builder.ValidateName(req.GetName()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	id := newBuildID()
	t.set(id, pb.TemplateBuildStatusResponse_BUILDING, "")
	go func() {
		if err := t.builder.Build(id, req.GetName(), req.GetDockerfile(), req.GetWithSnapshot()); err != nil {
			t.set(id, pb.TemplateBuildStatusResponse_FAILED, err.Error())
			return
		}
		t.set(id, pb.TemplateBuildStatusResponse_SUCCESS, "")
	}()
	return &pb.TemplateCreateResponse{BuildId: id}, nil
}

// TemplateBuildStatus returns a build's current state; an unknown id is NotFound.
func (t *templateService) TemplateBuildStatus(ctx context.Context, req *pb.TemplateBuildStatusRequest) (*pb.TemplateBuildStatusResponse, error) {
	t.mu.Lock()
	st, ok := t.builds[req.GetBuildId()]
	t.mu.Unlock()
	if !ok {
		return nil, status.Error(codes.NotFound, "no such build: "+req.GetBuildId())
	}
	return &pb.TemplateBuildStatusResponse{State: st.state, Detail: st.detail}, nil
}

func (t *templateService) set(id string, state pb.TemplateBuildStatusResponse_State, detail string) {
	t.mu.Lock()
	t.builds[id] = &buildState{state: state, detail: detail}
	t.mu.Unlock()
}
