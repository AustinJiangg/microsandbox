package main

import (
	"flag"
	"log"
	"net"
	"net/http"
)

// The daemon runs inside the Firecracker microVM as PID 1's payload (/init execs it).
// Its control channel is vsock; --addr lets tests/dev run it over TCP instead. Apart
// from the listener, everything is the transport-agnostic mux (server.go) -- the same
// "stable protocol, swappable transport" principle as the Python daemon.
func main() {
	addr := flag.String("addr", "", "TCP host:port to listen on (dev/test); empty = vsock (in-VM)")
	vsockPort := flag.Uint("vsock-port", 1024, "vsock port to listen on when --addr is empty")
	flag.Parse()

	var (
		ln  net.Listener
		err error
	)
	if *addr != "" {
		ln, err = net.Listen("tcp", *addr)
	} else {
		// In-VM: the kernel backend's Jupyter kernel talks ZMQ over 127.0.0.1, and a
		// microVM's loopback defaults to down -- bring it up best-effort (warn only).
		if e := bringLoopbackUp(); e != nil {
			log.Printf("warning: could not bring up loopback (kernel may be unavailable): %v", e)
		}
		ln, err = vsockListen(uint32(*vsockPort))
	}
	if err != nil {
		log.Fatalf("listen: %v", err)
	}

	log.Printf("microsandbox daemon listening on %s", ln.Addr())
	if err := http.Serve(ln, newMux()); err != nil {
		log.Fatal(err)
	}
}
