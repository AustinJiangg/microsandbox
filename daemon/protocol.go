package main

// OutputEvent is the daemon's internal representation of one streamed kernel output
// (stdout / stderr / error / end). As of Stage 11 the code-interpreter service
// (codeinterpreter.go) maps each into a Connect stream frame; ExitCode is a *int so the
// "end" event can carry a real 0 while the others carry none.
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

func endEvent(code int) OutputEvent { return OutputEvent{Type: evEnd, ExitCode: &code} }
