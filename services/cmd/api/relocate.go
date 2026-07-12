package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"microsandbox/services/pkg/catalog"
	pb "microsandbox/services/pkg/grpc/orchestrator"
	"microsandbox/services/pkg/placement"
)

// Sandbox relocation (Stage 26). E2B moves a sandbox off a node not with a server-driven migration
// loop (there is none) but through the pause -> resume lifecycle: Pause checkpoints the VM to
// object storage and frees its node; Resume restores it, preferring the origin node but re-placing
// it when the origin is draining. These two handlers are that mechanism's control plane; the
// drain-aware node choice is placement.Registry.PickPreferred (Stage 26a). See docs/STAGE26_DESIGN.md.
//
// Scope: the real orchestrator's Pause/Resume return Unimplemented (a real per-sandbox live snapshot
// is the Stage 20/22 producer, deferred -- D4), so on real VMs Pause here surfaces as 500. The
// relocation SCHEDULING these handlers implement is verified in process against fake orchestrators
// (placement_integration_test.go), which is the multi-node story on one box (Stage 23/24 rationale).

// handleSandboxPause: POST /sandboxes/{id}/pause -> checkpoint the sandbox and free its node so it
// can be resumed later (possibly elsewhere). Authorise (team ownership, like destroy) -> resolve
// the holding node from the catalog route -> gRPC Pause there -> record it paused with its origin
// node (so resume can prefer it) -> drop the route (a paused sandbox is unreachable). 404 on an
// unknown/other-team id; 409 if it is not currently running (no route to a node).
func (a *api) handleSandboxPause(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	team := teamFromContext(r.Context())
	owner, ok, err := a.store.SandboxTeam(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "ownership lookup failed: " + err.Error()})
		return
	}
	if !ok || owner != team {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such sandbox: " + id})
		return
	}
	// The catalog route names the node currently holding the running sandbox. No route means there
	// is nothing to pause (it is already paused, or was never routable) -> 409.
	route, ok, err := a.catalog.Get(id)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "route lookup failed: " + err.Error()})
		return
	}
	if !ok {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "sandbox is not running: " + id})
		return
	}
	node, found := a.registry.NodeByProxy(route.Node)
	if !found {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "node holding the sandbox is not in the fleet: " + route.Node})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if _, err := node.RPC.Pause(ctx, &pb.SandboxPauseRequest{SandboxId: id}); err != nil {
		writeGRPCError(w, err) // Unimplemented on the real orchestrator (D4) -> 500; the fake pauses
		return
	}
	// The VM is checkpointed and gone from its node: record the pause (origin = the node it was on,
	// so resume can prefer it) and drop the route so the data path stops resolving to a node that no
	// longer holds it. Both are best-effort/logged, consistent with create/destroy.
	if err := a.store.PauseSandbox(id, route.Node); err != nil {
		log.Printf("store: pause %s: %v", id, err)
	}
	if err := a.catalog.Delete(id); err != nil {
		log.Printf("catalog: delete route %s (pause): %v", id, err)
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "status": "paused"})
}

