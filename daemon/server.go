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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

// newMux wires the daemon's HTTP routes. Kept separate from the listener so tests
// can drive it over an httptest TCP server -- no vsock, no VM (server_test.go).
func newMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("POST /files/read", handleFileRead)
	mux.HandleFunc("POST /files/write", handleFileWrite)
	mux.HandleFunc("POST /files/list", handleFileList)
	mux.HandleFunc("POST /commands", handleCommand)
	return mux
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleFileRead: POST /files/read <- {"path"} -> {"content"}; missing/unreadable -> 404.
func handleFileRead(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if !decode(w, r, &req) {
		return
	}
	data, err := os.ReadFile(req.Path)
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"content": string(data)})
}

// handleFileWrite: POST /files/write <- {"path","content"} -> {"ok":true}. Like
// server.py it creates intermediate dirs; a read-only root (writing outside /tmp)
// surfaces as 400.
func handleFileWrite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := os.MkdirAll(filepath.Dir(req.Path), 0o755); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := os.WriteFile(req.Path, []byte(req.Content), 0o644); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleFileList: POST /files/list <- {"path"} -> {"entries":[{"name","is_dir"}]}.
func handleFileList(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if !decode(w, r, &req) {
		return
	}
	dirents, err := os.ReadDir(req.Path)
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
		entries = append(entries, entry{Name: d.Name(), IsDir: d.IsDir()})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}

// handleCommand: POST /commands <- {"command","timeout_seconds"} ->
// {"stdout","stderr","exit_code"}. Runs via `sh -c` in the daemon's own environment
// (= inside the VM). On timeout the process is killed and a fixed message returned
// with exit_code -1, mirroring server.py.
func handleCommand(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Command        string  `json:"command"`
		TimeoutSeconds float64 `json:"timeout_seconds"`
	}
	if !decode(w, r, &req) {
		return
	}
	timeout := req.TimeoutSeconds
	if timeout <= 0 {
		timeout = 30
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout*float64(time.Second)))
	defer cancel()

	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", req.Command)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()

	if ctx.Err() == context.DeadlineExceeded {
		writeJSON(w, http.StatusOK, map[string]any{
			"stdout":    "",
			"stderr":    "command timed out after " + strconv.FormatFloat(timeout, 'g', -1, 64) + "s",
			"exit_code": -1,
		})
		return
	}
	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	} else if err != nil {
		exitCode = -1 // failed to start (e.g. /bin/sh missing)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"stdout":    stdout.String(),
		"stderr":    stderr.String(),
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
