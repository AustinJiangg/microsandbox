package main

// Stage 11a unit tests for the ConnectRPC envd services. They drive the real handlers
// through a real Connect client over an httptest HTTP/1.1 server -- the same protocol that
// will ride the vsock bridge in production -- so no vsock or VM is needed.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"

	"microsandbox/daemon/genpb/envd"
	"microsandbox/daemon/genpb/envd/envdconnect"
)

// startEnvd stands up the Connect envd services on an httptest server and returns clients.
func startEnvd(t *testing.T) (envdconnect.FilesystemServiceClient, envdconnect.ProcessServiceClient) {
	t.Helper()
	mux := http.NewServeMux()
	registerEnvdServices(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return envdconnect.NewFilesystemServiceClient(srv.Client(), srv.URL),
		envdconnect.NewProcessServiceClient(srv.Client(), srv.URL)
}

func TestFilesystemWriteReadList(t *testing.T) {
	fs, _ := startEnvd(t)
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "f.txt") // sub/ doesn't exist -> Write must create it

	if _, err := fs.Write(ctx, connect.NewRequest(&envd.WriteRequest{Path: path, Content: "hello"})); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := fs.Read(ctx, connect.NewRequest(&envd.ReadRequest{Path: path}))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Msg.GetContent() != "hello" {
		t.Fatalf("Read content = %q, want hello", got.Msg.GetContent())
	}
	list, err := fs.List(ctx, connect.NewRequest(&envd.ListRequest{Path: filepath.Join(dir, "sub")}))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list.Msg.GetEntries()) != 1 || list.Msg.GetEntries()[0].GetName() != "f.txt" {
		t.Fatalf("List entries = %+v, want one entry f.txt", list.Msg.GetEntries())
	}
}

func TestFilesystemReadMissingIsNotFound(t *testing.T) {
	fs, _ := startEnvd(t)
	_, err := fs.Read(context.Background(),
		connect.NewRequest(&envd.ReadRequest{Path: filepath.Join(t.TempDir(), "nope")}))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("Read missing: code = %v, want NotFound", connect.CodeOf(err))
	}
}

func TestProcessRun(t *testing.T) {
	_, proc := startEnvd(t)
	resp, err := proc.Run(context.Background(),
		connect.NewRequest(&envd.RunRequest{Command: "echo hi; echo oops 1>&2; exit 3"}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Msg.GetStdout() != "hi\n" || resp.Msg.GetStderr() != "oops\n" || resp.Msg.GetExitCode() != 3 {
		t.Fatalf("Run = %+v, want stdout=hi\\n stderr=oops\\n exit=3", resp.Msg)
	}
}

func TestProcessRunTimeout(t *testing.T) {
	_, proc := startEnvd(t)
	resp, err := proc.Run(context.Background(),
		connect.NewRequest(&envd.RunRequest{Command: "sleep 5", TimeoutSeconds: 0.2}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if resp.Msg.GetExitCode() != -1 {
		t.Fatalf("timeout exit_code = %d, want -1", resp.Msg.GetExitCode())
	}
}
