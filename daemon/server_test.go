package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The envd HTTP surface is now just /health (Stage 11 moved files/commands/execute to
// ConnectRPC -- those are covered by envd_test.go's Connect tests). This drives /health
// over a plain httptest server, no vsock/VM.

func TestHealth(t *testing.T) {
	srv := httptest.NewServer(newMux())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["status"] != "ok" {
		t.Errorf("status = %q, want ok", body["status"])
	}
}
