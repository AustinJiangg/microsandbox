package envdclient

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRunRequestShape asserts the client speaks Connect unary JSON exactly as the daemon's connect-go
// handler expects: POST to /envd.ProcessService/Run, the two headers, and a camelCase body carrying the
// command + timeout. A drift here is what would silently 404/415 against the real envd.
func TestRunRequestShape(t *testing.T) {
	var gotPath, gotMethod, gotCT, gotProto string
	var gotBody runRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		gotCT = r.Header.Get("Content-Type")
		gotProto = r.Header.Get("Connect-Protocol-Version")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Write([]byte(`{"stdout":"ok\n","exitCode":0}`))
	}))
	defer srv.Close()

	res, err := New(srv.URL).Run(context.Background(), "echo ok", 12.5)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/envd.ProcessService/Run" {
		t.Errorf("path = %q, want /envd.ProcessService/Run", gotPath)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}
	if gotProto != "1" {
		t.Errorf("Connect-Protocol-Version = %q, want 1", gotProto)
	}
	if gotBody.Command != "echo ok" || gotBody.TimeoutSeconds != 12.5 {
		t.Errorf("request body = %+v, want {command:echo ok, timeoutSeconds:12.5}", gotBody)
	}
	if res.Stdout != "ok\n" || res.ExitCode != 0 {
		t.Errorf("result = %+v, want {stdout:ok\\n exitCode:0}", res)
	}
}

// TestRunEmptyResponse: proto3 JSON drops zero-valued fields, so a no-output success (e.g. `sync`) comes
// back as `{}`. It must decode to the zero RunResult (exit 0), not error.
func TestRunEmptyResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	res, err := New(srv.URL).Run(context.Background(), "sync", 0)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res != (RunResult{}) {
		t.Errorf("result = %+v, want zero RunResult", res)
	}
}

// TestRunNonZeroExit: a command that ran but failed (non-zero exit) is a normal result, not a transport
// error -- the producer inspects ExitCode to decide a build step failed.
func TestRunNonZeroExit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"exitCode":2,"stderr":"boom\n"}`))
	}))
	defer srv.Close()

	res, err := New(srv.URL).Run(context.Background(), "false", 0)
	if err != nil {
		t.Fatalf("Run returned error for a non-zero exit (should be a normal result): %v", err)
	}
	if res.ExitCode != 2 || res.Stderr != "boom\n" {
		t.Errorf("result = %+v, want {exitCode:2 stderr:boom\\n}", res)
	}
}

// TestRunConnectError: a non-2xx carries a Connect error {code, message}; Run surfaces the message.
func TestRunConnectError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"code":"internal","message":"kernel gone"}`))
	}))
	defer srv.Close()

	_, err := New(srv.URL).Run(context.Background(), "echo hi", 0)
	if err == nil {
		t.Fatal("Run: want error on non-2xx, got nil")
	}
	if !strings.Contains(err.Error(), "kernel gone") {
		t.Errorf("error = %q, want it to carry the Connect message %q", err, "kernel gone")
	}
}

// TestRunUnreachable: a dead endpoint is an error (the producer must fail the build, not snapshot a VM
// whose layer command never ran).
func TestRunUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // close immediately so the address refuses connections

	_, err := New(srv.URL).Run(context.Background(), "echo hi", 0)
	if err == nil {
		t.Fatal("Run: want error against a closed server, got nil")
	}
}
