package main

import "encoding/json"

// OutputEvent mirrors protocol.py's OutputEvent: one streamed SSE event, daemon ->
// client. The SSE bytes must match protocol.py.to_sse() exactly, since the SDK parses
// them line by line. ExitCode is a *int so it is emitted only for the END event:
// protocol.py includes "exit_code" only when it is not None, and 0 is a real END
// value, so a pointer with omitempty (nil omits, &0 includes) reproduces that exactly.
type OutputEvent struct {
	Type     string `json:"type"`
	Data     string `json:"data"`
	ExitCode *int   `json:"exit_code,omitempty"`
}

// Event types, matching protocol.py's EventType values.
const (
	evStdout = "stdout"
	evStderr = "stderr"
	evError  = "error"
	evEnd    = "end"
)

// sse renders the event as one SSE frame, byte-identical to protocol.py.to_sse():
//
//	data: {"type":"...","data":"..."[,"exit_code":N]}\n\n
func (e OutputEvent) sse() string {
	b, _ := json.Marshal(e)
	return "data: " + string(b) + "\n\n"
}

func endEvent(code int) OutputEvent { return OutputEvent{Type: evEnd, ExitCode: &code} }
