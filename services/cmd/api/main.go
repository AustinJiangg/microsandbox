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
	"microsandbox/services/pkg/placement"
	"microsandbox/services/pkg/store"
)

// api holds the orchestrator fleet (a placement.Registry picking a node per create, Stage 23 --
// before this it was a single gRPC client + a single node address), the template-build client
// (routed to one designated node), the metadata store (durable record), and the catalog (writes
// each sandbox's data-path route to the shared Redis that client-proxy reads). As of Stage 9c
// the api is lifecycle-only -- it never proxies the data path.
type api struct {
	registry  *placement.Registry // the orchestrator fleet + BestOfK placement (Stage 23)
	templates pbt.TemplateServiceClient
	store     store.Store
	catalog   catalog.Catalog
	dataURL   string // the public client-proxy data URL handed back to the SDK (where to send data)
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "host:port for the public REST API (the SDK's base URL)")
	orchGRPC := flag.String("orchestrator-grpc", "127.0.0.1:9090", "single-node fallback: orchestrator gRPC address (SandboxService) when --nodes is empty")
	orchProxy := flag.String("orchestrator-proxy", "127.0.0.1:5007", "single-node fallback: orchestrator data-proxy address (the catalog Route.Node) when --nodes is empty")
	nodesFlag := flag.String("nodes", "", "orchestrator fleet as comma-separated grpc@proxy entries (Stage 23 multi-host); empty falls back to the single --orchestrator-grpc/--orchestrator-proxy node")
	redisAddr := flag.String("redis-addr", "127.0.0.1:6379", "Redis address holding the sandbox routing catalog (the api writes routes here on create/destroy)")
	dataURL := flag.String("data-url", "http://127.0.0.1:8081", "public client-proxy data URL returned to the SDK as where to send the data path")
	storeDSN := flag.String("store-dsn", "postgres://postgres@127.0.0.1:5432/microsandbox?sslmode=disable",
		"metadata store DSN: postgres://… (default, Stage 14b) or sqlite://<path> (or a bare path) for the single-file backend")
	seedAPIKeys := flag.String("seed-api-keys", "msb_dev_key=default",
		"comma-separated key=team pairs to seed on startup (a bare key maps to team 'default'); empty seeds nothing (Stage 16)")
	flag.Parse()

	// Build the orchestrator fleet from --nodes (Stage 23), or the single legacy node when it
	// is empty. Each node gets its own gRPC client (NewClient is lazy -- it connects on the
	// first RPC, not here -- and plaintext, like E2B on-cluster; TLS/auth is out of scope).
	specs, err := parseNodeSpecs(*nodesFlag, *orchGRPC, *orchProxy)
	if err != nil {
		log.Fatal(err)
	}
	var nodes []*placement.Node
	var conns []*grpc.ClientConn
	for _, sp := range specs {
		conn, err := grpc.NewClient(sp.GRPC, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			log.Fatalf("dial orchestrator %s: %v", sp.GRPC, err)
		}
		conns = append(conns, conn)
		nodes = append(nodes, placement.NewNode(sp.GRPC, sp.Proxy, pb.NewSandboxServiceClient(conn), placement.DefaultCapacity))
	}
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	// The registry keeps each node's cached load + readiness fresh (Start primes it once
	// synchronously, then polls List ~1s) and picks a node per create via BestOfK.
	registry := placement.NewRegistry(nodes, placement.DefaultK)
	registry.Start()
	defer registry.Stop()

	st, err := store.Open(*storeDSN)
	if err != nil {
		log.Fatalf("open metadata store %s: %v", *storeDSN, err)
	}
	defer st.Close()

	// The routing catalog: a shared Redis the api writes (here) and client-proxy reads.
	cat := catalog.NewRedis(*redisAddr)
	defer cat.Close()

	a := &api{
		registry: registry,
		// Template builds route to one designated node (node[0]): artifacts land in shared
		// object storage keyed by build id (Stage 15), so any node restores from any build.
		templates: pbt.NewTemplateServiceClient(conns[0]),
		store:     st,
		catalog:   cat,
		dataURL:   *dataURL,
	}

	// Stage 16: seed the configured API keys (default a well-known dev key -> default team) so
	// local use works out of the box. Idempotent, so it is safe on every startup.
	if err := a.seedAPIKeys(*seedAPIKeys); err != nil {
		log.Fatalf("seed api keys: %v", err)
	}

	// Lifecycle-only routes (Stage 9c): the data path lives on client-proxy now. A request
	// to /sandboxes/{id}/<anything> matches none of these and gets a 404, which is correct
	// -- the SDK no longer sends the data path here. Every route except /health is wrapped in
	// withAuth (Stage 16): it requires an X-API-Key that resolves to a team and scopes the
	// handler to it. /health stays open so the test fixture / a load balancer can probe it.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", a.handleHealth)
	mux.HandleFunc("POST /sandboxes", a.withAuth(a.handleCreate))
	mux.HandleFunc("DELETE /sandboxes/{id}", a.withAuth(a.handleDestroy))
	mux.HandleFunc("GET /sandboxes", a.withAuth(a.handleList))
	// Template builds (Stage 10): create kicks an async build in the orchestrator; the SDK
	// polls the build status; list is the api's durable record.
	mux.HandleFunc("POST /templates", a.withAuth(a.handleTemplateCreate))
	mux.HandleFunc("GET /templates", a.withAuth(a.handleTemplateList))
	mux.HandleFunc("GET /templates/builds/{id}", a.withAuth(a.handleTemplateBuildStatus))

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

	log.Printf("api listening on %s (%d orchestrator node(s), redis=%s, data-url=%s, store=%s)",
		*addr, len(nodes), *redisAddr, *dataURL, *storeDSN)
	if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
