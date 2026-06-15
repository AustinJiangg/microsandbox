package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// The daemon's HTTP layer is transport-agnostic, so we exercise it over a plain TCP
// httptest server -- no vsock, no VM/KVM -- mirroring how control-plane's tests run
// without a VM. (vsock + loopback are Linux/in-VM wiring, verified for real only by
// the Python e2e suite after the 7c rootfs flip.)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(newMux(newKernelManager()))
	t.Cleanup(srv.Close)
	return srv
}

func post(t *testing.T, url, body string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, data
}

func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestHealth(t *testing.T) {
	srv := newTestServer(t)
	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "ok" {
		t.Errorf("status = %q, want ok", body["status"])
	}
}

func TestFilesWriteReadList(t *testing.T) {
	srv := newTestServer(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "data.txt") // sub/ is absent -> exercises MkdirAll

	resp, _ := post(t, srv.URL+"/files/write", `{"path":`+jsonStr(path)+`,"content":"42"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("write status = %d", resp.StatusCode)
	}

	resp, data := post(t, srv.URL+"/files/read", `{"path":`+jsonStr(path)+`}`)
	if resp.StatusCode != 200 {
		t.Fatalf("read status = %d", resp.StatusCode)
	}
	var rd struct {
		Content string `json:"content"`
	}
	_ = json.Unmarshal(data, &rd)
	if rd.Content != "42" {
		t.Errorf("content = %q, want 42", rd.Content)
	}

	resp, data = post(t, srv.URL+"/files/list", `{"path":`+jsonStr(filepath.Dir(path))+`}`)
	if resp.StatusCode != 200 {
		t.Fatalf("list status = %d", resp.StatusCode)
	}
	var ld struct {
		Entries []struct {
			Name  string `json:"name"`
			IsDir bool   `json:"is_dir"`
		} `json:"entries"`
	}
	_ = json.Unmarshal(data, &ld)
	if len(ld.Entries) != 1 || ld.Entries[0].Name != "data.txt" || ld.Entries[0].IsDir {
		t.Errorf("entries = %+v, want one file data.txt", ld.Entries)
	}
}

func TestFileReadMissing(t *testing.T) {
	srv := newTestServer(t)
	resp, _ := post(t, srv.URL+"/files/read", `{"path":"/no/such/file/xyz"}`)
	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestCommand(t *testing.T) {
	srv := newTestServer(t)
	resp, data := post(t, srv.URL+"/commands", `{"command":"echo hi; exit 3","timeout_seconds":10}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var cd struct {
		Stdout   string `json:"stdout"`
		ExitCode int    `json:"exit_code"`
	}
	_ = json.Unmarshal(data, &cd)
	if cd.Stdout != "hi\n" || cd.ExitCode != 3 {
		t.Errorf("stdout=%q exit=%d, want \"hi\\n\" / 3", cd.Stdout, cd.ExitCode)
	}
}

func TestCommandTimeout(t *testing.T) {
	srv := newTestServer(t)
	resp, data := post(t, srv.URL+"/commands", `{"command":"sleep 5","timeout_seconds":0.3}`)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var cd struct {
		ExitCode int    `json:"exit_code"`
		Stderr   string `json:"stderr"`
	}
	_ = json.Unmarshal(data, &cd)
	if cd.ExitCode != -1 || !strings.Contains(cd.Stderr, "timed out") {
		t.Errorf("exit=%d stderr=%q, want -1 / contains 'timed out'", cd.ExitCode, cd.Stderr)
	}
}
