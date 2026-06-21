package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
)

// The daemon's two in-VM TCP ports (E2B's), reached over the VM's eth0 via the host's
// per-sandbox netns. They must match fc.EnvdTCPPort / fc.CodeInterpreterTCPPort on the host side.
const (
	envdTCPPort = 49983
	ciTCPPort   = 49999
)

// The daemon runs inside the Firecracker microVM as PID 1's payload (/init execs it). It splits
// into E2B's two in-VM services, each on its own TCP port reached via the VM's NIC (Stage 12c
// retired vsock): envd (Filesystem / Process / health) on :49983 and the code-interpreter (the
// stateful kernel) on :49999. Both drive the one shared kernelManager. --addr serves both on one
// combined TCP port for dev/test (where there is no orchestrator to route by port).
func main() {
	addr := flag.String("addr", "", "serve both services on this one TCP host:port (dev/test); empty = in-VM (envd :49983 + code-interpreter :49999)")
	flag.Parse()

	km := newKernelManager()

	// Dev/test over TCP: no orchestrator to route by port, so serve envd + code-interpreter
	// together on the one --addr (the real in-VM path splits them across two TCP ports, below).
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
	// Stage 12c: serve over the VM's NIC on E2B's two TCP ports (vsock retired). The orchestrator
	// dials the slot at the port carried in the <port>-<id> hostname -- envd or the code-interpreter.
	// Both serve 0.0.0.0, so they are reachable on eth0 through the host's per-sandbox netns DNAT.
	envdMux := newMux()
	ciMux := http.NewServeMux()
	registerCodeInterpreterService(ciMux, km)

	ciLn, err := net.Listen("tcp", fmt.Sprintf(":%d", ciTCPPort))
	if err != nil {
		log.Fatalf("code-interpreter listen on :%d: %v", ciTCPPort, err)
	}
	go func() {
		if err := http.Serve(ciLn, ciMux); err != nil {
			log.Fatalf("code-interpreter serve: %v", err)
		}
	}()

	envdLn, err := net.Listen("tcp", fmt.Sprintf(":%d", envdTCPPort))
	if err != nil {
		log.Fatalf("envd listen on :%d: %v", envdTCPPort, err)
	}
	log.Printf("microsandbox daemon: envd on :%d, code-interpreter on :%d", envdTCPPort, ciTCPPort)
	if err := http.Serve(envdLn, envdMux); err != nil {
		log.Fatal(err)
	}
}
