package main

import (
	"net"

	"github.com/mdlayher/vsock"
)

// vsockListen returns a net.Listener on this VM's vsock at the given port. The host
// (control plane) reaches it through Firecracker's UDS + the CONNECT handshake. A
// net.Listener is all stdlib net/http needs, so the daemon serves HTTP/SSE with no
// hand-rolled parser. (mdlayher/vsock is the project's first external Go dependency;
// see docs/STAGE7_DESIGN.md Decision 6 -- vsock is not in the standard library.)
func vsockListen(port uint32) (net.Listener, error) {
	return vsock.Listen(port, nil)
}
