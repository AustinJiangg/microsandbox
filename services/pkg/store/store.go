// Package store is the api's durable metadata: which sandboxes exist, on which
// template, since when. It mirrors E2B's api-owned database: the orchestrator keeps the
// *live* VM registry in memory, while the api persists the authoritative record that
// survives restarts.
//
// Stage 8c first backed it with SQLite (cgo-free modernc.org/sqlite). Stage 14b makes the
// backend pluggable behind this Store interface and adds a Postgres implementation (pgx),
// flipping the default to Postgres to match E2B -- SQLite stays selectable as a single-file
// escape hatch. The two impls live side by side (sqlite.go, postgres.go) so the diff
// between them is exactly the SQL-dialect differences; Open(dsn) dispatches by URL scheme.
// See docs/STAGE14_DESIGN.md.
//
// E2B generates its query layer with sqlc; for two tables and six statements we write plain
// database/sql, which is more transparent here. The Store *interface* is the seam -- swapping
// SQLite for Postgres is a change behind it, not above it (the api holds a store.Store value).
package store

import (
	"fmt"
	"strings"
)

// Sandbox is one persisted row.
type Sandbox struct {
	ID        string
	Template  string
	Status    string
	CreatedAt string // a timestamp string; SQLite stores CURRENT_TIMESTAMP as text, Postgres formats its timestamptz to RFC3339
}

// Build is one persisted template-build row.
type Build struct {
	BuildID   string
	Name      string
	State     string // building | success | failed
	Detail    string
	CreatedAt string
}

// Store is the api's metadata store. Every method can fail because the real backend
// (Postgres) is a network database; the api treats most writes as best-effort and logs
// failures (the orchestrator's in-memory registry is the live truth this stage). The two
// implementations -- sqliteStore and postgresStore -- satisfy this one contract.
type Store interface {
	InsertSandbox(id, template string) error
	DeleteSandbox(id string) error
	ListSandboxes() ([]Sandbox, error)
	InsertBuild(buildID, name string) error
	UpdateBuild(buildID, state, detail string) error
	ListBuilds() ([]Build, error)
	Close() error
}

// schema is applied on Open; IF NOT EXISTS makes it safe to run every startup (a poor man's
// migration -- a real one would version these). Both engines accept it verbatim (TEXT,
// TIMESTAMP, DEFAULT CURRENT_TIMESTAMP, IF NOT EXISTS are all standard) -- the one wrinkle is
// that Postgres rejects multiple statements in a single Exec, so postgres.go applies these
// one at a time (splitSchema), while SQLite runs the whole string at once. The builds table
// arrives with the TemplateService (Stage 10): the api records each template build and its
// outcome, like E2B's api owns templates/builds in Postgres.
const schema = `
CREATE TABLE IF NOT EXISTS sandboxes (
    id         TEXT PRIMARY KEY,
    template   TEXT NOT NULL,
    status     TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE TABLE IF NOT EXISTS builds (
    build_id   TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    state      TEXT NOT NULL,
    detail     TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);`

// splitSchema breaks the multi-statement schema into individual statements. Postgres's
// extended query protocol (pgx's default) rejects more than one command per Exec, so its
// schema setup runs each separately; sqlite.go doesn't need this but it is harmless there.
func splitSchema() []string {
	var stmts []string
	for _, s := range strings.Split(schema, ";") {
		if strings.TrimSpace(s) != "" {
			stmts = append(stmts, s)
		}
	}
	return stmts
}

// Open returns a Store backed by the database the DSN names, dispatching by URL scheme:
//
//	postgres://… / postgresql://…  -> Postgres (pgx); the Stage 14b default
//	sqlite://<path>                -> SQLite (modernc); the <path> is everything after sqlite://
//	<bare path>                    -> SQLite (modernc); backward-compatible with the old --db flag
//
// Keeping the bare-path case means an old "vendor/microsandbox.db" argument still opens
// SQLite, so the single-file escape hatch needs no scheme.
func Open(dsn string) (Store, error) {
	switch {
	case strings.HasPrefix(dsn, "postgres://"), strings.HasPrefix(dsn, "postgresql://"):
		return openPostgres(dsn)
	case strings.HasPrefix(dsn, "sqlite://"):
		return openSQLite(strings.TrimPrefix(dsn, "sqlite://"))
	default:
		return openSQLite(dsn)
	}
}

// errSchema wraps a schema-application failure uniformly across both backends.
func errSchema(err error) error { return fmt.Errorf("ensuring schema: %w", err) }