// handleSandboxResume: POST /sandboxes/{id}/resume -> restore a paused sandbox, preferring the node
// it was paused on but relocating it when that node is draining/gone. Authorise -> confirm it is
// paused and read its origin + template -> PickPreferred a target (origin if still eligible, else
// BestOfK; both skip draining nodes) -> gRPC Resume there -> mint a fresh data-plane token and
// rewrite the catalog route to the new node -> mark it running. 404 unknown/other-team; 409 if not
// paused; 503 if no node is eligible to resume on.
func (a *api) handleSandboxResume(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	team := teamFromContext(r.Context())
	owner, ok, err := a.store.SandboxTeam(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "ownership lookup failed: " + err.Error()})
		return
	}
	if !ok || owner != team {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such sandbox: " + id})
		return
	}
	origin, tmpl, paused, err := a.store.PausedSandbox(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "paused-state lookup failed: " + err.Error()})
		return
	}
	if !paused {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "sandbox is not paused: " + id})
		return
	}
	// Prefer the origin node, but only while it is eligible: NodeByProxy yields nil when the origin
	// has left the fleet, and PickPreferred drops it when it is draining/not-ready -- so a sandbox
	// whose origin is draining resumes on ANOTHER node. This is the whole point of the stage.
	preferred, _ := a.registry.NodeByProxy(origin)
	cfg := &pb.SandboxConfig{Template: tmpl, FromSnapshot: true}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	node, _, err := a.placeResume(ctx, id, preferred, cfg)
	if err != nil {
		if errors.Is(err, placement.ErrNoNode) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no orchestrator node available to resume on: " + err.Error()})
		} else {
			writeGRPCError(w, err)
		}
		return
	}
	defer node.Release()

	// If we cannot make the resumed VM routable, tear it down so it doesn't leak (mirrors create's
	// rollback). Deleting loses the restored state, but a route-less VM is unreachable anyway; on the
	// real orchestrator Resume is Unimplemented, so this rollback path is exercised only in process.
	rollback := func() {
		rb, cancelRB := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancelRB()
		if _, derr := node.RPC.Delete(rb, &pb.SandboxDeleteRequest{SandboxId: id}); derr != nil {
			log.Printf("resume rollback: delete %s: %v", id, derr)
		}
	}
	token, err := newAccessToken()
	if err != nil {
		rollback()
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not mint access token: " + err.Error()})
		return
	}
	// Rewrite the data-path route to the node the sandbox resumed on -- the catalog is the single
	// source of truth for where a sandbox lives, so client-proxy follows the relocation for free.
	if err := a.catalog.Set(id, catalog.Route{Node: node.Proxy, Token: token}); err != nil {
		rollback()
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "could not register sandbox route: " + err.Error()})
		return
	}
	if err := a.store.ResumeSandbox(id); err != nil {
		log.Printf("store: resume %s: %v", id, err)
	}
	// Hand back the same shape create does, so a client can reconnect: where to reach the data path
	// and the fresh access token to send.
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "data_url": a.dataURL, "token": token})
}

// placeResume picks a node to resume a sandbox on -- preferring its origin (PickPreferred) and
// failing over past a node whose Resume errors -- mirroring placeCreate for Create. It returns the
// chosen node RESERVED (the caller Releases once settled) and the Resume response. Error discipline
// matches placeCreate: an InvalidArgument is the request's fault and is returned WITHOUT failover
// (-> 400); any other Resume error excludes the node and retries; ErrNoNode surfaces (-> 503) only
// when nothing is eligible to even attempt. Excluding a failed node's ID also drops it as the
// preferred on the next PickPreferred, so a broken origin doesn't get retried forever.
func (a *api) placeResume(ctx context.Context, id string, preferred *placement.Node, cfg *pb.SandboxConfig) (*placement.Node, *pb.SandboxResumeResponse, error) {
	excluded := map[string]struct{}{}
	var lastErr error
	for {
		node, err := a.registry.PickPreferred(preferred, excluded)
		if err != nil {
			if lastErr != nil {
				return nil, nil, lastErr // surface the real resume failure, not "no node"
			}
			return nil, nil, err // never attempted a node -> ErrNoNode (503)
		}
		node.Reserve()
		resp, rerr := node.RPC.Resume(ctx, &pb.SandboxResumeRequest{SandboxId: id, Config: cfg})
		if rerr != nil {
			node.Release()
			if status.Code(rerr) == codes.InvalidArgument {
				return nil, nil, rerr // request's fault -> don't fail over
			}
			lastErr = rerr
			excluded[node.ID] = struct{}{}
			log.Printf("placement: resume of %s on node %s failed (%v); excluding and retrying", id, node.ID, rerr)
			continue
		}
		return node, resp, nil // reserved; the caller releases once settled
	}
}
