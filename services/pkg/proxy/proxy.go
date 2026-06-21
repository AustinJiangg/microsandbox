// Package proxy is the host side of the data path: it reverse-proxies plain HTTP requests to
// the in-VM daemon (envd / the code-interpreter) over the VM's NIC (TCP), and probes the
// daemon's /health the same way. Stage 12 replaced the original vsock bridge with this TCP path
// (the daemon listens on TCP, reached through the per-sandbox netns); callers pass the slot's
// routable address + port (fc.MicroVM.Slot.Addr(port)).
package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"time"
)

// TCPProxy returns a reverse proxy that forwards a request to the in-VM daemon at addr (the
// slot's RoutableIP:port) over plain TCP, preserving the request path. Stage 12b flipped the
// data path from vsock to the VM's NIC. FlushInterval -1 flushes every write so the
// code-interpreter's streamed Execute reaches the SDK live. The Transport sets Proxy nil on
// purpose: it bypasses any HTTP_PROXY in the (root, -E) env, which on WSL would otherwise
// intercept the 10.x slot address -- see TCPHealthy + the wsl2-proxy memory.
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

// TCPHealthy does one /health GET over TCP at addr (host:port), returning whether it answered
// 200. Transport.Proxy is nil on purpose: it bypasses any HTTP_PROXY in the environment. On WSL
// the autoproxy would otherwise intercept the 10.x per-sandbox address (its no_proxy "10.*" glob
// is not honored) and the probe would spuriously fail. See docs/STAGE12_DESIGN.md + the wsl2-proxy memory.
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

// TCPWaitHealthy polls the daemon's /health over TCP at addr until it answers 200 or the timeout
// elapses. Polling (not one-shot TCPHealthy) matters because the daemon's TCP listener can bind a
// beat after the VM is otherwise up, so a single probe races readiness; the control plane waits it
// out before it makes the TCP path authoritative and hands the sandbox over.
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
