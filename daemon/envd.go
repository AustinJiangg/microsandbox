package main

// Stage 11a: the ConnectRPC envd services (Filesystem + Process), matching E2B's envd.
// They are mounted alongside the existing HTTP endpoints (server.go) -- nothing routes to
// them yet; the SDK flips to them in 11c, when the HTTP endpoints are removed. The actual
// filesystem/process logic lives in the small helpers below, shared with server.go's HTTP
// handlers so the two cannot drift while both exist. See docs/STAGE11_DESIGN.md.

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"connectrpc.com/connect"

	"microsandbox/daemon/genpb/envd"
	"microsandbox/daemon/genpb/envd/envdconnect"
)

// ----- shared filesystem/process core (used by the Connect services here and, until 11c
// removes them, by server.go's HTTP handlers) -----

func readFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	return string(b), err
}

// writeFile creates intermediate dirs then writes the file (server.py's behavior); on a
// read-only root, writing outside /tmp surfaces as the underlying error.
func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

type dirEntry struct {
	Name  string
	IsDir bool
}

func listDir(path string) ([]dirEntry, error) {
	dirents, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	entries := make([]dirEntry, 0, len(dirents))
	for _, d := range dirents {
		entries = append(entries, dirEntry{Name: d.Name(), IsDir: d.IsDir()})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}

// runCommand runs command via `sh -c` with a timeout, returning captured output and the
// exit code. On timeout: stderr carries the timeout message and exit_code is -1; a
// failed-to-start command is also -1 (mirroring the Python daemon / server.go's handleCommand).
func runCommand(command string, timeoutSeconds float64) (stdout, stderr string, exitCode int) {
	timeout := timeoutSeconds
	if timeout <= 0 {
		timeout = 30
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout*float64(time.Second)))
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return "", "command timed out after " + strconv.FormatFloat(timeout, 'g', -1, 64) + "s", -1
	}
	code := 0
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	} else if err != nil {
		code = -1 // failed to start (e.g. /bin/sh missing)
	}
	return out.String(), errb.String(), code
}

// ----- ConnectRPC services -----

type filesystemService struct {
	envdconnect.UnimplementedFilesystemServiceHandler
}

func (filesystemService) Read(_ context.Context, req *connect.Request[envd.ReadRequest]) (*connect.Response[envd.ReadResponse], error) {
	content, err := readFile(req.Msg.GetPath())
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	return connect.NewResponse(&envd.ReadResponse{Content: content}), nil
}

func (filesystemService) Write(_ context.Context, req *connect.Request[envd.WriteRequest]) (*connect.Response[envd.WriteResponse], error) {
	if err := writeFile(req.Msg.GetPath(), req.Msg.GetContent()); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	return connect.NewResponse(&envd.WriteResponse{}), nil
}

func (filesystemService) List(_ context.Context, req *connect.Request[envd.ListRequest]) (*connect.Response[envd.ListResponse], error) {
	entries, err := listDir(req.Msg.GetPath())
	if err != nil {
		return nil, connect.NewError(connect.CodeNotFound, err)
	}
	out := make([]*envd.Entry, 0, len(entries))
	for _, e := range entries {
		out = append(out, &envd.Entry{Name: e.Name, IsDir: e.IsDir})
	}
	return connect.NewResponse(&envd.ListResponse{Entries: out}), nil
}

type processService struct {
	envdconnect.UnimplementedProcessServiceHandler
}

func (processService) Run(_ context.Context, req *connect.Request[envd.RunRequest]) (*connect.Response[envd.RunResponse], error) {
	stdout, stderr, code := runCommand(req.Msg.GetCommand(), req.Msg.GetTimeoutSeconds())
	return connect.NewResponse(&envd.RunResponse{Stdout: stdout, Stderr: stderr, ExitCode: int32(code)}), nil
}

// registerEnvdServices mounts the Connect Filesystem + Process handlers on mux. In 11a they
// sit alongside the HTTP endpoints with nothing routed to them; they become the only path
// in 11c. The connect paths (/envd.FilesystemService/..., /envd.ProcessService/...) don't
// collide with the HTTP routes.
func registerEnvdServices(mux *http.ServeMux) {
	fsPath, fsHandler := envdconnect.NewFilesystemServiceHandler(filesystemService{})
	mux.Handle(fsPath, fsHandler)
	procPath, procHandler := envdconnect.NewProcessServiceHandler(processService{})
	mux.Handle(procPath, procHandler)
}
