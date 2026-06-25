package main

// Unit tests for the edge data proxy, KVM-free: a local httptest server impersonates the
// orchestrator data proxy (which would otherwise bridge to a VM over TCP), so we cover
// <port>-<id> hostname routing, the 400/404 error paths, and streaming without booting a
// microVM. The catalog here is an InMemory test double; the live Redis backend is covered
// in services/pkg/catalog/redis_test.go.

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"microsandbox/services/pkg/catalog"
)

// newTestProxy returns a clientProxy with an empty catalog and the real data proxy.
func newTestProxy() *clientProxy {
	return &clientProxy{catalog: catalog.NewInMemory(), proxy: newDataProxy()}
}

func TestHandleDataMalformedHost(t *testing.T) {
	// A Host that isn't <port>-<sandboxId> is a 400 (here: no dash, so no port/id to split out).
	req := httptest.NewRequest("POST", "/execute", nil)
	req.Host = "nodash"
	rec := httptest.NewRecorder()
	newTestProxy().handleData(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed host: status = %d, want 400", rec.Code)
	}
}

func TestHandleDataUnknownSandbox(t *testing.T) {
	req := httptest.NewRequest("POST", "/execute", nil)
	req.Host = "49983-sb_unknown" // well-formed route, but the catalog has no such sandbox
	rec := httptest.NewRecorder()
	newTestProxy().handleData(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown sandbox: status = %d, want 404", rec.Code)
	}
}

// The happy path: a <port>-<id> Host is reverse-proxied to the sandbox's node with the path
// preserved and the id + port handed over as X-Sandbox-Id / X-Sandbox-Port, body passing through.
func TestHandleDataRoutesToNode(t *testing.T) {
	var gotPath, gotID, gotPort string
	var gotBody []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotID = r.Header.Get("X-Sandbox-Id")
		gotPort = r.Header.Get("X-Sandbox-Port")
		gotBody, _ = io.ReadAll(r.Body)
		io.WriteString(w, `{"ok":true}`)
	}))
	defer backend.Close()

	cp := newTestProxy()
	cp.catalog.Set("sb_1", strings.TrimPrefix(backend.URL, "http://"))

	front := httptest.NewServer(http.HandlerFunc(cp.handleData))
	defer front.Close()

	req, _ := http.NewRequest("POST", front.URL+"/execute", strings.NewReader(`{"code":"x=1"}`))
	req.Host = "49983-sb_1" // <port>-<id>: the envd port for sandbox sb_1
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if gotPath != "/execute" {
		t.Errorf("forwarded path = %q, want /execute", gotPath)
	}
	if gotID != "sb_1" {
		t.Errorf("forwarded X-Sandbox-Id = %q, want sb_1", gotID)
	}
	if gotPort != "49983" {
		t.Errorf("forwarded X-Sandbox-Port = %q, want 49983", gotPort)
	}
	if string(gotBody) != `{"code":"x=1"}` {
		t.Errorf("forwarded body = %q", gotBody)
	}
	if !strings.Contains(string(body), `"ok":true`) {
		t.Errorf("response body = %q, want it to carry the backend's reply", body)
	}
}

// SSE must stream through: the backend emits two events and the client sees both (proves
// FlushInterval -1 carries /execute's event stream).
func TestHandleDataStreamsSSE(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"stdout\",\"data\":\"hi\\n\"}\n\n")
		w.(http.Flusher).Flush()
		io.WriteString(w, "data: {\"type\":\"end\",\"exit_code\":0}\n\n")
	}))
	defer backend.Close()

	cp := newTestProxy()
	cp.catalog.Set("sb_1", strings.TrimPrefix(backend.URL, "http://"))
	front := httptest.NewServer(http.HandlerFunc(cp.handleData))
	defer front.Close()

	req, _ := http.NewRequest("POST", front.URL+"/execute", nil)
	req.Host = "49999-sb_1" // the code-interpreter port; any well-formed route streams the same
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()

	var sawStdout, sawEnd bool
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if strings.Contains(sc.Text(), `"type":"stdout"`) {
			sawStdout = true
		}
		if strings.Contains(sc.Text(), `"type":"end"`) {
			sawEnd = true
		}
	}
	if !sawStdout || !sawEnd {
		t.Fatalf("streamed SSE missing events (stdout=%v end=%v)", sawStdout, sawEnd)
	}
}
