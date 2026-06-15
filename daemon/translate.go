package main

import (
	"regexp"
	"strings"
)

// IPython tracebacks carry ANSI color codes; strip them before they land in our
// plain-text stderr (same as backend.py._strip_ansi).
var ansiRE = regexp.MustCompile("\x1b\\[[0-9;]*m")

// translate maps one kernel iopub message to an OutputEvent. It returns
// (event, emit, done): emit=false means "nothing to forward for this message",
// done=true means "the kernel is back to idle, this execution is finished". It is a
// pure function (no I/O), a direct port of backend.py._translate, so it is unit-tested
// without a kernel.
func translate(msgType string, content map[string]any) (OutputEvent, bool, bool) {
	switch msgType {
	case "stream":
		name, _ := content["name"].(string)
		text, _ := content["text"].(string)
		t := evStdout
		if name != "stdout" {
			t = evStderr
		}
		return OutputEvent{Type: t, Data: text}, true, false
	case "execute_result", "display_data":
		// The value of an expression (REPL echo). Folded into stdout, so we don't add
		// a new event type to the /execute protocol.
		text := textPlain(content)
		if text == "" {
			return OutputEvent{}, false, false
		}
		return OutputEvent{Type: evStdout, Data: text + "\n"}, true, false
	case "error":
		tb := ansiRE.ReplaceAllString(joinTraceback(content), "")
		return OutputEvent{Type: evStderr, Data: tb + "\n"}, true, false
	case "status":
		if es, _ := content["execution_state"].(string); es == "idle" {
			return OutputEvent{}, false, true
		}
	}
	return OutputEvent{}, false, false // execute_input / busy / etc. -- nothing to forward
}

// textPlain pulls content.data["text/plain"] (the plain-text mimetype) out of an
// execute_result / display_data message.
func textPlain(content map[string]any) string {
	data, _ := content["data"].(map[string]any)
	s, _ := data["text/plain"].(string)
	return s
}

// joinTraceback joins an error message's traceback frames with newlines.
func joinTraceback(content map[string]any) string {
	raw, _ := content["traceback"].([]any)
	frames := make([]string, 0, len(raw))
	for _, f := range raw {
		if s, ok := f.(string); ok {
			frames = append(frames, s)
		}
	}
	return strings.Join(frames, "\n")
}
