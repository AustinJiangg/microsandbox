package store

import (
	"os"
	"path/filepath"
	"testing"
)

// One contract, two backends. The CRUD logic lives in runSandboxContract/runBuildsContract/
// runAuthContract and is exercised against both impls: SQLite on a temp file (hermetic --
// runs anywhere Go is, the cgo-free modernc driver needs no C toolchain), and Postgres only
// when MSB_TEST_PG_DSN points at a live server (the e2e/CI sets it; a bare `go test` skips it,
// the same discipline catalog's redis_test.go uses). The contract checks membership by id
// and deltas rather than absolute counts, and cleans up its own rows, so it is correct even
// against a shared Postgres that already holds other rows. Since Stage 16 the contracts are
// team-scoped (rows carry a team_id, reads filter by it). See docs/STAGE14/16_DESIGN.md.

func TestSQLiteStore(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer st.Close()

	// A fresh SQLite file lists nothing for any team -- an extra check the shared
	// (shared-DB-safe) contract can't make, but a temp-file backend can.
	if rows, err := st.ListSandboxes("team_a"); err != nil || len(rows) != 0 {
		t.Fatalf("fresh sqlite: want 0 rows (err=%v), got %d", err, len(rows))
	}
	runSandboxContract(t, st)
	runPauseResumeContract(t, st)
	runBuildsContract(t, st)
	runAuthContract(t, st)
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
		// A working store can round-trip a row, scoped to its team.
		if err := st.InsertSandbox("sb_dispatch", "default", "team_a"); err != nil {
			t.Errorf("Open(%q): insert: %v", dsn, err)
		}
		if rows, _ := st.ListSandboxes("team_a"); len(rows) != 1 {
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
	runPauseResumeContract(t, st)
	runBuildsContract(t, st)
	runAuthContract(t, st)
}

// runPauseResumeContract covers the Stage 26 pause/resume metadata: a paused sandbox reports its
// origin_node and status "paused" (visible in the list), and resume restores status "running".
func runPauseResumeContract(t *testing.T, st Store) {
	t.Helper()
	const id, team = "sb_pause_contract", "team_pause"
	clean := func() { _ = st.DeleteSandbox(id) }
	clean()
	defer clean()

	if err := st.InsertSandbox(id, "ml-env", team); err != nil {
		t.Fatalf("insert %s: %v", id, err)
	}
	// A fresh (running) sandbox is not paused.
	if origin, tmpl, paused, err := st.PausedSandbox(id); err != nil || paused || origin != "" || tmpl != "" {
		t.Fatalf("PausedSandbox(running) = (%q,%q,%v,%v), want (\"\",\"\",false,nil)", origin, tmpl, paused, err)
	}

	// Pause records the origin node and flips the status; the list reflects it.
	const origin = "proxy-node-a:5007"
	if err := st.PauseSandbox(id, origin); err != nil {
		t.Fatalf("PauseSandbox: %v", err)
	}
	// PausedSandbox returns the origin node AND the template resume needs.
	if got, tmpl, paused, err := st.PausedSandbox(id); err != nil || !paused || got != origin || tmpl != "ml-env" {
		t.Fatalf("PausedSandbox(paused) = (%q,%q,%v,%v), want (%q,\"ml-env\",true,nil)", got, tmpl, paused, err, origin)
	}
	if row, ok := sandboxIDs(t, st, team)[id]; !ok || row.Status != "paused" {
		t.Fatalf("after pause: want status=paused in the list, got %+v (ok=%v)", row, ok)
	}

	// Resume flips it back to running; it is no longer reported as paused.
	if err := st.ResumeSandbox(id); err != nil {
		t.Fatalf("ResumeSandbox: %v", err)
	}
	if _, _, paused, err := st.PausedSandbox(id); err != nil || paused {
		t.Fatalf("PausedSandbox(after resume) paused=%v err=%v, want false,nil", paused, err)
	}
	if row, ok := sandboxIDs(t, st, team)[id]; !ok || row.Status != "running" {
		t.Fatalf("after resume: want status=running, got %+v (ok=%v)", row, ok)
	}

	// Pausing/resuming an absent id is idempotent (zero rows, not an error).
	if err := st.PauseSandbox("sb_pause_missing", origin); err != nil {
		t.Errorf("idempotent PauseSandbox(missing) errored: %v", err)
	}
	if err := st.ResumeSandbox("sb_pause_missing"); err != nil {
		t.Errorf("idempotent ResumeSandbox(missing) errored: %v", err)
	}
}

// sandboxIDs returns the set of ids a team currently lists.
func sandboxIDs(t *testing.T, st Store, team string) map[string]Sandbox {
	t.Helper()
	rows, err := st.ListSandboxes(team)
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
	const a, b, other = "sb_contract_a", "sb_contract_b", "sb_contract_other"
	const teamA, teamB = "team_contract_a", "team_contract_b"
	clean := func() { _ = st.DeleteSandbox(a); _ = st.DeleteSandbox(b); _ = st.DeleteSandbox(other) }
	clean()       // clear any leftovers from a prior aborted run (matters on a shared Postgres)
	defer clean() // leave the table as we found it

	// Insert two for teamA and one for teamB; each gets status "running" and a non-empty created_at.
	if err := st.InsertSandbox(a, "default", teamA); err != nil {
		t.Fatalf("insert %s: %v", a, err)
	}
	if err := st.InsertSandbox(b, "ml-env", teamA); err != nil {
		t.Fatalf("insert %s: %v", b, err)
	}
	if err := st.InsertSandbox(other, "default", teamB); err != nil {
		t.Fatalf("insert %s: %v", other, err)
	}
	got := sandboxIDs(t, st, teamA)
	for _, sb := range []struct{ id, tmpl string }{{a, "default"}, {b, "ml-env"}} {
		row, ok := got[sb.id]
		if !ok {
			t.Fatalf("after insert: %s missing from teamA list", sb.id)
		}
		if row.Template != sb.tmpl || row.Status != "running" || row.TeamID != teamA || row.CreatedAt == "" {
			t.Errorf("%s: got %+v, want template=%s status=running team=%s non-empty created_at", sb.id, row, sb.tmpl, teamA)
		}
	}
	// Team isolation: teamA must not see teamB's sandbox, and vice versa.
	if _, ok := got[other]; ok {
		t.Errorf("isolation: teamA sees teamB's sandbox %s", other)
	}
	if _, ok := sandboxIDs(t, st, teamB)[a]; ok {
		t.Errorf("isolation: teamB sees teamA's sandbox %s", a)
	}

	// SandboxTeam reports ownership for the delete-authorisation path; an unknown id is a miss.
	if team, ok, err := st.SandboxTeam(a); err != nil || !ok || team != teamA {
		t.Errorf("SandboxTeam(%s) = (%q,%v,%v), want (%q,true,nil)", a, team, ok, err, teamA)
	}
	if _, ok, err := st.SandboxTeam("sb_contract_missing"); err != nil || ok {
		t.Errorf("SandboxTeam(missing) = (_,%v,%v), want (_,false,nil)", ok, err)
	}

	// Delete one; the other (and teamB's) remain.
	if err := st.DeleteSandbox(a); err != nil {
		t.Fatalf("delete %s: %v", a, err)
	}
	if _, ok := sandboxIDs(t, st, teamA)[a]; ok {
		t.Errorf("after delete: %s still listed", a)
	}
	if _, ok := sandboxIDs(t, st, teamA)[b]; !ok {
		t.Errorf("after delete: %s should remain", b)
	}

	// Deleting an absent id is idempotent (not an error).
	if err := st.DeleteSandbox("sb_contract_missing"); err != nil {
		t.Errorf("idempotent delete errored: %v", err)
	}

	// A duplicate id violates the primary key (b is still present).
	if err := st.InsertSandbox(b, "x", teamA); err == nil {
		t.Error("duplicate insert: want a primary-key error, got nil")
	}
}

func buildByID(t *testing.T, st Store, team, id string) (Build, bool) {
	t.Helper()
	rows, err := st.ListBuilds(team)
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
	const id = "bld_contract_team_1"
	const team = "team_contract_builds"
	// The Store API has no DeleteBuild (E2B keeps builds as an audit record), so this row
	// outlives the test on a shared Postgres. To stay re-runnable we tolerate a leftover row:
	// insert only if absent, then verify the transitions rather than insisting on a fresh insert.
	// The id is team-qualified so it never collides on the global PK with a row some other run
	// (or a pre-Stage-16 migration's backfill into team 'default') left under a different team.
	if _, present := buildByID(t, st, team, id); !present {
		if err := st.InsertBuild(id, "demo", team); err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
	}
	row, ok := buildByID(t, st, team, id)
	if !ok {
		t.Fatalf("after insert: %s missing from list", id)
	}
	if row.Name != "demo" || row.TeamID != team || row.CreatedAt == "" {
		t.Fatalf("unexpected build row: %+v", row)
	}

	// BuildTeam reports ownership (authorises the status read); an unknown id is a miss.
	if got, ok, err := st.BuildTeam(id); err != nil || !ok || got != team {
		t.Errorf("BuildTeam(%s) = (%q,%v,%v), want (%q,true,nil)", id, got, ok, err, team)
	}
	if _, ok, err := st.BuildTeam("bld_contract_missing"); err != nil || ok {
		t.Errorf("BuildTeam(missing) = (_,%v,%v), want (_,false,nil)", ok, err)
	}

	// Another team must not see this build.
	if _, ok := buildByID(t, st, "team_other", id); ok {
		t.Errorf("isolation: team_other sees build %s", id)
	}

	// Update records the terminal state + detail.
	if err := st.UpdateBuild(id, "failed", "docker build: boom"); err != nil {
		t.Fatalf("update %s: %v", id, err)
	}
	if row, _ := buildByID(t, st, team, id); row.State != "failed" || row.Detail != "docker build: boom" {
		t.Fatalf("after update: %+v", row)
	}

	// Updating an absent build is idempotent (zero rows affected, not an error).
	if err := st.UpdateBuild("bld_contract_missing", "success", ""); err != nil {
		t.Errorf("idempotent update errored: %v", err)
	}

	// A duplicate build id violates the primary key.
	if err := st.InsertBuild(id, "demo", team); err == nil {
		t.Error("duplicate build insert: want a primary-key error, got nil")
	}
}

// runAuthContract covers the Stage 16 teams/keys: an idempotent seed, key->team resolution,
// and that an unknown key is a clean miss (not an error).
func runAuthContract(t *testing.T, st Store) {
	t.Helper()
	const team = "team_auth"
	const hash = "deadbeef_contract_keyhash"

	if err := st.EnsureTeam(team, "Auth Test"); err != nil {
		t.Fatalf("EnsureTeam: %v", err)
	}
	if err := st.EnsureTeam(team, "Auth Test"); err != nil { // idempotent: a second call is a no-op
		t.Fatalf("EnsureTeam (idempotent): %v", err)
	}
	if err := st.InsertAPIKey(hash, team); err != nil {
		t.Fatalf("InsertAPIKey: %v", err)
	}
	if err := st.InsertAPIKey(hash, team); err != nil { // idempotent
		t.Fatalf("InsertAPIKey (idempotent): %v", err)
	}

	if got, ok, err := st.ResolveAPIKey(hash); err != nil || !ok || got != team {
		t.Errorf("ResolveAPIKey(hit) = (%q,%v,%v), want (%q,true,nil)", got, ok, err, team)
	}
	if got, ok, err := st.ResolveAPIKey("no_such_key_hash"); err != nil || ok || got != "" {
		t.Errorf("ResolveAPIKey(miss) = (%q,%v,%v), want (\"\",false,nil)", got, ok, err)
	}
}
