// Command orchestrator is the per-machine VM service. Stage 8b gives it two listeners:
//
//   - a gRPC SandboxService (grpc.go) -- the lifecycle seam the api calls (Create /
//     Delete / List); this is E2B's api -> orchestrator boundary.
//   - an HTTP data proxy (dataproxy.go) -- the TCP bridge to the in-VM daemon over the
//     VM's NIC (Stage 12 retired vsock), routed by the X-Sandbox-Id + X-Sandbox-Port
//     headers. GET /health on this port is the orchestrator's own liveness (no sandbox
//     id); every other request is proxied into a VM.
//
// It still owns the microVM fleet + warm pool (server.go). This file wires flags, starts
// both listeners, and tears everything down (destroying all VMs) on a signal.
package main

import (
	"context"
	"flag"
	"fmt"
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
	proxyAddr := flag.String("proxy-addr", "127.0.0.1:5007", "host:port for the HTTP data proxy (TCP bridge to envd over the VM's NIC)")
	vendorDir := flag.String("vendor-dir", "vendor",
		"directory holding firecracker / vmlinux / rootfs.ext4 / snapshot")
	poolSize := flag.Int("pool-size", 0,
		"keep this many default-template microVMs warm for instant from_snapshot creates (0 = disabled)")
	var poolFlags repeatedFlag
	flag.Var(&poolFlags, "pool", "pre-warm K VMs of a named template (repeatable): --pool name=K")
	scriptsDir := flag.String("scripts-dir", "",
		"dir with build-rootfs.sh / build-snapshot.sh for template builds (default: sibling of --vendor-dir)")
	useUffd := flag.Bool("uffd", false,
		"in local-fs mode, restore snapshots over a userfaultfd page-fault handler (pkg/uffd) instead of the File backend (Stage 13). In s3 mode the memfile always streams over UFFD, so this is ignored")
	storageMode := flag.String("storage", "s3",
		"artifact source (Stage 15): s3 (object storage, the default) or local-fs (read artifacts from --vendor-dir directly)")
	s3Endpoint := flag.String("s3-endpoint", "127.0.0.1:9000", "S3/MinIO endpoint host:port (no scheme), for --storage s3")
	s3Bucket := flag.String("s3-bucket", "msb", "S3 bucket holding template artifacts, for --storage s3")
	s3AccessKey := flag.String("s3-access-key", "minioadmin", "S3 access key, for --storage s3")
	s3SecretKey := flag.String("s3-secret-key", "minioadmin", "S3 secret key, for --storage s3")
	s3SSL := flag.Bool("s3-ssl", false, "use https for the S3 endpoint, for --storage s3")
	flag.Parse()

	poolSpecs, err := parsePoolSpecs(poolFlags, *poolSize)
	if err != nil {
		log.Fatal(err)
	}
	// Stage 15: build the artifact store (s3 default). In s3 mode this connects to MinIO/S3 and
	// creates the bucket if absent; a failure here is loud (the "flip the default" cost), exactly
	// like Stage 14's Postgres/Redis. local-fs returns a nil provider (read --vendor-dir directly).
	provider, err := newStorageProvider(*storageMode, *s3Endpoint, *s3AccessKey, *s3SecretKey, *s3Bucket, *s3SSL)
	if err != nil {
		log.Fatal(err)
	}
	srv := newServer(*vendorDir, poolSpecs, *useUffd, provider)

	// Template builder (Stage 10): the scripts dir defaults to the sibling of vendor (their
	// repo layout), overridable by flag. The builder writes artifacts in place under
	// vendorDir/templates/<name>/ via the storage provider.
	sd := *scriptsDir
	if sd == "" {
		sd = filepath.Join(filepath.Dir(*vendorDir), "scripts")
	}
	tmplBuilder := build.New(provider, *vendorDir, sd)

	// 1) gRPC SandboxService -- the lifecycle seam.
	lis, err := net.Listen("tcp", *grpcAddr)
	if err != nil {
		log.Fatal(err)
	}
	grpcServer := grpc.NewServer()
	pb.RegisterSandboxServiceServer(grpcServer, &sandboxService{srv: srv})
	pbtmpl.RegisterTemplateServiceServer(grpcServer, newTemplateService(tmplBuilder))

	// 2) HTTP data proxy -- the TCP bridge to the VM's NIC, routed by X-Sandbox-Id + Port. The "GET /health"
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
	log.Printf("orchestrator: gRPC on %s, data proxy on %s (vendor=%s, scripts=%s, pools=%v, storage=%s, uffd=%v)",
		*grpcAddr, *proxyAddr, *vendorDir, sd, poolSpecs, *storageMode, *useUffd)

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

// newStorageProvider builds the artifact store from the flags (Stage 15): "s3" connects to MinIO/S3
// (the default; creating the bucket if absent, which doubles as the readiness check), "local-fs"
// returns a nil provider so the fleet reads artifacts from --vendor-dir directly (the pre-Stage-15
// behavior, kept as an escape hatch). The build pipeline gets the same provider (nil => no upload).
func newStorageProvider(mode, endpoint, accessKey, secretKey, bucket string, ssl bool) (storage.StorageProvider, error) {
	switch mode {
	case "local-fs":
		return nil, nil
	case "s3":
		return storage.NewS3(context.Background(), endpoint, accessKey, secretKey, bucket, ssl)
	default:
		return nil, fmt.Errorf("--storage must be s3 or local-fs, got %q", mode)
	}
}
