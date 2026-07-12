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
	TeamID    string // the owning team (Stage 16); reads are scoped to it, so it is not re-emitted in the api JSON
	CreatedAt string // a timestamp string; SQLite stores CURRENT_TIMESTAMP as text, Postgres formats its timestamptz to RFC3339
}

// Build is one persisted template-build row.
type Build struct {
	BuildID   string
	Name      string
	State     string // building | success | failed
	Detail    string
	TeamID    string // the owning team (Stage 16)
	CreatedAt string
}

// Store is the api's metadata store. Every method can fail because the real backend
// (Postgres) is a network database; the api treats most writes as best-effort and logs
// failures (the orchestrator's in-memory registry is the live truth this stage). The two
// implementations -- sqliteStore and postgresStore -- satisfy this one contract.
//
// Stage 16 makes it team-aware (E2B's api-key->team model): rows carry a team_id, reads are
// scoped to a team, and the store also resolves an API key (stored hashed) to its team. The
// ownership lookups (SandboxTeam / BuildTeam) let the api authorise a delete / status read
// before touching the live VM, rather than scope the mutation itself -- the teardown that
// follows is unscoped because ownership was already checked. See docs/STAGE16_DESIGN.md.
type Store interface {
	// sandboxes -- team-scoped
	InsertSandbox(id, template, teamID string) error
	SandboxTeam(id string) (teamID string, ok bool, err error) // ownership lookup (for delete/pause/resume)
	DeleteSandbox(id string) error                             // unscoped; called after the ownership check
	ListSandboxes(teamID string) ([]Sandbox, error)
	// pause/resume relocation (Stage 26): a paused sandbox is on no node. PauseSandbox marks it
	// paused and records origin_node (the data-proxy addr it was paused from); PausedSandbox reads
	// that back so resume can prefer the origin (dropping to placement when it drains); ResumeSandbox
	// marks it running again. All unscoped -- the api checks ownership (SandboxTeam) first, like delete.
	PauseSandbox(id, originNode string) error
	PausedSandbox(id string) (originNode, template string, paused bool, err error)
	ResumeSandbox(id string) error
	// template builds -- team-scoped
	InsertBuild(buildID, name, teamID string) error
	BuildTeam(buildID string) (teamID string, ok bool, err error)
	UpdateBuild(buildID, state, detail string) error
	ListBuilds(teamID string) ([]Build, error)
	// auth (Stage 16): keys are stored hashed (sha256 hex); the seed helpers are idempotent
	ResolveAPIKey(keyHash string) (teamID string, ok bool, err error)
	EnsureTeam(id, name string) error
	InsertAPIKey(keyHash, teamID string) error
	Close() error
}

// schema is applied on Open; IF NOT EXISTS makes it safe to run every startup (a poor man's
// migration -- a real one would version these). Both engines accept it verbatim (TEXT,
// TIMESTAMP, DEFAULT CURRENT_TIMESTAMP, IF NOT EXISTS are all standard) -- the one wrinkle is
// that Postgres rejects multiple statements in a single Exec, so postgres.go applies these
// one at a time (splitSchema), while SQLite runs the whole string at once. The builds table
// arrives with the TemplateService (Stage 10): the api records each template build and its
// outcome, like E2B's api owns templates/builds in Postgres. Stage 16 adds the teams +
// api_keys tables (keys stored hashed) for the X-API-Key->team auth.
//
// The team_id column on sandboxes/builds is added by migrateTeamColumns, NOT here: a poor
// man's "CREATE TABLE IF NOT EXISTS" is a no-op against a table that already exists (the
// conftest reuses a Postgres already on :5432, so a prior session's DB lacks the column), so
// the column has to be added with an idempotent ALTER instead. Keeping it out of the CREATE
// above leaves one source of truth for that column (the migration), exercised on fresh and
// existing DBs alike. Stage 26 adds sandboxes.origin_node the same way (migrateOriginNode), for
// the same reason -- it records the node a paused sandbox was paused from, so resume can prefer it.
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
);
CREATE TABLE IF NOT EXISTS teams (
    id   TEXT PRIMARY KEY,
    name TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS api_keys (
    key_hash   TEXT PRIMARY KEY,
    team_id    TEXT NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);`

// teamColumnTables are the pre-existing tables that gain a team_id column in Stage 16. The
// DEFAULT 'default' backfills any rows written before the upgrade into the default team, so
// an old DB stays consistent. Each backend adds the column idempotently (see migrateTeamColumns
// in sqlite.go / postgres.go) because the two engines spell "add a column if absent" differently.
var teamColumnTables = []string{"sandboxes", "builds"}

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
