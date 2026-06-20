package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
)

// Stage 12a: the daemon also listens on these TCP ports (E2B's), reachable over the VM's
// eth0 via the host's per-sandbox netns, alongside the vsock listeners. They must match
// fc.EnvdTCPPort / fc.CodeInterpreterTCPPort on the host side.
const (
	envdTCPPort = 49983
	ciTCPPort   = 49999
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

	envdMux := newMux()
	ciMux := http.NewServeMux()
	registerCodeInterpreterService(ciMux, km)
	go func() {
		if err := http.Serve(ciLn, ciMux); err != nil {
			log.Fatalf("code-interpreter serve: %v", err)
		}
	}()

	// Stage 12a: also serve both services over TCP (E2B's ports), reachable via the VM's
	// NIC, alongside vsock. Additive -- the orchestrator still routes the data path over
	// vsock until 12b; these listeners let the host verify the NIC path now. The same
	// handlers serve both transports; a bind failure is non-fatal (vsock still works).
	serveTCP := func(port int, h http.Handler, name string) {
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			log.Printf("warning: %s TCP listen on :%d failed: %v", name, port, err)
			return
		}
		go func() {
			if err := http.Serve(ln, h); err != nil {
				log.Printf("%s TCP serve: %v", name, err)
			}
		}()
	}
	serveTCP(envdTCPPort, envdMux, "envd")
	serveTCP(ciTCPPort, ciMux, "code-interpreter")

	log.Printf("microsandbox daemon: envd on %s + tcp :%d, code-interpreter on %s + tcp :%d",
		envdLn.Addr(), envdTCPPort, ciLn.Addr(), ciTCPPort)
	if err := http.Serve(envdLn, envdMux); err != nil {
		log.Fatal(err)
	}
}
