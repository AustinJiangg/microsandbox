package main

import "testing"

// translate + the SSE rendering are pure, so they pin the /execute byte contract
// (the mapping ported from backend.py._translate, and the exact `data: {...}` frame)
// with no kernel, no VM.

func TestTranslate(t *testing.T) {
	cases := []struct {
		name     string
		msgType  string
		content  map[string]any
		wantType string
		wantData string
		wantEmit bool
		wantDone bool
	}{
		{"stdout", "stream", map[string]any{"name": "stdout", "text": "hi"}, evStdout, "hi", true, false},
		{"stderr", "stream", map[string]any{"name": "stderr", "text": "oops"}, evStderr, "oops", true, false},
		{"result", "execute_result", map[string]any{"data": map[string]any{"text/plain": "42"}}, evStdout, "42\n", true, false},
		{"empty result", "execute_result", map[string]any{"data": map[string]any{}}, "", "", false, false},
		{"idle done", "status", map[string]any{"execution_state": "idle"}, "", "", false, true},
		{"busy skipped", "status", map[string]any{"execution_state": "busy"}, "", "", false, false},
		{"execute_input skipped", "execute_input", map[string]any{}, "", "", false, false},
	}
	for _, c := range cases {
		ev, emit, done := translate(c.msgType, c.content)
		if emit != c.wantEmit || done != c.wantDone {
			t.Errorf("%s: emit=%v done=%v, want emit=%v done=%v", c.name, emit, done, c.wantEmit, c.wantDone)
		}
		if emit && (ev.Type != c.wantType || ev.Data != c.wantData) {
			t.Errorf("%s: event=%+v, want type=%q data=%q", c.name, ev, c.wantType, c.wantData)
		}
	}
}

func TestTranslateErrorStripsANSI(t *testing.T) {
	content := map[string]any{"traceback": []any{"\x1b[0;31mTraceback\x1b[0m", "ValueError: x"}}
	ev, emit, _ := translate("error", content)
	if !emit || ev.Type != evStderr {
		t.Fatalf("emit=%v type=%q", emit, ev.Type)
	}
	if ev.Data != "Traceback\nValueError: x\n" {
		t.Errorf("data = %q, want ANSI-stripped joined traceback", ev.Data)
	}
}

func TestSSEFraming(t *testing.T) {
	if got := (OutputEvent{Type: evStdout, Data: "hi"}).sse(); got != "data: {\"type\":\"stdout\",\"data\":\"hi\"}\n\n" {
		t.Errorf("stdout sse = %q", got)
	}
	// END carries exit_code (even 0); other events omit it.
	if got := endEvent(0).sse(); got != "data: {\"type\":\"end\",\"data\":\"\",\"exit_code\":0}\n\n" {
		t.Errorf("end sse = %q", got)
	}
}
