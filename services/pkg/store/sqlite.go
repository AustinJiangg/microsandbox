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
	return &sqliteStore{db: db}, nil
}

func (s *sqliteStore) Close() error { return s.db.Close() }

// InsertSandbox records a newly created sandbox. status starts as "running"; created_at is
// filled by SQLite's CURRENT_TIMESTAMP. A duplicate id violates the primary key and returns
// an error (ids are random, so this should never happen in practice).
func (s *sqliteStore) InsertSandbox(id, template string) error {
	_, err := s.db.Exec(
		`INSERT INTO sandboxes (id, template, status) VALUES (?, ?, 'running')`,
		id, template)
	return err
}

// DeleteSandbox removes a sandbox record. Idempotent: deleting an absent id is not an error
// (DELETE simply affects zero rows).
func (s *sqliteStore) DeleteSandbox(id string) error {
	_, err := s.db.Exec(`DELETE FROM sandboxes WHERE id = ?`, id)
	return err
}

// ListSandboxes returns all sandbox records, newest first.
func (s *sqliteStore) ListSandboxes() ([]Sandbox, error) {
	rows, err := s.db.Query(
		`SELECT id, template, status, created_at FROM sandboxes ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Sandbox
	for rows.Next() {
		var sb Sandbox
		// SQLite returns created_at as text, so it scans straight into a string.
		if err := rows.Scan(&sb.ID, &sb.Template, &sb.Status, &sb.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, sb)
	}
	return out, rows.Err()
}

// InsertBuild records a newly started build. state starts as "building"; created_at is
// filled by SQLite. The api inserts this when the orchestrator accepts a TemplateCreate.
func (s *sqliteStore) InsertBuild(buildID, name string) error {
	_, err := s.db.Exec(
		`INSERT INTO builds (build_id, name, state) VALUES (?, ?, 'building')`, buildID, name)
	return err
}

// UpdateBuild records the latest state/detail of a build (the api calls it as it polls the
// orchestrator). Updating an absent build affects zero rows, which is not an error.
func (s *sqliteStore) UpdateBuild(buildID, state, detail string) error {
	_, err := s.db.Exec(
		`UPDATE builds SET state = ?, detail = ? WHERE build_id = ?`, state, detail, buildID)
	return err
}

// ListBuilds returns all build records, newest first.
func (s *sqliteStore) ListBuilds() ([]Build, error) {
	rows, err := s.db.Query(
		`SELECT build_id, name, state, detail, created_at FROM builds ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Build
	for rows.Next() {
		var b Build
		if err := rows.Scan(&b.BuildID, &b.Name, &b.State, &b.Detail, &b.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}
