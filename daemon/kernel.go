package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// kernelManager owns a stateful Python kernel, driven the way E2B does it: a headless
// Jupyter Kernel Gateway speaking the HTTP + WebSocket kernels API (Decision 3). It is
// the Go counterpart of backend.py's JupyterKernelBackend -- lazy start, one execution
// at a time (stateful REPL), translate iopub messages into OutputEvents, and
// interrupt-on-timeout so the namespace survives. See docs/STAGE7_DESIGN.md.
//
// The channels WebSocket is opened ONCE and read by a background goroutine, mirroring
// jupyter_client's persistent channels. A fresh WS per execution hits the ZMQ PUB/SUB
// "slow joiner" race -- the kernel's first outputs are published before the new iopub
// subscription propagates, so they are lost -- which is exactly why this is persistent.
type kernelManager struct {
	mu       sync.Mutex // serialize executions: the kernel runs one cell at a time
	started  bool
	proc     *exec.Cmd // the kernel gateway process
	baseURL  string    // http://127.0.0.1:<port>
	kernelID string
	httpc    *http.Client

	ctx    context.Context    // manager lifetime; bounds the WS + reader goroutine
	cancel context.CancelFunc // called by close()

	conn       *websocket.Conn // persistent channels WS (opened once in ensureStarted)
	msgs       chan kernelMsg  // reader goroutine -> the in-flight execution
	readerDone chan struct{}   // closed when the reader exits (WS error / shutdown)
}

func newKernelManager() *kernelManager {
	ctx, cancel := context.WithCancel(context.Background())
	return &kernelManager{
		httpc:      &http.Client{},
		ctx:        ctx,
		cancel:     cancel,
		msgs:       make(chan kernelMsg, 64),
		readerDone: make(chan struct{}),
	}
}

// execute runs one cell and streams OutputEvents to emit(); it blocks until the cell
// finishes (idle), times out, or errors. Mirrors backend.py.execute + _run_one: the
// lock serializes access to the shared kernel, and the kernel is started lazily on the
// first call (the few-seconds cold start is paid once).
func (k *kernelManager) execute(ctx context.Context, code, language string, timeout time.Duration, emit func(OutputEvent)) {
	if language != "python" {
		emit(OutputEvent{Type: evError, Data: "unsupported language: " + language})
		emit(endEvent(1))
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()

	if err := k.ensureStarted(); err != nil {
		emit(OutputEvent{Type: evError, Data: "kernel failed to start: " + err.Error()})
		emit(endEvent(1))
		return
	}
	k.runOne(ctx, code, timeout, emit)
}

// runOne sends one execute_request on the shared channels WS and translates the
// kernel's iopub replies (filtered to this request, drained from the background reader)
// into OutputEvents up to idle. On timeout it interrupts the kernel (SIGINT ->
// KeyboardInterrupt, keeping the namespace), drains the interrupted cell to idle so the
// next run starts clean, and reports exit_code -1; a clean run reports 0, or 1 if the
// cell raised.
func (k *kernelManager) runOne(execCtx context.Context, code string, timeout time.Duration, emit func(OutputEvent)) {
	msgID := newUUID()
	if err := wsjson.Write(k.ctx, k.conn, executeRequest(msgID, code)); err != nil {
		emit(OutputEvent{Type: evError, Data: "kernel send failed: " + err.Error()})
		emit(endEvent(1))
		return
	}

	// The timer covers only this cell, not the one-off kernel cold start above.
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	hadError := false
	for {
		select {
		case msg := <-k.msgs:
			if msg.Channel != "iopub" || msg.ParentHeader.MsgID != msgID {
				continue // other channels / other executions' leftovers
			}
			if msg.Header.MsgType == "error" {
				hadError = true
			}
			ev, doEmit, done := translate(msg.Header.MsgType, msg.content())
			if doEmit {
				emit(ev)
			}
			if done {
				exitCode := 0
				if hadError {
					exitCode = 1
				}
				emit(endEvent(exitCode))
				return
			}
		case <-timer.C:
			k.interrupt()           // keep the kernel + namespace alive; just stop this cell
			k.drainUntilIdle(msgID) // consume the interrupted cell's tail so the next run is clean
			emit(OutputEvent{Type: evError, Data: "execution timed out after " + secs(timeout) + "s"})
			emit(endEvent(-1))
			return
		case <-k.readerDone:
			emit(OutputEvent{Type: evError, Data: "kernel connection lost"})
			emit(endEvent(1))
			return
		case <-execCtx.Done():
			return // the client went away; stop streaming
		}
	}
}

// drainUntilIdle consumes this execution's leftover iopub messages up to its idle
// status, so an interrupt's trailing output doesn't bleed into the next execution.
// Port of backend.py._drain_until_idle (needed because the channels WS is shared).
func (k *kernelManager) drainUntilIdle(msgID string) {
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case msg := <-k.msgs:
			if msg.Channel == "iopub" && msg.ParentHeader.MsgID == msgID &&
				msg.Header.MsgType == "status" {
				if es, _ := msg.content()["execution_state"].(string); es == "idle" {
					return
				}
			}
		case <-timer.C:
			return // give up draining; msgID filtering tolerates leftovers anyway
		case <-k.readerDone:
			return
		}
	}
}

