package main

// Stage 11b: the ConnectRPC code-interpreter service, matching E2B's separate
// code-interpreter. It serves a server-streaming Execute -- the Connect-over-HTTP/1.1
// equivalent of the daemon's /execute SSE -- by driving the shared kernelManager and
// forwarding each OutputEvent as a stream frame. It runs on its own vsock port (the
// orchestrator routes /codeinterpreter.* there); the kernel is the same one the legacy
// HTTP /execute still drives (removed in 11c). See docs/STAGE11_DESIGN.md.

import (
	"context"
	"net/http"
	"time"

	"connectrpc.com/connect"

	ci "microsandbox/daemon/genpb/codeinterpreter"
	cic "microsandbox/daemon/genpb/codeinterpreter/codeinterpreterconnect"
)

type codeInterpreterService struct {
	cic.UnimplementedCodeInterpreterServiceHandler
	km *kernelManager
}

// Execute streams the cell's OutputEvents as it runs. It mirrors handleExecute (kernel.go):
// default language python, default timeout 30s, drive the shared kernel, end on idle /
// timeout / error. Each internal OutputEvent becomes one Connect stream frame; a send
// error (client gone) stops the stream.
func (s *codeInterpreterService) Execute(
	ctx context.Context,
	req *connect.Request[ci.ExecuteRequest],
	stream *connect.ServerStream[ci.OutputEvent],
) error {
	language := req.Msg.GetLanguage()
	if language == "" {
		language = "python"
	}
	timeout := req.Msg.GetTimeoutSeconds()
	if timeout <= 0 {
		timeout = 30
	}

	var sendErr error
	s.km.execute(ctx, req.Msg.GetCode(), language, time.Duration(timeout*float64(time.Second)), func(ev OutputEvent) {
		if sendErr != nil {
			return // a prior send failed (client went away); stop forwarding
		}
		// ExitCode is meaningful only on the end event; protocol.go carries it as *int
		// (nil otherwise). 0 is the natural default for the rest.
		var exit int32
		if ev.ExitCode != nil {
			exit = int32(*ev.ExitCode)
		}
		sendErr = stream.Send(&ci.OutputEvent{Type: ev.Type, Data: ev.Data, ExitCode: exit})
	})
	return sendErr
}

// registerCodeInterpreterService mounts the Connect code-interpreter handler on mux.
func registerCodeInterpreterService(mux *http.ServeMux, km *kernelManager) {
	path, handler := cic.NewCodeInterpreterServiceHandler(&codeInterpreterService{km: km})
	mux.Handle(path, handler)
}
