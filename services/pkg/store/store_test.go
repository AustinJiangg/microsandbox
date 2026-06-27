package store

import (
	"os"
	"path/filepath"
	"testing"
)

// One contract, two backends. The CRUD logic lives in runSandboxContract/runBuildsContract
// and is exercised against both impls: SQLite on a temp file (hermetic -- runs anywhere Go
// is, the cgo-free modernc driver needs no C toolchain), and Postgres only when
// MSB_TEST_PG_DSN points at a live server (the e2e/CI sets it; a bare `go test` skips it,
// the same discipline catalog's redis_test.go uses). The contract checks membership by id
// and deltas rather than absolute counts, and cleans up its own rows, so it is correct even
// against a shared Postgres that already holds other rows. See docs/STAGE14_DESIGN.md.

func TestSQLiteStore(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer st.Close()

	// A fresh SQLite file lists nothing -- an extra check the shared (shared-DB-safe) contract
	// can't make, but a temp-file backend can.
	if rows, err := st.ListSandboxes(); err != nil || len(rows) != 0 {
		t.Fatalf("fresh sqlite: want 0 rows (err=%v), got %d", err, len(rows))
	}
	runSandboxContract(t, st)
	runBuildsContract(t, st)
}

// TestOpenDispatch covers the scheme dispatch in Open: both a bare path and an explicit
// sqlite:// DSN must open SQLite (the single-file escape hatch the api keeps, Decision 1).
// The postgres:// branch is exercised by TestPostgresStore when MSB_TEST_PG_DSN is set.
func TestOpenDispatch(t *testing.T) {
	dir := t.TempDir()
	for _, dsn := range []string{
		filepath.Join(dir, "bare.db"),            // bare path -> sqlite (backward compatible)
		"sqlite://" + filepath.Join(dir, "x.db"), // explicit sqlite:// scheme
	} {
		st, err := Open(dsn)
		if err != nil {
			t.Fatalf("Open(%q): %v", dsn, err)
		}
		// A working store can round-trip a row.
		if err := st.InsertSandbox("sb_dispatch", "default"); err != nil {
			t.Errorf("Open(%q): insert: %v", dsn, err)
		}
		if rows, _ := st.ListSandboxes(); len(rows) != 1 {
			t.Errorf("Open(%q): want 1 row, got %d", dsn, len(rows))
		}
		st.Close()
	}
}

func TestPostgresStore(t *testing.T) {
	dsn := os.Getenv("MSB_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("MSB_TEST_PG_DSN not set; skipping live Postgres store test")
	}
	st, err := Open(dsn)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	defer st.Close()
	runSandboxContract(t, st)
	runBuildsContract(t, st)
}

// sandboxIDs returns the set of ids currently listed.
func sandboxIDs(t *testing.T, st Store) map[string]Sandbox {
	t.Helper()
	rows, err := st.ListSandboxes()
	if err != nil {
		t.Fatalf("list sandboxes: %v", err)
	}
	m := make(map[string]Sandbox, len(rows))
	for _, sb := range rows {
		m[sb.ID] = sb
	}
	return m
}

func runSandboxContract(t *testing.T, st Store) {
	t.Helper()
	const a, b = "sb_contract_a", "sb_contract_b"
	clean := func() { _ = st.DeleteSandbox(a); _ = st.DeleteSandbox(b) }
	clean()       // clear any leftovers from a prior aborted run (matters on a shared Postgres)
	defer clean() // leave the table as we found it

	// Insert two; each gets status "running" and a non-empty created_at from the DB.
	if err := st.InsertSandbox(a, "default"); err != nil {
		t.Fatalf("insert %s: %v", a, err)
	}
	if err := st.InsertSandbox(b, "ml-env"); err != nil {
		t.Fatalf("insert %s: %v", b, err)
	}
	got := sandboxIDs(t, st)
	for _, sb := range []struct{ id, tmpl string }{{a, "default"}, {b, "ml-env"}} {
		row, ok := got[sb.id]
		if !ok {
			t.Fatalf("after insert: %s missing from list", sb.id)
		}
		if row.Template != sb.tmpl || row.Status != "running" || row.CreatedAt == "" {
			t.Errorf("%s: got %+v, want template=%s status=running non-empty created_at", sb.id, row, sb.tmpl)
		}
	}

	// Delete one; the other remains.
	if err := st.DeleteSandbox(a); err != nil {
		t.Fatalf("delete %s: %v", a, err)
	}
	got = sandboxIDs(t, st)
	if _, ok := got[a]; ok {
		t.Errorf("after delete: %s still listed", a)
	}
	if _, ok := got[b]; !ok {
		t.Errorf("after delete: %s should remain", b)
	}

	// Deleting an absent id is idempotent (not an error).
	if err := st.DeleteSandbox("sb_contract_missing"); err != nil {
		t.Errorf("idempotent delete errored: %v", err)
	}

	// A duplicate id violates the primary key (b is still present).
	if err := st.InsertSandbox(b, "x"); err == nil {
		t.Error("duplicate insert: want a primary-key error, got nil")
	}
}

func buildByID(t *testing.T, st Store, id string) (Build, bool) {
	t.Helper()
	rows, err := st.ListBuilds()
	if err != nil {
		t.Fatalf("list builds: %v", err)
	}
	for _, b := range rows {
		if b.BuildID == id {
			return b, true
		}
	}
	return Build{}, false
}

func runBuildsContract(t *testing.T, st Store) {
	t.Helper()
	const id = "bld_contract_1"
	// The Store API has no DeleteBuild (E2B keeps builds as an audit record), so this row
	// outlives the test on a shared Postgres. To stay re-runnable we tolerate a leftover row:
	// insert only if absent, then verify the transitions rather than insisting on a fresh insert.
	if _, present := buildByID(t, st, id); !present {
		if err := st.InsertBuild(id, "demo"); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	row, ok := buildByID(t, st, id)
	if !ok {
		t.Fatalf("after insert: %s missing from list", id)
	}
	if row.Name != "demo" || row.CreatedAt == "" {
		t.Fatalf("unexpected build row: %+v", row)
	}

	// Update records the terminal state + detail.
	if err := st.UpdateBuild(id, "failed", "docker build: boom"); err != nil {
		t.Fatalf("update %s: %v", id, err)
	}
	if row, _ := buildByID(t, st, id); row.State != "failed" || row.Detail != "docker build: boom" {
		t.Fatalf("after update: %+v", row)
	}

	// Updating an absent build is idempotent (zero rows affected, not an error).
	if err := st.UpdateBuild("bld_contract_missing", "success", ""); err != nil {
		t.Errorf("idempotent update errored: %v", err)
	}

	// A duplicate build id violates the primary key.
	if err := st.InsertBuild(id, "demo"); err == nil {
		t.Error("duplicate build insert: want a primary-key error, got nil")
	}
}