// ensureStarted launches the kernel gateway, creates one python3 kernel, opens the
// channels WebSocket, and starts the background reader -- all once.
func (k *kernelManager) ensureStarted() error {
	if k.started {
		return nil
	}
	port, err := freePort()
	if err != nil {
		return err
	}
	k.baseURL = fmt.Sprintf("http://127.0.0.1:%d", port)

	// A headless Jupyter Kernel Gateway: no notebook UI, just the /api/kernels HTTP +
	// WebSocket API. It owns the kernel's ZMQ channels; we speak its JSON-over-WebSocket
	// API, so Go needs no ZMQ.
	cmd := exec.Command("jupyter", "kernelgateway",
		"--KernelGatewayApp.ip=127.0.0.1",
		"--KernelGatewayApp.port="+strconv.Itoa(port),
		"--KernelGatewayApp.allow_origin=*",
	)
	cmd.Stdout = os.Stderr // gateway logs join the daemon's stderr (the guest console)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting kernel gateway: %w", err)
	}
	k.proc = cmd

	if err := k.waitGatewayReady(); err != nil {
		return err
	}
	id, err := k.createKernel()
	if err != nil {
		return err
	}
	k.kernelID = id

	wsURL := "ws" + strings.TrimPrefix(k.baseURL, "http") + "/api/kernels/" + id + "/channels"
	conn, _, err := websocket.Dial(k.ctx, wsURL, nil)
	if err != nil {
		return fmt.Errorf("connecting kernel channels: %w", err)
	}
	conn.SetReadLimit(64 << 20) // cell output can be large; don't cap at the 32KiB default
	k.conn = conn
	go k.readLoop()

	k.started = true
	return nil
}

// readLoop pumps every WebSocket message into k.msgs for the in-flight execution. One
// persistent reader keeps the iopub subscription stable (no slow-joiner loss).
func (k *kernelManager) readLoop() {
	defer close(k.readerDone)
	for {
		var msg kernelMsg
		if err := wsjson.Read(k.ctx, k.conn, &msg); err != nil {
			return // ctx cancelled (shutdown) or the WS failed
		}
		select {
		case k.msgs <- msg:
		case <-k.ctx.Done():
			return
		}
	}
}

func (k *kernelManager) waitGatewayReady() error {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequestWithContext(k.ctx, "GET", k.baseURL+"/api", nil)
		if resp, err := k.httpc.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		select {
		case <-k.ctx.Done():
			return k.ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("kernel gateway did not become ready in time")
}

func (k *kernelManager) createKernel() (string, error) {
	req, _ := http.NewRequestWithContext(k.ctx, "POST", k.baseURL+"/api/kernels",
		bytes.NewReader([]byte(`{"name":"python3"}`)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := k.httpc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("create kernel: HTTP %d", resp.StatusCode)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", fmt.Errorf("create kernel: empty id")
	}
	return out.ID, nil
}

// interrupt asks the gateway to SIGINT the kernel -- the cell raises KeyboardInterrupt
// but the kernel and its namespace live on (the value of being stateful).
func (k *kernelManager) interrupt() {
	req, _ := http.NewRequestWithContext(k.ctx, "POST", k.baseURL+"/api/kernels/"+k.kernelID+"/interrupt", nil)
	if resp, err := k.httpc.Do(req); err == nil {
		resp.Body.Close()
	}
}

// close stops the reader and kills the gateway (and with it the kernel). The daemon
// normally lives as long as the VM, so this is for shutdown / tests.
func (k *kernelManager) close() {
	k.cancel()
	if k.conn != nil {
		k.conn.Close(websocket.StatusNormalClosure, "")
	}
	if k.proc != nil && k.proc.Process != nil {
		_ = k.proc.Process.Kill()
		_, _ = k.proc.Process.Wait()
	}
}

// kernelMsg is the subset of a Jupyter WebSocket message we need: which channel it came
// on, its type, the request it answers, and the raw content (decoded lazily).
type kernelMsg struct {
	Channel string `json:"channel"`
	Header  struct {
		MsgType string `json:"msg_type"`
	} `json:"header"`
	ParentHeader struct {
		MsgID string `json:"msg_id"`
	} `json:"parent_header"`
	Content json.RawMessage `json:"content"`
}

func (m kernelMsg) content() map[string]any {
	var c map[string]any
	_ = json.Unmarshal(m.Content, &c)
	return c
}

// executeRequest builds a Jupyter execute_request message (the same fields
// jupyter_client sends): store_history true so the REPL keeps history, allow_stdin
// false since the sandbox is non-interactive.
func executeRequest(msgID, code string) map[string]any {
	return map[string]any{
		"header": map[string]any{
			"msg_id": msgID, "session": newUUID(), "username": "microsandbox",
			"msg_type": "execute_request", "version": "5.3",
		},
		"parent_header": map[string]any{},
		"metadata":      map[string]any{},
		"content": map[string]any{
			"code": code, "silent": false, "store_history": true,
			"user_expressions": map[string]any{}, "allow_stdin": false, "stop_on_error": true,
		},
		"channel": "shell",
	}
}

func newUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// secs formats a duration's seconds the way Python's str(float) does (keeping a
// trailing ".0" for whole numbers), so the timeout message matches the Python daemon.
func secs(d time.Duration) string {
	s := d.Seconds()
	if s == float64(int64(s)) {
		return strconv.FormatFloat(s, 'f', 1, 64)
	}
	return strconv.FormatFloat(s, 'g', -1, 64)
}
