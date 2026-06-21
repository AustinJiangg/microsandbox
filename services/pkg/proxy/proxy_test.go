package proxy

// Unit tests for the host-side data proxy over TCP (Stage 12 replaced the vsock bridge). A local
// httptest backend impersonates the in-VM daemon on its NIC, so TCPProxy's request/response
// forwarding + live streaming and the /health probes are covered without booting a microVM.
// These run anywhere Go is installed (no firecracker / KVM needed).

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TCPProxy forwards the request (method, path, body) to the daemon and returns its response.
func TestTCPProxyForwardsRequestAndResponse(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		io.WriteString(w, `{"content":"hi"}`)
	}))
	defer backend.Close()
	addr := strings.TrimPrefix(backend.URL, "http://")

	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		TCPProxy(addr).ServeHTTP(w, r)
	}))
	defer front.Close()

	resp, err := http.Post(front.URL+"/files/read", "application/json", strings.NewReader(`{"path":"/tmp/x"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if gotMethod != "POST" || gotPath != "/files/read" {
		t.Errorf("forwarded %s %s, want POST /files/read", gotMethod, gotPath)
	}
	if string(gotBody) != `{"path":"/tmp/x"}` {
		t.Errorf("forwarded body = %q", gotBody)
	}
	if string(body) != `{"content":"hi"}` {
		t.Errorf("response body = %q", body)
	}
}

// Streaming: the backend flushes two events; the client sees both (proves FlushInterval -1 carries
// the code-interpreter's Execute stream through the proxy live).
func TestTCPProxyStreamsSSE(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"stdout\",\"data\":\"hello\\n\"}\n\n")
		w.(http.Flusher).Flush()
		io.WriteString(w, "data: {\"type\":\"end\",\"exit_code\":0}\n\n")
	}))
	defer backend.Close()
	addr := strings.TrimPrefix(backend.URL, "http://")

	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		TCPProxy(addr).ServeHTTP(w, r)
	}))
	defer front.Close()

	resp, err := http.Post(front.URL+"/x", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"type":"stdout"`) || !strings.Contains(string(body), `"type":"end"`) {
		t.Fatalf("proxied stream missing events: %q", body)
	}
}

func TestTCPHealthy(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	if !TCPHealthy(strings.TrimPrefix(backend.URL, "http://")) {
		t.Fatal("TCPHealthy = false, want true")
	}
}

func TestTCPHealthyFalseWhenNothingListening(t *testing.T) {
	// Nothing listens on 127.0.0.1:1, so the probe fails fast (connection refused).
	if TCPHealthy("127.0.0.1:1") {
		t.Fatal("TCPHealthy = true for a dead address, want false")
	}
}

func TestTCPWaitHealthy(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	if err := TCPWaitHealthy(strings.TrimPrefix(backend.URL, "http://"), 2*time.Second); err != nil {
		t.Fatalf("TCPWaitHealthy = %v, want nil (backend is up)", err)
	}
	if err := TCPWaitHealthy("127.0.0.1:1", 300*time.Millisecond); err == nil {
		t.Fatal("TCPWaitHealthy = nil for a dead address, want a timeout error")
	}
}
