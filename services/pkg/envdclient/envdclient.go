// Package envdclient is a tiny Connect-JSON client for the in-VM daemon's ProcessService (Stage 22).
//
// The orchestrator's layer producer (docs/STAGE22_DESIGN.md) resumes the base VM and must run the
// layer's build command *inside the guest* -- so the one re-snapshot that follows captures a mutually
// consistent RAM (page cache) + disk (writable overlay) pair, unlike the Stage-20 producer that
// grafted a separately-built rootfs onto the base's RAM. To reach envd's ProcessService.Run over the
// VM's NIC, we speak Connect unary with the JSON codec: a plain POST of the JSON message, a JSON reply
// (connectrpc.com). This is the Go twin of the Python SDK's src/microsandbox/connect.py -- hand-rolled
// over stdlib net/http so the orchestrator (module microsandbox/services) needs neither a protobuf
// runtime nor a cross-module import of the daemon's genpb, matching the SDK's zero-new-dep discipline.
package envdclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// runProcedure is envd's ProcessService.Run, the same procedure path connect-go mounts in the daemon
// (daemon/envd.go registerEnvdServices) and the Python SDK POSTs to.
const runProcedure = "/envd.ProcessService/Run"

// RunResult is the outcome of an in-guest command (envd ProcessService.Run). A zero ExitCode is
// success; the caller decides what a non-zero code means (e.g. a failed build step). ExitCode is -1
// on a daemon-side timeout / failed-to-start, mirroring the daemon (daemon/proto/envd/envd.proto).
type RunResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// Client runs commands inside one VM's guest via envd. BaseURL is the daemon's envd endpoint reached
// over the VM's NIC, e.g. "http://10.0.4.2:49983". One Client is cheap; the producer holds one per VM.
type Client struct {
	HTTP    *http.Client
	BaseURL string
}

// New builds a Client for baseURL (e.g. "http://<slot-ip>:49983") over http.DefaultClient. The default
// client has no timeout, which is intended: a build step's own duration is bounded by the daemon's
// per-command timeout (RunRequest.timeout_seconds), not by the HTTP client cutting the request off.
func New(baseURL string) *Client { return &Client{HTTP: http.DefaultClient, BaseURL: baseURL} }

// runRequest / runResponse mirror envd.RunRequest / envd.RunResponse in Connect's canonical JSON
// (lowerCamelCase field names). proto3 JSON omits zero-valued fields, so a successful no-output command
// (e.g. `sync`) comes back as `{}` and decodes to the zero RunResult -- ExitCode 0, empty output.
type runRequest struct {
	Command        string  `json:"command"`
	TimeoutSeconds float64 `json:"timeoutSeconds"`
}

type runResponse struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exitCode"`
}

// connectError is a Connect unary error body ({code, message}) returned on a non-2xx response.
type connectError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Run executes command inside the guest and returns its stdout/stderr/exit code. timeoutSeconds == 0
// lets the daemon apply its default (30s). An unreachable daemon or a non-2xx (a Connect error) is an
// error; a completed command with a non-zero exit code is NOT an error (the caller inspects ExitCode).
func (c *Client) Run(ctx context.Context, command string, timeoutSeconds float64) (RunResult, error) {
	body, err := json.Marshal(runRequest{Command: command, TimeoutSeconds: timeoutSeconds})
	if err != nil {
		return RunResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+runProcedure, bytes.NewReader(body))
	if err != nil {
		return RunResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return RunResult{}, fmt.Errorf("envd Run at %s: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return RunResult{}, fmt.Errorf("envd Run at %s: read body: %w", c.BaseURL, err)
	}
	if resp.StatusCode != http.StatusOK {
		return RunResult{}, fmt.Errorf("envd Run at %s: %s", c.BaseURL, errorDetail(raw, resp.Status))
	}
	var out runResponse
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return RunResult{}, fmt.Errorf("envd Run at %s: decode response: %w", c.BaseURL, err)
		}
	}
	return RunResult{Stdout: out.Stdout, Stderr: out.Stderr, ExitCode: out.ExitCode}, nil
}

// errorDetail pulls a human message out of a non-2xx body (a Connect error {code, message}), falling
// back to the raw body or the HTTP status line if it is not the expected shape.
func errorDetail(raw []byte, status string) string {
	var ce connectError
	if err := json.Unmarshal(raw, &ce); err == nil && ce.Message != "" {
		return ce.Message
	}
	if len(raw) > 0 {
		return string(raw)
	}
	return status
}
