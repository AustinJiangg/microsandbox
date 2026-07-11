package main

import "testing"

func TestParseNodeSpecsFallback(t *testing.T) {
	// Empty --nodes falls back to the single legacy node (backward compat: the e2e fixture and
	// dev-up.sh pass only --orchestrator-grpc/--orchestrator-proxy).
	specs, err := parseNodeSpecs("", "127.0.0.1:9090", "127.0.0.1:5007")
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 || specs[0].GRPC != "127.0.0.1:9090" || specs[0].Proxy != "127.0.0.1:5007" {
		t.Fatalf("empty --nodes should yield the single fallback node, got %+v", specs)
	}
}

func TestParseNodeSpecsMultiNode(t *testing.T) {
	specs, err := parseNodeSpecs("127.0.0.1:9090@127.0.0.1:5007, 127.0.0.1:9091@127.0.0.1:5017", "x", "y")
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 {
		t.Fatalf("want 2 nodes, got %d (%+v)", len(specs), specs)
	}
	if specs[1].GRPC != "127.0.0.1:9091" || specs[1].Proxy != "127.0.0.1:5017" {
		t.Fatalf("second node mis-parsed (whitespace should be trimmed): %+v", specs[1])
	}
}

func TestParseNodeSpecsRejectsBadAndDup(t *testing.T) {
	cases := map[string]string{
		"no separator":   "127.0.0.1:9090",
		"empty proxy":    "127.0.0.1:9090@",
		"empty grpc":     "@127.0.0.1:5007",
		"duplicate grpc": "a:1@b:1,a:1@c:1",
		"duplicate proxy": "a:1@b:1,c:1@b:1",
	}
	for name, flag := range cases {
		if _, err := parseNodeSpecs(flag, "x", "y"); err == nil {
			t.Errorf("%s: expected an error for --nodes=%q", name, flag)
		}
	}
}
