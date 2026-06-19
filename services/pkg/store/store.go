// Package store is the api's durable metadata: which sandboxes exist, on which
// template, since when. It mirrors E2B's api-owned database (Postgres there): the
// orchestrator keeps the *live* VM registry in memory, while the api persists the
// authoritative record that survives restarts. Stage 8c backs it with SQLite via the
// cgo-free modernc.org/sqlite driver, so the api stays a single static binary. See
// docs/STAGE8_DESIGN.md.
//
// E2B generates its query layer with sqlc; for one table and three statements we write
// plain database/sql, which is more transparent here. The Store type is the seam --
// swapping SQLite for Postgres (or these queries for sqlc) is a change behind it, not
// above it.
package store

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite" // registers the cgo-free "sqlite" database/sql driver
)

// Sandbox is one persisted row.
type Sandbox struct {
	ID        string
	Template  string
	Status    string
	CreatedAt string // text, as SQLite stores CURRENT_TIMESTAMP
}

// Store is a thin typed wrapper over the SQLite database.
type Store struct {
	db *sql.DB
}

// schema is applied on Open; IF NOT EXISTS makes it safe to run every startup (a poor
// man's migration -- a real one would version these). The builds table arrives with the
// TemplateService (Stage 10): the api records each template build and its outcome, like
// E2B's api owns templates/builds in Postgres.
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

// Open opens (creating the file if needed) the SQLite database at path and ensures the
// schema is present.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// SQLite allows only one writer at a time; capping the pool at a single connection
	// serializes all access through database/sql and sidesteps "database is locked"
	// under concurrent requests. Our request volume is tiny, so this costs nothing.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("ensuring schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// InsertSandbox records a newly created sandbox. status starts as "running"; created_at
// is filled by SQLite's CURRENT_TIMESTAMP. A duplicate id violates the primary key and
// returns an error (ids are random, so this should never happen in practice).
func (s *Store) InsertSandbox(id, template string) error {
	_, err := s.db.Exec(
		`INSERT INTO sandboxes (id, template, status) VALUES (?, ?, 'running')`,
		id, template)
	return err
}

// DeleteSandbox removes a sandbox record. Idempotent: deleting an absent id is not an
// error (DELETE simply affects zero rows).
func (s *Store) DeleteSandbox(id string) error {
	_, err := s.db.Exec(`DELETE FROM sandboxes WHERE id = ?`, id)
	return err
}

// ListSandboxes returns all sandbox records, newest first.
func (s *Store) ListSandboxes() ([]Sandbox, error) {
	rows, err := s.db.Query(
		`SELECT id, template, status, created_at FROM sandboxes ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Sandbox
	for rows.Next() {
		var sb Sandbox
		if err := rows.Scan(&sb.ID, &sb.Template, &sb.Status, &sb.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, sb)
	}
	return out, rows.Err()
}

// Build is one persisted template-build row.
type Build struct {
	BuildID   string
	Name      string
	State     string // building | success | failed
	Detail    string
	CreatedAt string
}

// InsertBuild records a newly started build. state starts as "building"; created_at is
// filled by SQLite. The api inserts this when the orchestrator accepts a TemplateCreate.
func (s *Store) InsertBuild(buildID, name string) error {
	_, err := s.db.Exec(
		`INSERT INTO builds (build_id, name, state) VALUES (?, ?, 'building')`, buildID, name)
	return err
}

// UpdateBuild records the latest state/detail of a build (the api calls it as it polls the
// orchestrator). Updating an absent build affects zero rows, which is not an error.
func (s *Store) UpdateBuild(buildID, state, detail string) error {
	_, err := s.db.Exec(
		`UPDATE builds SET state = ?, detail = ? WHERE build_id = ?`, state, detail, buildID)
	return err
}

// ListBuilds returns all build records, newest first.
func (s *Store) ListBuilds() ([]Build, error) {
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
