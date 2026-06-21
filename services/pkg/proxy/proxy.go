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

// TCPProxy returns a reverse proxy that forwards a request to the in-VM daemon at addr
// (the slot's RoutableIP:port) over plain TCP, preserving the request path. Stage 12b flips
// the data path from vsock to the VM's NIC; this is the TCP twin of VsockProxy. FlushInterval
// -1 flushes every write so code-interpreter's streamed Execute reaches the SDK live. The
// Transport sets Proxy nil on purpose: it bypasses any HTTP_PROXY in the (root, -E) env, which
// on WSL would otherwise intercept the 10.x slot address -- see TCPHealthy + the wsl2-proxy memory.
func TCPProxy(addr string) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = addr // path + query carry over from the inbound request
			pr.Out.Host = addr
		},
		Transport:     &http.Transport{Proxy: nil},
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

// TCPHealthy does one /health GET over TCP at addr (host:port), returning whether it
// answered 200. Stage 12a uses it to confirm a cold-started VM is reachable over its NIC
// (the path 12b switches the data plane to), alongside the authoritative vsock probe.
//
// Transport.Proxy is nil on purpose: it bypasses any HTTP_PROXY in the environment. On WSL
// the autoProxy would otherwise intercept the 10.x per-sandbox address (its no_proxy "10.*"
// glob is not honored) and the probe would spuriously fail. See docs/STAGE12_DESIGN.md
// (Decision 7) and the wsl2-proxy memory.
func TCPHealthy(addr string) bool {
	client := &http.Client{
		Timeout:   2 * time.Second,
		Transport: &http.Transport{Proxy: nil},
	}
	resp, err := client.Get("http://" + addr + "/health")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode == http.StatusOK
}

// TCPWaitHealthy polls the daemon's /health over TCP at addr until it answers 200 or the
// timeout elapses -- the TCP twin of WaitHealthy. Polling (not one-shot TCPHealthy) matters
// because the daemon's TCP listener can bind a beat after the VM is otherwise up (Stage 12a saw
// a single probe race readiness ~10% of the time), so the control plane must wait it out before
// it makes the TCP path authoritative and hands the sandbox over.
func TCPWaitHealthy(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if TCPHealthy(addr) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("did not become healthy (TCP %s) within %s", addr, timeout)
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
