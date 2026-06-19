// Command orchestrator is the per-machine VM service. Stage 8b gives it two listeners:
//
//   - a gRPC SandboxService (grpc.go) -- the lifecycle seam the api calls (Create /
//     Delete / List); this is E2B's api -> orchestrator boundary.
//   - an HTTP data proxy (dataproxy.go) -- the vsock bridge to the in-VM daemon, routed
//     by the X-Sandbox-Id header. GET /health on this port is the orchestrator's own
//     liveness (no sandbox id); every other request is proxied into a VM.
//
// It still owns the microVM fleet + warm pool (server.go). This file wires flags, starts
// both listeners, and tears everything down (destroying all VMs) on a signal.
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"google.golang.org/grpc"

	"microsandbox/services/pkg/build"
	pb "microsandbox/services/pkg/grpc/orchestrator"
	pbtmpl "microsandbox/services/pkg/grpc/templatemanager"
	"microsandbox/services/pkg/storage"
)

func main() {
	grpcAddr := flag.String("grpc-addr", "127.0.0.1:9090", "host:port for the gRPC SandboxService (the api calls this)")
	proxyAddr := flag.String("proxy-addr", "127.0.0.1:5007", "host:port for the HTTP data proxy (vsock bridge to envd)")
	vendorDir := flag.String("vendor-dir", "vendor",
		"directory holding firecracker / vmlinux / rootfs.ext4 / snapshot")
	poolSize := flag.Int("pool-size", 0,
		"keep this many default-template microVMs warm for instant from_snapshot creates (0 = disabled)")
	var poolFlags repeatedFlag
	flag.Var(&poolFlags, "pool", "pre-warm K VMs of a named template (repeatable): --pool name=K")
	scriptsDir := flag.String("scripts-dir", "",
		"dir with build-rootfs.sh / build-snapshot.sh for template builds (default: sibling of --vendor-dir)")
	flag.Parse()

	poolSpecs, err := parsePoolSpecs(poolFlags, *poolSize)
	if err != nil {
		log.Fatal(err)
	}
	srv := newServer(*vendorDir, poolSpecs)

	// Template builder (Stage 10): the scripts dir defaults to the sibling of vendor (their
	// repo layout), overridable by flag. The builder writes artifacts in place under
	// vendorDir/templates/<name>/ via the storage provider.
	sd := *scriptsDir
	if sd == "" {
		sd = filepath.Join(filepath.Dir(*vendorDir), "scripts")
	}
	tmplBuilder := build.New(storage.NewLocal(*vendorDir), sd)

	// 1) gRPC SandboxService -- the lifecycle seam.
	lis, err := net.Listen("tcp", *grpcAddr)
	if err != nil {
		log.Fatal(err)
	}
	grpcServer := grpc.NewServer()
	pb.RegisterSandboxServiceServer(grpcServer, &sandboxService{srv: srv})
	pbtmpl.RegisterTemplateServiceServer(grpcServer, newTemplateService(tmplBuilder))

	// 2) HTTP data proxy -- the vsock bridge, routed by X-Sandbox-Id. The "GET /health"
	// pattern is more specific than "/", so the orchestrator's own liveness never gets
	// proxied into a VM (and nothing proxies GET /health to a sandbox anyway).
	proxyMux := http.NewServeMux()
	proxyMux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	proxyMux.HandleFunc("/", srv.handleData)
	proxyServer := &http.Server{Addr: *proxyAddr, Handler: proxyMux}

	go func() {
		if err := grpcServer.Serve(lis); err != nil {
			log.Fatalf("gRPC serve: %v", err)
		}
	}()
	go func() {
		if err := proxyServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("data proxy serve: %v", err)
		}
	}()
	log.Printf("orchestrator: gRPC on %s, data proxy on %s (vendor=%s, scripts=%s, pools=%v)",
		*grpcAddr, *proxyAddr, *vendorDir, sd, poolSpecs)

	// Graceful shutdown: stop accepting, then destroy every VM so we never leak
	// firecracker processes (killing the process destroys the whole VM).
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("shutting down: destroying all sandboxes")
	grpcServer.GracefulStop()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = proxyServer.Shutdown(ctx)
	srv.destroyAll()
}
