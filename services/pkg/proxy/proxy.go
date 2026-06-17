// Package proxy is the host side of the vsock data path: it bridges plain HTTP
// requests to the in-VM daemon (envd) over Firecracker's vsock UDS, and probes the
// daemon's /health the same way. Ported verbatim from control-plane/proxy.go (Stage
// 8a: relocated; the three entry points are exported now that the orchestrator -- and,
// from Stage 9, client-proxy -- live in other packages). It knows nothing about VMs:
// callers pass the per-VM uds path + vsock port (fc.MicroVM.UDSPath, fc.VsockPort).
package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"
)

// vsockRoundTripper bridges one HTTP request to the in-VM daemon over Firecracker's
// vsock UDS. It is the Go port of client.py's _VsockTransport: dial the UDS, do the
// `CONNECT <port>` text handshake, then speak plain HTTP/1.1 over the raw stream.
type vsockRoundTripper struct {
	udsPath   string
	vsockPort int
}

func (t *vsockRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	conn, err := net.Dial("unix", t.udsPath)
	if err != nil {
		return nil, err
	}
	// Firecracker vsock handshake: CONNECT <port> -> OK <hostport>.
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", t.vsockPort); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	ack, err := br.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, err
	}
	if !strings.HasPrefix(ack, "OK") {
		// e.g. nothing is listening on that vsock port inside the guest.
		conn.Close()
		return nil, fmt.Errorf("vsock CONNECT rejected: %q", strings.TrimSpace(ack))
	}
	// Forward the request, then read the daemon's response off the same stream (br
	// already holds any bytes buffered past the CONNECT ack, so reuse it).
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, err
	}
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, err
	}
	// Close the vsock connection once the response body is drained/closed.
	resp.Body = &connClosingBody{ReadCloser: resp.Body, conn: conn}
	return resp, nil
}

// connClosingBody closes the underlying vsock connection when the response body is
// closed -- http.ReadResponse's body does not own the connection, so we do.
type connClosingBody struct {
	io.ReadCloser
	conn net.Conn
}

func (b *connClosingBody) Close() error {
	err := b.ReadCloser.Close()
	b.conn.Close()
	return err
}

// VsockProxy returns a reverse proxy that forwards a request to the in-VM daemon at
// /<rest> over vsock. FlushInterval -1 flushes every write immediately, so the
// daemon's SSE stream (/execute) reaches the SDK live.
func VsockProxy(udsPath string, vsockPort int, rest string) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = "sandbox" // ignored by the vsock transport, but must be a valid URL
			pr.Out.URL.Path = "/" + rest
			pr.Out.URL.RawQuery = ""
			pr.Out.Host = "sandbox"
		},
		Transport:     &vsockRoundTripper{udsPath: udsPath, vsockPort: vsockPort},
		FlushInterval: -1,
	}
}

// WaitHealthy polls the in-VM daemon's /health over vsock until it answers 200 or
// the timeout elapses. Ported from client.py's _wait_until_healthy; the control
// plane now does it, so a sandbox is healthy by the time POST /sandboxes returns.
func WaitHealthy(udsPath string, vsockPort int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if VsockHealthy(udsPath, vsockPort) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("did not become healthy within %s", timeout)
}

// VsockHealthy does one /health probe over vsock, returning whether it answered 200.
func VsockHealthy(udsPath string, vsockPort int) bool {
	conn, err := net.DialTimeout("unix", udsPath, time.Second)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", vsockPort); err != nil {
		return false
	}
	br := bufio.NewReader(conn)
	ack, err := br.ReadString('\n')
	if err != nil || !strings.HasPrefix(ack, "OK") {
		return false
	}
	if _, err := io.WriteString(conn,
		"GET /health HTTP/1.1\r\nHost: sandbox\r\nConnection: close\r\n\r\n"); err != nil {
		return false
	}
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}
