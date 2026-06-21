// Command client-proxy is E2B's edge data proxy. It owns the sandbox routing catalog
// (pkg/catalog, in-memory) and runs two listeners:
//
//   - a public data port (--addr): every request is routed into a sandbox by its
//     `<port>-<id>` Host header (Stage 12) -- look the id up in the catalog, then
//     reverse-proxy to that node's orchestrator data proxy (handing it the id + target
//     port, which dials the VM's NIC over TCP -> envd). GET /health is its own liveness.
//   - an internal control port (--internal-addr): the api writes the catalog here
//     (PUT/DELETE /routes/{id}) when sandboxes are created/destroyed. Kept off the public
//     port so the routing table is not writable by data-plane clients.
//
// This is the role the api's temporary passthrough played in Stage 8; Stage 9 moves the
// data plane off the api and onto client-proxy. See docs/STAGE9_DESIGN.md.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"net/http/httputil"
	"os"
	"os/signal"
	"syscall"
	"time"

	"microsandbox/services/pkg/catalog"
)

// clientProxy bundles the routing catalog and the shared reverse proxy the handlers use.
type clientProxy struct {
	catalog catalog.Catalog
	proxy   *httputil.ReverseProxy
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8081",
		"host:port for the public data plane (the SDK's data_url)")
	internalAddr := flag.String("internal-addr", "127.0.0.1:5008",
		"host:port for the internal catalog control endpoints (the api writes routes here)")
	flag.Parse()

	s := &clientProxy{catalog: catalog.NewInMemory(), proxy: newDataProxy()}

	// Public data plane: own liveness + the header-routed catch-all. "GET /health" is more
	// specific than "/", so client-proxy's own health is never treated as a data request.
	dataMux := http.NewServeMux()
	dataMux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	dataMux.HandleFunc("/", s.handleData)
	dataServer := &http.Server{Addr: *addr, Handler: dataMux}

	// Internal control plane: the api writes the catalog here on create/destroy.
	internalMux := http.NewServeMux()
	internalMux.HandleFunc("PUT /routes/{id}", s.handleRouteSet)
	internalMux.HandleFunc("DELETE /routes/{id}", s.handleRouteDelete)
	internalServer := &http.Server{Addr: *internalAddr, Handler: internalMux}

	go func() {
		if err := internalServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("internal control serve: %v", err)
		}
	}()
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = dataServer.Shutdown(ctx)
		_ = internalServer.Shutdown(ctx)
		os.Exit(0)
	}()

	log.Printf("client-proxy: data plane on %s, internal control on %s", *addr, *internalAddr)
	if err := dataServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

// writeJSON mirrors the helper in the api / orchestrator -- a small JSON body with a status.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
