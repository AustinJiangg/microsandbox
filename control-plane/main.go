// Command control-plane is the microsandbox control plane: a small HTTP service
// that owns the Firecracker microVM fleet. The Python SDK (client.py) used to
// fork firecracker itself; from Stage 4 on it asks this service over HTTP for a
// sandbox and runs code through it. See docs/STAGE4_DESIGN.md.
//
// This file wires up flags, routing and shutdown only. The real logic lives in
// server.go (handlers + VM registry), microvm.go (firecracker lifecycle) and,
// in Stage 4b, proxy.go (the transparent vsock bridge).
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "host:port to listen on")
	vendorDir := flag.String("vendor-dir", "vendor",
		"directory holding firecracker / vmlinux / rootfs.ext4 / snapshot")
	poolSize := flag.Int("pool-size", 0,
		"keep this many default-template microVMs warm for instant from_snapshot creates (0 = disabled)")
	var poolFlags repeatedFlag
	flag.Var(&poolFlags, "pool",
		"pre-warm K VMs of a named template (repeatable): --pool name=K")
	flag.Parse()

	poolSpecs, err := parsePoolSpecs(poolFlags, *poolSize)
	if err != nil {
		log.Fatal(err)
	}
	srv := newServer(*vendorDir, poolSpecs)

	// Go 1.22+ ServeMux: method + path-wildcard patterns. The trailing-slash
	// pattern is the catch-all transparent proxy (Stage 4b); the two exact
	// patterns are the lifecycle endpoints.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", srv.handleHealth)
	mux.HandleFunc("POST /sandboxes", srv.handleCreate)
	mux.HandleFunc("DELETE /sandboxes/{id}", srv.handleDestroy)
	mux.HandleFunc("/sandboxes/{id}/{rest...}", srv.handleProxy)

	httpServer := &http.Server{Addr: *addr, Handler: mux}

	// Graceful shutdown: on SIGINT/SIGTERM stop accepting, then destroy every VM
	// so we never leak firecracker processes (killing the process destroys the
	// whole VM -- see docs/ARCHITECTURE.md).
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		log.Println("shutting down: destroying all sandboxes")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(ctx)
		srv.destroyAll()
		os.Exit(0)
	}()

	log.Printf("control-plane listening on %s (vendor=%s, pools=%v)", *addr, *vendorDir, poolSpecs)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
