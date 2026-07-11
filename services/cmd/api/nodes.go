package main

import (
	"fmt"
	"strings"
)

// nodeSpec is one orchestrator the api can place sandboxes on: its gRPC address (where the api
// calls Create/Delete) and its data-proxy address (written to the catalog as Route.Node, where
// client-proxy routes that sandbox's data path). See docs/STAGE23_DESIGN.md §4.
type nodeSpec struct {
	GRPC  string
	Proxy string
}

// parseNodeSpecs turns the --nodes flag into the orchestrator fleet. Format: comma-separated
// "grpc@proxy" entries, e.g. "127.0.0.1:9090@127.0.0.1:5007,127.0.0.1:9091@127.0.0.1:5017".
//
// Backward compatibility (Stage 23): an empty --nodes falls back to a single node built from
// the legacy --orchestrator-grpc / --orchestrator-proxy flags, so every pre-Stage-23
// invocation (dev-up.sh, the e2e fixture) keeps working as a one-node cluster with no flag
// change. Duplicate gRPC or proxy addresses are rejected: the proxy keys the catalog routes
// and the registry's byProxy index, so it must be unique.
func parseNodeSpecs(nodesFlag, fallbackGRPC, fallbackProxy string) ([]nodeSpec, error) {
	if strings.TrimSpace(nodesFlag) == "" {
		return []nodeSpec{{GRPC: fallbackGRPC, Proxy: fallbackProxy}}, nil
	}
	var specs []nodeSpec
	seenGRPC, seenProxy := map[string]bool{}, map[string]bool{}
	for _, entry := range strings.Split(nodesFlag, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		grpc, proxy, ok := strings.Cut(entry, "@")
		grpc, proxy = strings.TrimSpace(grpc), strings.TrimSpace(proxy)
		if !ok || grpc == "" || proxy == "" {
			return nil, fmt.Errorf("invalid --nodes entry %q: want grpc@proxy", entry)
		}
		if seenGRPC[grpc] {
			return nil, fmt.Errorf("--nodes: duplicate gRPC address %q", grpc)
		}
		if seenProxy[proxy] {
			return nil, fmt.Errorf("--nodes: duplicate proxy address %q", proxy)
		}
		seenGRPC[grpc], seenProxy[proxy] = true, true
		specs = append(specs, nodeSpec{GRPC: grpc, Proxy: proxy})
	}
	if len(specs) == 0 {
		return nil, fmt.Errorf("--nodes %q parsed to no nodes", nodesFlag)
	}
	return specs, nil
}
