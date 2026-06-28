package store

import (
	"database/sql"

	_ "modernc.org/sqlite" // registers the cgo-free "sqlite" database/sql driver
)

// sqliteStore is the SQLite implementation of Store (the Stage 8c original, now one of two
// backends). It stays a first-class option -- selectable via a "sqlite://path" or bare-path
// DSN -- because the api is its sole user, so a single-process / no-docker run still works
// end to end on a SQLite file (docs/STAGE14_DESIGN.md, Decision 1). It keeps SQLite's three
// dialect choices, which postgres.go contrasts: "?" placeholders, a single-writer connection
// cap, and created_at scanned as text.
type sqliteStore struct {
	db *sql.DB
}

// sqliteStore must satisfy Store.
var _ Store = (*sqliteStore)(nil)

// openSQLite opens (creating the file if needed) the SQLite database at path and ensures
// the schema is present.
func openSQLite(path string) (Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// SQLite allows only one writer at a time; capping the pool at a single connection
	// serializes all access through database/sql and sidesteps "database is locked" under
	// concurrent requests. Our request volume is tiny, so this costs nothing. (Postgres has
	// real MVCC concurrency, so postgres.go drops this cap.)
	db.SetMaxOpenConns(1)
	// SQLite accepts the whole multi-statement schema in one Exec.
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, errSchema(err)
	}
	// Stage 16: add the team_id column to the pre-existing tables. SQLite (modernc) has no
	// "ADD COLUMN IF NOT EXISTS", so check the table's columns first and ALTER only when absent.
	if err := sqliteMigrateTeamColumns(db); err != nil {
		db.Close()
		return nil, errSchema(err)
	}
	return &sqliteStore{db: db}, nil
}

// sqliteMigrateTeamColumns adds team_id to each table that needs it, idempotently: PRAGMA
// table_info lists the columns, and we ALTER only when team_id is missing. (Postgres uses
// ADD COLUMN IF NOT EXISTS for the same effect; SQLite lacks that clause.)
func sqliteMigrateTeamColumns(db *sql.DB) error {
	for _, table := range teamColumnTables {
		has, err := sqliteHasColumn(db, table, "team_id")
		if err != nil {
			return err
		}
		if has {
			continue
		}
		if _, err := db.Exec(
			`ALTER TABLE ` + table + ` ADD COLUMN team_id TEXT NOT NULL DEFAULT 'default'`); err != nil {
			return err
		}
	}
	return nil
}

// sqliteHasColumn reports whether table has a column named col, via PRAGMA table_info (whose
// rows are cid, name, type, notnull, dflt_value, pk). table is an internal constant, never
// user input, so interpolating it into the PRAGMA is safe.
func sqliteHasColumn(db *sql.DB, table, col string) (bool, error) {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == col {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (s *sqliteStore) Close() error { return s.db.Close() }

// InsertSandbox records a newly created sandbox owned by teamID. status starts as "running";
// created_at is filled by SQLite's CURRENT_TIMESTAMP. A duplicate id violates the primary key
// and returns an error (ids are random, so this should never happen in practice).
func (s *sqliteStore) InsertSandbox(id, template, teamID string) error {
	_, err := s.db.Exec(
		`INSERT INTO sandboxes (id, template, status, team_id) VALUES (?, ?, 'running', ?)`,
		id, template, teamID)
	return err
}

// SandboxTeam returns the owning team of a sandbox, and whether it exists. The api uses it to
// authorise a delete before tearing the VM down; a missing row is ("", false, nil).
func (s *sqliteStore) SandboxTeam(id string) (string, bool, error) {
	var team string
	err := s.db.QueryRow(`SELECT team_id FROM sandboxes WHERE id = ?`, id).Scan(&team)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return team, true, nil
}

// DeleteSandbox removes a sandbox record. Idempotent: deleting an absent id is not an error
// (DELETE simply affects zero rows). Unscoped: the api checks ownership via SandboxTeam first.
func (s *sqliteStore) DeleteSandbox(id string) error {
	_, err := s.db.Exec(`DELETE FROM sandboxes WHERE id = ?`, id)
	return err
}

// ListSandboxes returns a team's sandbox records, newest first.
func (s *sqliteStore) ListSandboxes(teamID string) ([]Sandbox, error) {
	rows, err := s.db.Query(
		`SELECT id, template, status, team_id, created_at FROM sandboxes WHERE team_id = ? ORDER BY created_at DESC`,
		teamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Sandbox
	for rows.Next() {
		var sb Sandbox
		// SQLite returns created_at as text, so it scans straight into a string.
		if err := rows.Scan(&sb.ID, &sb.Template, &sb.Status, &sb.TeamID, &sb.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, sb)
	}
	return out, rows.Err()
}

// InsertBuild records a newly started build owned by teamID. state starts as "building";
// created_at is filled by SQLite. The api inserts this when the orchestrator accepts a TemplateCreate.
func (s *sqliteStore) InsertBuild(buildID, name, teamID string) error {
	_, err := s.db.Exec(
		`INSERT INTO builds (build_id, name, state, team_id) VALUES (?, ?, 'building', ?)`,
		buildID, name, teamID)
	return err
}

// BuildTeam returns the owning team of a build, and whether it exists (the api authorises a
// status read with it). A missing row is ("", false, nil).
func (s *sqliteStore) BuildTeam(buildID string) (string, bool, error) {
	var team string
	err := s.db.QueryRow(`SELECT team_id FROM builds WHERE build_id = ?`, buildID).Scan(&team)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return team, true, nil
}

// UpdateBuild records the latest state/detail of a build (the api calls it as it polls the
// orchestrator). Updating an absent build affects zero rows, which is not an error.
func (s *sqliteStore) UpdateBuild(buildID, state, detail string) error {
	_, err := s.db.Exec(
		`UPDATE builds SET state = ?, detail = ? WHERE build_id = ?`, state, detail, buildID)
	return err
}

// ListBuilds returns a team's build records, newest first.
func (s *sqliteStore) ListBuilds(teamID string) ([]Build, error) {
	rows, err := s.db.Query(
		`SELECT build_id, name, state, detail, team_id, created_at FROM builds WHERE team_id = ? ORDER BY created_at DESC`,
		teamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Build
	for rows.Next() {
		var b Build
		if err := rows.Scan(&b.BuildID, &b.Name, &b.State, &b.Detail, &b.TeamID, &b.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ResolveAPIKey maps a hashed key to its team. A miss is ("", false, nil) -- a genuine "no
// such key", which the api turns into a 401; a DB failure is ("", false, err), distinct so the
// api can answer 500 instead of wrongly rejecting a valid key.
func (s *sqliteStore) ResolveAPIKey(keyHash string) (string, bool, error) {
	var team string
	err := s.db.QueryRow(`SELECT team_id FROM api_keys WHERE key_hash = ?`, keyHash).Scan(&team)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return team, true, nil
}

// EnsureTeam creates a team if absent (idempotent via INSERT OR IGNORE). The api calls it on
// startup to seed the default team.
func (s *sqliteStore) EnsureTeam(id, name string) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO teams (id, name) VALUES (?, ?)`, id, name)
	return err
}

// InsertAPIKey registers a hashed key for a team if absent (idempotent). Re-seeding the same
// dev key on every startup is a no-op.
func (s *sqliteStore) InsertAPIKey(keyHash, teamID string) error {
	_, err := s.db.Exec(
		`INSERT OR IGNORE INTO api_keys (key_hash, team_id) VALUES (?, ?)`, keyHash, teamID)
	return err
}
