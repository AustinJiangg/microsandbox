package main

// Unit tests for the gRPC TemplateService, with a fake builder injected so the async
// state machine (BUILDING -> SUCCESS / FAILED) is exercised without docker / KVM. Status
// is polled, exactly as the api polls it -- which also avoids racing the build goroutine.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	pb "microsandbox/services/pkg/grpc/templatemanager"
)

// fakeBuilder stands in for *build.Builder: it returns canned outcomes instead of running
// docker / firecracker. validateErr fails TemplateCreate synchronously; buildErr makes the
// async build end FAILED.
type fakeBuilder struct {
	validateErr error
	buildErr    error

	mu       sync.Mutex
	gotBase  string // the base passed to the last Build (asserts the layered field is plumbed through)
	gotBuilt bool
}

func (f *fakeBuilder) ValidateName(name string) error { return f.validateErr }
func (f *fakeBuilder) Build(buildID, name, dockerfile, base string, withSnapshot bool) error {
	f.mu.Lock()
	f.gotBase, f.gotBuilt = base, true
	f.mu.Unlock()
	return f.buildErr
}

// waitState polls TemplateBuildStatus until the build reaches want (or fails the test).
func waitState(t *testing.T, ts *templateService, id string, want pb.TemplateBuildStatusResponse_State) *pb.TemplateBuildStatusResponse {
	t.Helper()
	for i := 0; i < 200; i++ {
		resp, err := ts.TemplateBuildStatus(context.Background(), &pb.TemplateBuildStatusRequest{BuildId: id})
		if err != nil {
			t.Fatalf("TemplateBuildStatus: %v", err)
		}
		if resp.GetState() == want {
			return resp
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("build %s did not reach %v", id, want)
	return nil
}

func TestTemplateCreateSuccess(t *testing.T) {
	ts := newTemplateService(&fakeBuilder{})
	resp, err := ts.TemplateCreate(context.Background(), &pb.TemplateCreateRequest{Name: "demo", Dockerfile: "FROM x"})
	if err != nil {
		t.Fatalf("TemplateCreate: %v", err)
	}
	if resp.GetBuildId() == "" {
		t.Fatal("TemplateCreate returned an empty build id")
	}
	waitState(t, ts, resp.GetBuildId(), pb.TemplateBuildStatusResponse_SUCCESS)
}

// TestTemplateCreatePlumbsBase asserts a layered request's `base` reaches the builder unchanged --
// the orchestrator-side half of the Stage 18 API (the api fills `from` in 18d).
func TestTemplateCreatePlumbsBase(t *testing.T) {
	fb := &fakeBuilder{}
	ts := newTemplateService(fb)
	resp, err := ts.TemplateCreate(context.Background(), &pb.TemplateCreateRequest{Name: "derived", Dockerfile: "FROM x", Base: "default"})
	if err != nil {
		t.Fatalf("TemplateCreate: %v", err)
	}
	waitState(t, ts, resp.GetBuildId(), pb.TemplateBuildStatusResponse_SUCCESS)
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if !fb.gotBuilt || fb.gotBase != "default" {
		t.Fatalf("builder got base=%q built=%v, want base=default built=true", fb.gotBase, fb.gotBuilt)
	}
}

// TestTemplateCreateRequestBaseWire round-trips the hand-edited descriptor over the wire: the new
// `base` field (number 4) must serialize and deserialize, since the api->orchestrator call is real gRPC.
func TestTemplateCreateRequestBaseWire(t *testing.T) {
	in := &pb.TemplateCreateRequest{Name: "derived", Dockerfile: "FROM x", WithSnapshot: true, Base: "default"}
	b, err := proto.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out pb.TemplateCreateRequest
	if err := proto.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.GetBase() != "default" || out.GetName() != "derived" || !out.GetWithSnapshot() {
		t.Fatalf("round-trip lost fields: %+v", &out)
	}
}

func TestTemplateCreateBuildFails(t *testing.T) {
	ts := newTemplateService(&fakeBuilder{buildErr: errors.New("docker build: boom")})
	resp, err := ts.TemplateCreate(context.Background(), &pb.TemplateCreateRequest{Name: "demo", Dockerfile: "FROM x"})
	if err != nil {
		t.Fatalf("TemplateCreate: %v", err)
	}
	final := waitState(t, ts, resp.GetBuildId(), pb.TemplateBuildStatusResponse_FAILED)
	if final.GetDetail() == "" {
		t.Error("a failed build should carry a detail message")
	}
}

func TestTemplateCreateRejectsBadName(t *testing.T) {
	ts := newTemplateService(&fakeBuilder{validateErr: errors.New("the default template cannot be built via the API")})
	_, err := ts.TemplateCreate(context.Background(), &pb.TemplateCreateRequest{Name: "default", Dockerfile: "FROM x"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("TemplateCreate(default): code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestTemplateBuildStatusUnknown(t *testing.T) {
	ts := newTemplateService(&fakeBuilder{})
	_, err := ts.TemplateBuildStatus(context.Background(), &pb.TemplateBuildStatusRequest{BuildId: "bld_nope"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("unknown build id: code = %v, want NotFound", status.Code(err))
	}
}
