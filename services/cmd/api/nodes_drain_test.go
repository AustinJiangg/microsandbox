package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc/codes"

	"microsandbox/services/pkg/placement"
)

// TestHandleDrainStaticModeUnsupported: with no DrainCommands (a.drain == nil, i.e. --node-discovery
// static) drain is rejected with 501 rather than silently no-op'ing -- a fixed fleet has no
// heartbeat to carry a status change (Decision D5).
func TestHandleDrainStaticModeUnsupported(t *testing.T) {
	reg := placement.NewStaticRegistry(nil, 1)
	a := &api{registry: reg} // drain == nil -> static mode

	req := httptest.NewRequest(http.MethodPost, "/nodes/n1/drain", nil)
	req.SetPathValue("id", "n1")
	w := httptest.NewRecorder()
	a.handleDrain(w, req)

	if w.Code != http.StatusNotImplemented {
		t.Fatalf("static-mode drain should be 501 Not Implemented, got %d", w.Code)
	}
}

// TestHandleDrainUnknownNode: drain is wired (redis mode) but the target id is not in the fleet ->
// 404 before any Redis command is issued. The DrainCommands' Redis client is lazy, so this needs no
// live Redis (NodeByID fails first).
func TestHandleDrainUnknownNode(t *testing.T) {
	addr, _ := startFakeOrch(t, "n0", codes.OK)
	reg := placement.NewStaticRegistry([]*placement.Node{nodeTo(t, addr)}, 1)
	a := &api{registry: reg, drain: placement.NewDrainCommands("127.0.0.1:6379")} // client never dialed

	req := httptest.NewRequest(http.MethodPost, "/nodes/does-not-exist/drain", nil)
	req.SetPathValue("id", "does-not-exist")
	w := httptest.NewRecorder()
	a.handleDrain(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("draining a node not in the fleet should be 404, got %d", w.Code)
	}
}
