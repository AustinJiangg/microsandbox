// Package main is the in-VM daemon (Stage 7): a Go rewrite of the Python
// server.py + backend.py, matching E2B's envd. It listens on vsock and serves the
// exact same HTTP/SSE protocol the Python daemon did (protocol.py is unchanged).
//
// This file is the 7a slice -- /health, /files/*, /commands -- all served with
// stdlib net/http (a real simplification over server.py's hand-rolled HTTP parser).
// /execute, which drives the stateful Python kernel over the Jupyter WebSocket API,
// arrives in 7b. See docs/STAGE7_DESIGN.md.
package main

import (
	"encoding/json"
	"net/http"
)

// newMux wires the daemon's HTTP routes. Kept separate from the listener so tests
// can drive it over an httptest TCP server -- no vsock, no VM (server_test.go).
func newMux(km *kernelManager) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("POST /execute", km.handleExecute)
	mux.HandleFunc("POST /files/read", handleFileRead)
	mux.HandleFunc("POST /files/write", handleFileWrite)
	mux.HandleFunc("POST /files/list", handleFileList)
	mux.HandleFunc("POST /commands", handleCommand)
	// Stage 11a: the ConnectRPC envd services (Filesystem + Process), mounted alongside the
	// HTTP endpoints above. Nothing routes to them yet -- the SDK flips in 11c, and the HTTP
	// endpoints are removed then. The connect paths don't collide with the HTTP routes.
	registerEnvdServices(mux)
	return mux
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleFileRead: POST /files/read <- {"path"} -> {"content"}; missing/unreadable -> 404.
// The filesystem/process logic lives in envd.go's helpers, shared with the ConnectRPC
// services so the HTTP and Connect paths cannot drift while both exist (Stage 11a).
func handleFileRead(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if !decode(w, r, &req) {
		return
	}
	content, err := readFile(req.Path)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"content": content})
}

// handleFileWrite: POST /files/write <- {"path","content"} -> {"ok":true}. Creates
// intermediate dirs; a read-only root (writing outside /tmp) surfaces as 400.
func handleFileWrite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := writeFile(req.Path, req.Content); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleFileList: POST /files/list <- {"path"} -> {"entries":[{"name","is_dir"}]} (sorted).
func handleFileList(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if !decode(w, r, &req) {
		return
	}
	dirents, err := listDir(req.Path)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	type entry struct {
		Name  string `json:"name"`
		IsDir bool   `json:"is_dir"`
	}
	entries := make([]entry, 0, len(dirents))
	for _, d := range dirents {
		entries = append(entries, entry{Name: d.Name, IsDir: d.IsDir})
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

// handleCommand: POST /commands <- {"command","timeout_seconds"} ->
// {"stdout","stderr","exit_code"}. Runs via `sh -c` inside the VM; on timeout returns a
// fixed message with exit_code -1 (mirroring server.py). See runCommand in envd.go.
func handleCommand(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Command        string  `json:"command"`
		TimeoutSeconds float64 `json:"timeout_seconds"`
	}
	if !decode(w, r, &req) {
		return
	}
	stdout, stderr, exitCode := runCommand(req.Command, req.TimeoutSeconds)
	writeJSON(w, http.StatusOK, map[string]any{
		"stdout":    stdout,
		"stderr":    stderr,
		"exit_code": exitCode,
	})
}

// ----- small HTTP/JSON helpers (the daemon speaks JSON everywhere except /execute's SSE) -----

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request: " + err.Error()})
		return false
	}
	return true
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// writeJSON marshals with json.Marshal (not Encoder.Encode) to avoid a trailing
// newline, so the body matches server.py's byte-for-byte.
func writeJSON(w http.ResponseWriter, status int, body any) {
	data, _ := json.Marshal(body)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
