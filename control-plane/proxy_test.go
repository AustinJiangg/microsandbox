package main

// Unit tests for the vsock bridge (the Go port of the old Python test_transport.py).
// A local AF_UNIX server impersonates Firecracker's vsock UDS, feeding fixed bytes
// and asserting the bridge's behaviour -- so this error-prone byte handling
// (CONNECT handshake + HTTP/1.1 + SSE) is covered without booting a microVM. These
// run anywhere Go is installed (no firecracker / KVM needed).

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// fakeVsock starts an AF_UNIX server that runs handle(conn) for each connection,
// simulating Firecracker's vsock UDS. Returns the socket path.
func fakeVsock(t *testing.T, handle func(conn net.Conn)) string {
	t.Helper()
	uds := filepath.Join(t.TempDir(), "fc.vsock")
	ln, err := net.Listen("unix", uds)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				handle(conn)
			}()
		}
	}()
	return uds
}

type captured struct {
	requestLine string
	body        []byte
}

func TestVsockRoundTripDeliversRequestAndReadsJSON(t *testing.T) {
	got := make(chan captured, 1)
	uds := fakeVsock(t, func(conn net.Conn) {
		br := bufio.NewReader(conn)
		if line, _ := br.ReadString('\n'); line != "CONNECT 1024\n" {
			t.Errorf("connect line = %q, want CONNECT 1024", line)
		}
		io.WriteString(conn, "OK 1024\n")
		req, err := http.ReadRequest(br)
		if err != nil {
			t.Errorf("read request: %v", err)
			return
		}
		body, _ := io.ReadAll(req.Body)
		got <- captured{requestLine: req.Method + " " + req.URL.Path, body: body}
		payload := `{"content":"hi"}`
		fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n"+
			"Content-Length: %d\r\nConnection: close\r\n\r\n%s", len(payload), payload)
	})

	rt := &vsockRoundTripper{udsPath: uds, vsockPort: 1024}
	req, _ := http.NewRequest("POST", "http://sandbox/files/read", strings.NewReader(`{"path":"/tmp/x"}`))
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != `{"content":"hi"}` {
		t.Fatalf("response body = %q", body)
	}
	c := <-got
	if c.requestLine != "POST /files/read" {
		t.Errorf("forwarded request line = %q, want POST /files/read", c.requestLine)
	}
	if string(c.body) != `{"path":"/tmp/x"}` {
		t.Errorf("forwarded body = %q", c.body)
	}
}

func TestVsockProxyStreamsSSE(t *testing.T) {
	uds := fakeVsock(t, func(conn net.Conn) {
		br := bufio.NewReader(conn)
		br.ReadString('\n') // consume CONNECT
		io.WriteString(conn, "OK 1024\n")
		http.ReadRequest(br) // consume the request
		io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nConnection: close\r\n\r\n")
		io.WriteString(conn, "data: {\"type\":\"stdout\",\"data\":\"hello\\n\"}\n\n")
		io.WriteString(conn, "data: {\"type\":\"end\",\"exit_code\":0}\n\n")
		// returning closes conn -> the client reads EOF and ends the stream
	})

	// Exercise the full proxy ServeHTTP path through a front HTTP server.
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		vsockProxy(uds, 1024, "execute").ServeHTTP(w, r)
	}))
	defer front.Close()

	resp, err := http.Post(front.URL+"/x", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"type":"stdout"`) || !strings.Contains(string(body), `"type":"end"`) {
		t.Fatalf("proxied SSE missing events: %q", body)
	}
}

func TestVsockRoundTripConnectRejected(t *testing.T) {
	uds := fakeVsock(t, func(conn net.Conn) {
		bufio.NewReader(conn).ReadString('\n') // consume CONNECT
		io.WriteString(conn, "FAILED\n")        // simulate nothing listening on the guest port
	})
	rt := &vsockRoundTripper{udsPath: uds, vsockPort: 1024}
	req, _ := http.NewRequest("GET", "http://sandbox/health", nil)
	if _, err := rt.RoundTrip(req); err == nil {
		t.Fatal("expected an error when CONNECT is rejected")
	}
}

func TestVsockHealthy(t *testing.T) {
	uds := fakeVsock(t, func(conn net.Conn) {
		br := bufio.NewReader(conn)
		br.ReadString('\n') // consume CONNECT
		io.WriteString(conn, "OK 1024\n")
		http.ReadRequest(br) // consume GET /health
		io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
	})
	if !vsockHealthy(uds, 1024) {
		t.Fatal("vsockHealthy = false, want true")
	}
}

func TestVsockHealthyFalseWhenNoSocket(t *testing.T) {
	if vsockHealthy(filepath.Join(t.TempDir(), "absent.sock"), 1024) {
		t.Fatal("vsockHealthy = true for a missing socket, want false")
	}
}
