// Command client-proxy is E2B's edge data proxy. It reads the sandbox routing catalog
// (pkg/catalog, a shared Redis since Stage 14a) and runs a single public data listener
// (--addr): every request is routed into a sandbox by its `<port>-<id>` Host header
// (Stage 12) -- look the id up in Redis, then reverse-proxy to that node's orchestrator data
// proxy (handing it the id + target port, which dials the VM's NIC over TCP -> envd). GET
// /health is its own liveness.
//
// Through Stage 13 client-proxy also ran an internal control port the api wrote routes to
// (PUT/DELETE /routes/{id}), because the catalog was an in-process map only this process
// could mutate. Stage 14a moves the catalog into Redis, which the api writes directly, so
// that control port and its handlers are gone -- net less code, and closer to E2B (which
// also routes off a shared Redis catalog). See docs/STAGE14_DESIGN.md / STAGE9_DESIGN.md.
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
	redisAddr := flag.String("redis-addr", "127.0.0.1:6379",
		"Redis address holding the sandbox routing catalog (read on every data request)")
	flag.Parse()

	cat := catalog.NewRedis(*redisAddr)
	defer cat.Close()
	s := &clientProxy{catalog: cat, proxy: newDataProxy()}

	// Public data plane: own liveness + the header-routed catch-all. "GET /health" is more
	// specific than "/", so client-proxy's own health is never treated as a data request.
	dataMux := http.NewServeMux()
	dataMux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	dataMux.HandleFunc("/", s.handleData)
	dataServer := &http.Server{Addr: *addr, Handler: dataMux}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = dataServer.Shutdown(ctx)
		os.Exit(0)
	}()

	log.Printf("client-proxy: data plane on %s, catalog on redis %s", *addr, *redisAddr)
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
