package main

import (
	"flag"
	"log"
	"net"
	"net/http"
)

// The daemon runs inside the Firecracker microVM as PID 1's payload (/init execs it).
// Stage 11b splits it into E2B's two in-VM services on two vsock ports: envd (Filesystem /
// Process / health + the legacy HTTP endpoints) and code-interpreter (the stateful kernel).
// Both drive the one shared kernelManager. --addr serves both on one TCP port for dev/test.
func main() {
	addr := flag.String("addr", "", "TCP host:port to listen on (dev/test, one combined port); empty = vsock (in-VM)")
	vsockPort := flag.Uint("vsock-port", 1024, "envd vsock port (in-VM)")
	ciVsockPort := flag.Uint("ci-vsock-port", 1025, "code-interpreter vsock port (in-VM)")
	flag.Parse()

	km := newKernelManager()

	// Dev/test over TCP: no orchestrator to route by port, so serve envd + code-interpreter
	// together on the one --addr (the real e2e path is vsock, below).
	if *addr != "" {
		mux := newMux()
		registerCodeInterpreterService(mux, km)
		ln, err := net.Listen("tcp", *addr)
		if err != nil {
			log.Fatalf("listen: %v", err)
		}
		log.Printf("microsandbox daemon (envd + code-interpreter) listening on %s", ln.Addr())
		if err := http.Serve(ln, mux); err != nil {
			log.Fatal(err)
		}
		return
	}

	// In-VM: the kernel's Jupyter gateway talks ZMQ over 127.0.0.1, and a microVM's
	// loopback defaults to down -- bring it up best-effort (warn only).
	if e := bringLoopbackUp(); e != nil {
		log.Printf("warning: could not bring up loopback (kernel may be unavailable): %v", e)
	}
	// Two vsock listeners on the one vsock device: Firecracker multiplexes them by
	// CONNECT <port>, and the orchestrator routes /codeinterpreter.* to ciVsockPort.
	envdLn, err := vsockListen(uint32(*vsockPort))
	if err != nil {
		log.Fatalf("envd vsock listen: %v", err)
	}
	ciLn, err := vsockListen(uint32(*ciVsockPort))
	if err != nil {
		log.Fatalf("code-interpreter vsock listen: %v", err)
	}

	ciMux := http.NewServeMux()
	registerCodeInterpreterService(ciMux, km)
	go func() {
		if err := http.Serve(ciLn, ciMux); err != nil {
			log.Fatalf("code-interpreter serve: %v", err)
		}
	}()

	log.Printf("microsandbox daemon: envd on %s, code-interpreter on %s", envdLn.Addr(), ciLn.Addr())
	if err := http.Serve(envdLn, newMux()); err != nil {
		log.Fatal(err)
	}
}
