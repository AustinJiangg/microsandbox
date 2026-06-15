package main

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// These drive a real Python kernel through the gateway -- the live proof of the
// WebSocket kernel-driving path (7b). They need `jupyter` (kernel gateway + ipykernel)
// on PATH and auto-skip where it's absent, like the VM tests gate on KVM. Note: this
// needs Python + the gateway, NOT KVM, so it can run on any dev box with them.

func requireJupyter(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("jupyter"); err != nil {
		t.Skip("jupyter not on PATH; skipping live kernel integration test")
	}
}

func runCell(t *testing.T, km *kernelManager, code string) []OutputEvent {
	t.Helper()
	var evs []OutputEvent
	km.execute(context.Background(), code, "python", 30*time.Second, func(e OutputEvent) {
		evs = append(evs, e)
	})
	return evs
}

func lastExit(t *testing.T, evs []OutputEvent) int {
	t.Helper()
	if n := len(evs); n == 0 || evs[n-1].Type != evEnd || evs[n-1].ExitCode == nil {
		t.Fatalf("expected a terminal END event, got %+v", evs)
	}
	return *evs[len(evs)-1].ExitCode
}

func collectStdout(evs []OutputEvent) string {
	var b strings.Builder
	for _, e := range evs {
		if e.Type == evStdout {
			b.WriteString(e.Data)
		}
	}
	return b.String()
}

func TestKernelStatefulAndStreams(t *testing.T) {
	requireJupyter(t)
	km := newKernelManager()
	t.Cleanup(km.close)

	evs := runCell(t, km, "print('hello')")
	if got := collectStdout(evs); got != "hello\n" {
		t.Errorf("stdout = %q, want \"hello\\n\"", got)
	}
	if lastExit(t, evs) != 0 {
		t.Errorf("exit = %d, want 0", lastExit(t, evs))
	}

	// statefulness: a variable from one cell is visible in the next
	runCell(t, km, "x = 41")
	evs = runCell(t, km, "print(x + 1)")
	if got := collectStdout(evs); got != "42\n" {
		t.Errorf("stateful stdout = %q, want \"42\\n\"", got)
	}
}

func TestKernelErrorExitCode(t *testing.T) {
	requireJupyter(t)
	km := newKernelManager()
	t.Cleanup(km.close)

	evs := runCell(t, km, "raise ValueError('boom')")
	if lastExit(t, evs) != 1 {
		t.Errorf("exit = %d, want 1 (cell raised)", lastExit(t, evs))
	}
	sawErr := false
	for _, e := range evs {
		if e.Type == evStderr && strings.Contains(e.Data, "ValueError") {
			sawErr = true
		}
	}
	if !sawErr {
		t.Errorf("expected a stderr event carrying the traceback, got %+v", evs)
	}
}

func TestKernelTimeoutPreservesState(t *testing.T) {
	requireJupyter(t)
	km := newKernelManager()
	t.Cleanup(km.close)

	runCell(t, km, "y = 7")
	var evs []OutputEvent
	km.execute(context.Background(), "import time; time.sleep(30)", "python", 500*time.Millisecond, func(e OutputEvent) {
		evs = append(evs, e)
	})
	if lastExit(t, evs) != -1 {
		t.Errorf("exit = %d, want -1 (timeout)", lastExit(t, evs))
	}
	// the namespace survived the interrupt -- the point of interrupt-not-kill
	evs = runCell(t, km, "print(y)")
	if got := collectStdout(evs); got != "7\n" {
		t.Errorf("post-timeout stdout = %q, want \"7\\n\" (state preserved)", got)
	}
}
