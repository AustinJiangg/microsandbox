// Command api is the public REST front of the control plane (E2B's `api`). It owns no
// VMs: sandbox lifecycle (POST/DELETE/GET /sandboxes) is delegated to the orchestrator
// over gRPC, the durable record of which sandboxes exist is kept in a metadata store
// (SQLite, Stage 8c -- E2B uses Postgres), and the data path (/sandboxes/{id}/...) is
// -- for Stage 8 only -- reverse-proxied to the orchestrator's data proxy. The SDK talks
// only to this service, so its base URL is unchanged across the split. Stage 9
// introduces client-proxy and the SDK sends the data path there directly, retiring the
// passthrough. See docs/STAGE8_DESIGN.md.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"net/http/httputil"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "microsandbox/services/pkg/grpc/orchestrator"
	"microsandbox/services/pkg/store"
)

// api holds the gRPC client to the orchestrator (lifecycle), the metadata store
// (durable record), the catalog client (registers each sandbox's data-path route in
// client-proxy), the node value it registers, and -- temporarily, Stage 8/9a -- the
// reverse proxy used for the data-path passthrough (removed in Stage 9c).
type api struct {
	client    pb.SandboxServiceClient
	store     *store.Store
	catalog   *catalogClient
	nodeAddr  string // the node (orchestrator data-proxy addr) registered for each sandbox
	dataProxy *httputil.ReverseProxy
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "host:port for the public REST API (the SDK's base URL)")
	orchGRPC := flag.String("orchestrator-grpc", "127.0.0.1:9090", "orchestrator gRPC address (SandboxService)")
	orchProxy := flag.String("orchestrator-proxy", "127.0.0.1:5007", "orchestrator data-proxy address: the node value registered in the catalog (and, until Stage 9c, the passthrough target)")
	clientProxyInternal := flag.String("client-proxy-internal", "127.0.0.1:5008", "client-proxy internal control address (the api writes sandbox routes here)")
	db := flag.String("db", "vendor/microsandbox.db", "path to the SQLite metadata database")
	flag.Parse()

	// gRPC client to the orchestrator. NewClient is lazy (it connects on the first RPC,
	// not here) and plaintext (insecure creds), like E2B on-cluster -- TLS/auth is out
	// of scope for this learning project.
	conn, err := grpc.NewClient(*orchGRPC, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	st, err := store.Open(*db)
	if err != nil {
		log.Fatalf("open metadata db %s: %v", *db, err)
	}
	defer st.Close()

	a := &api{
		client:   pb.NewSandboxServiceClient(conn),
		store:    st,
		catalog:  newCatalogClient(*clientProxyInternal),
		nodeAddr: *orchProxy,
		// TEMPORARY (Stage 8): reverse-proxy the data path to the orchestrator's data
		// proxy, tagging each request with X-Sandbox-Id (which the orchestrator routes
		// on). Stage 9 replaces this with client-proxy and removes it. One proxy is
		// reused for all requests; the per-request id/rest come from the matched route
		// via pr.In.PathValue.
		dataProxy: &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.Out.URL.Scheme = "http"
				pr.Out.URL.Host = *orchProxy
				pr.Out.URL.Path = "/" + pr.In.PathValue("rest")
				pr.Out.URL.RawQuery = ""
				pr.Out.Host = *orchProxy
				pr.Out.Header.Set("X-Sandbox-Id", pr.In.PathValue("id"))
			},
			FlushInterval: -1, // flush every write so the daemon's SSE (/execute) streams live
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", a.handleHealth)
	mux.HandleFunc("POST /sandboxes", a.handleCreate)
	mux.HandleFunc("DELETE /sandboxes/{id}", a.handleDestroy)
	mux.HandleFunc("GET /sandboxes", a.handleList)
	mux.HandleFunc("/sandboxes/{id}/{rest...}", a.handleProxy)

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

	log.Printf("api listening on %s (orchestrator grpc=%s proxy=%s, client-proxy-internal=%s, db=%s)",
		*addr, *orchGRPC, *orchProxy, *clientProxyInternal, *db)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
