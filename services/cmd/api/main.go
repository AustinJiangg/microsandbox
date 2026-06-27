// Command api is the public REST front of the control plane (E2B's `api`), and as of
// Stage 9c it is lifecycle-only: it owns no VMs and never touches the data path. Sandbox
// lifecycle (POST/DELETE/GET /sandboxes) is delegated to the orchestrator over gRPC, the
// durable record of which sandboxes exist is kept in a metadata store (Postgres by default
// since Stage 14b, matching E2B; SQLite still selectable via a sqlite:// DSN), and on create
// the api registers the sandbox's data route in the
// catalog (a shared Redis since Stage 14a, which client-proxy reads to route data; before
// 14a the api wrote it over an internal RPC to client-proxy). It returns the data_url the
// SDK posts the data path to. The data plane goes SDK -> client-proxy -> orchestrator ->
// the VM's NIC (TCP) -> envd, never through here. See docs/STAGE14_DESIGN.md / STAGE9_DESIGN.md.
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

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"microsandbox/services/pkg/catalog"
	pb "microsandbox/services/pkg/grpc/orchestrator"
	pbt "microsandbox/services/pkg/grpc/templatemanager"
	"microsandbox/services/pkg/store"
)

// api holds the gRPC client to the orchestrator (lifecycle), the metadata store (durable
// record), the catalog (writes each sandbox's data-path route to the shared Redis that
// client-proxy reads), and the node value it registers. As of Stage 9c the api is
// lifecycle-only -- it no longer proxies the data path.
type api struct {
	client    pb.SandboxServiceClient
	templates pbt.TemplateServiceClient
	store     store.Store
	catalog   catalog.Catalog
	nodeAddr  string // the node (orchestrator data-proxy addr) registered for each sandbox
	dataURL   string // the public client-proxy data URL handed back to the SDK (where to send data)
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "host:port for the public REST API (the SDK's base URL)")
	orchGRPC := flag.String("orchestrator-grpc", "127.0.0.1:9090", "orchestrator gRPC address (SandboxService)")
	orchProxy := flag.String("orchestrator-proxy", "127.0.0.1:5007", "orchestrator data-proxy address: the node value registered in the catalog for each sandbox")
	redisAddr := flag.String("redis-addr", "127.0.0.1:6379", "Redis address holding the sandbox routing catalog (the api writes routes here on create/destroy)")
	dataURL := flag.String("data-url", "http://127.0.0.1:8081", "public client-proxy data URL returned to the SDK as where to send the data path")
	storeDSN := flag.String("store-dsn", "postgres://postgres@127.0.0.1:5432/microsandbox?sslmode=disable",
		"metadata store DSN: postgres://… (default, Stage 14b) or sqlite://<path> (or a bare path) for the single-file backend")
	flag.Parse()

	// gRPC client to the orchestrator. NewClient is lazy (it connects on the first RPC,
	// not here) and plaintext (insecure creds), like E2B on-cluster -- TLS/auth is out
	// of scope for this learning project.
	conn, err := grpc.NewClient(*orchGRPC, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	st, err := store.Open(*storeDSN)
	if err != nil {
		log.Fatalf("open metadata store %s: %v", *storeDSN, err)
	}
	defer st.Close()

	// The routing catalog: a shared Redis the api writes (here) and client-proxy reads.
	cat := catalog.NewRedis(*redisAddr)
	defer cat.Close()

	a := &api{
		client:    pb.NewSandboxServiceClient(conn),
		templates: pbt.NewTemplateServiceClient(conn),
		store:     st,
		catalog:   cat,
		nodeAddr:  *orchProxy,
		dataURL:   *dataURL,
	}

	// Lifecycle-only routes (Stage 9c): the data path lives on client-proxy now. A request
	// to /sandboxes/{id}/<anything> matches none of these and gets a 404, which is correct
	// -- the SDK no longer sends the data path here.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", a.handleHealth)
	mux.HandleFunc("POST /sandboxes", a.handleCreate)
	mux.HandleFunc("DELETE /sandboxes/{id}", a.handleDestroy)
	mux.HandleFunc("GET /sandboxes", a.handleList)
	// Template builds (Stage 10): create kicks an async build in the orchestrator; the SDK
	// polls the build status; list is the api's durable record.
	mux.HandleFunc("POST /templates", a.handleTemplateCreate)
	mux.HandleFunc("GET /templates", a.handleTemplateList)
	mux.HandleFunc("GET /templates/builds/{id}", a.handleTemplateBuildStatus)

	httpServer := &http.Server{Addr: *addr, Handler: mux}
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(ctx)
		os.Exit(0)
	}()

	log.Printf("api listening on %s (orchestrator grpc=%s proxy=%s, redis=%s, data-url=%s, store=%s)",
		*addr, *orchGRPC, *orchProxy, *redisAddr, *dataURL, *storeDSN)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
