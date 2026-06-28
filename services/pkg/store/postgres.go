package store

import (
	"database/sql"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the pure-Go "pgx" database/sql driver
)

// postgresStore is the Postgres implementation of Store and, since Stage 14b, the default
// backend -- matching E2B, whose api persists to Postgres. It is the same six statements as
// sqlite.go behind the same interface; the only differences are the three SQL-dialect points
// the doc calls out, marked inline below. The driver is jackc/pgx via its database/sql
// "stdlib" adapter, which is pure Go, so every host binary stays statically linkable (the
// same reason pkg/store chose modernc.org/sqlite). See docs/STAGE14_DESIGN.md §3.2.
type postgresStore struct {
	db *sql.DB
}

// postgresStore must satisfy Store.
var _ Store = (*postgresStore)(nil)

// openPostgres connects to the Postgres named by dsn (a "postgres://…" URL) and ensures the
// schema is present. Unlike SQLite there is no single-writer cap: Postgres has real MVCC
// concurrency, so we use the default connection pool (dialect difference #2).
func openPostgres(dsn string) (Store, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	// sql.Open is lazy; Ping forces a real connection now so a misconfigured DSN or a
	// not-yet-ready server fails here at startup rather than on the first request.
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	// Postgres's extended protocol rejects multiple commands in one Exec, so apply the
	// schema one statement at a time (sqlite.go runs the whole string at once).
	for _, stmt := range splitSchema() {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, errSchema(err)
		}
	}
	// Stage 16: add team_id to the pre-existing tables. Postgres spells the idempotent add as
	// ADD COLUMN IF NOT EXISTS (sqlite.go has to PRAGMA-check first, lacking that clause).
	for _, table := range teamColumnTables {
		if _, err := db.Exec(
			`ALTER TABLE ` + table + ` ADD COLUMN IF NOT EXISTS team_id TEXT NOT NULL DEFAULT 'default'`); err != nil {
			db.Close()
			return nil, errSchema(err)
		}
	}
	return &postgresStore{db: db}, nil
}

func (s *postgresStore) Close() error { return s.db.Close() }

// InsertSandbox records a newly created sandbox owned by teamID. Dialect difference #1:
// Postgres uses positional $N placeholders where SQLite uses ?. A duplicate id violates the
// primary key and returns an error.
func (s *postgresStore) InsertSandbox(id, template, teamID string) error {
	_, err := s.db.Exec(
		`INSERT INTO sandboxes (id, template, status, team_id) VALUES ($1, $2, 'running', $3)`,
		id, template, teamID)
	return err
}

// SandboxTeam returns the owning team of a sandbox, and whether it exists (the api authorises
// a delete with it). A missing row is ("", false, nil).
func (s *postgresStore) SandboxTeam(id string) (string, bool, error) {
	var team string
	err := s.db.QueryRow(`SELECT team_id FROM sandboxes WHERE id = $1`, id).Scan(&team)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return team, true, nil
}

// DeleteSandbox removes a sandbox record. Idempotent: deleting an absent id affects zero
// rows, which is not an error. Unscoped: the api checks ownership via SandboxTeam first.
func (s *postgresStore) DeleteSandbox(id string) error {
	_, err := s.db.Exec(`DELETE FROM sandboxes WHERE id = $1`, id)
	return err
}

// ListSandboxes returns a team's sandbox records, newest first. Dialect difference #3:
// Postgres returns created_at as a timestamptz, so it scans into a time.Time, which we format
// to RFC3339 -- keeping Sandbox.CreatedAt a string, so the api's JSON shape is unchanged.
func (s *postgresStore) ListSandboxes(teamID string) ([]Sandbox, error) {
	rows, err := s.db.Query(
		`SELECT id, template, status, team_id, created_at FROM sandboxes WHERE team_id = $1 ORDER BY created_at DESC`,
		teamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Sandbox
	for rows.Next() {
		var sb Sandbox
		var created time.Time
		if err := rows.Scan(&sb.ID, &sb.Template, &sb.Status, &sb.TeamID, &created); err != nil {
			return nil, err
		}
		sb.CreatedAt = created.Format(time.RFC3339)
		out = append(out, sb)
	}
	return out, rows.Err()
}

// InsertBuild records a newly started build (state "building") owned by teamID.
func (s *postgresStore) InsertBuild(buildID, name, teamID string) error {
	_, err := s.db.Exec(
		`INSERT INTO builds (build_id, name, state, team_id) VALUES ($1, $2, 'building', $3)`,
		buildID, name, teamID)
	return err
}

// BuildTeam returns the owning team of a build, and whether it exists. A missing row is
// ("", false, nil).
func (s *postgresStore) BuildTeam(buildID string) (string, bool, error) {
	var team string
	err := s.db.QueryRow(`SELECT team_id FROM builds WHERE build_id = $1`, buildID).Scan(&team)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return team, true, nil
}

// UpdateBuild records the latest state/detail of a build. Updating an absent build affects
// zero rows, which is not an error.
func (s *postgresStore) UpdateBuild(buildID, state, detail string) error {
	_, err := s.db.Exec(
		`UPDATE builds SET state = $1, detail = $2 WHERE build_id = $3`, state, detail, buildID)
	return err
}

// ListBuilds returns a team's build records, newest first (created_at formatted as in ListSandboxes).
func (s *postgresStore) ListBuilds(teamID string) ([]Build, error) {
	rows, err := s.db.Query(
		`SELECT build_id, name, state, detail, team_id, created_at FROM builds WHERE team_id = $1 ORDER BY created_at DESC`,
		teamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Build
	for rows.Next() {
		var b Build
		var created time.Time
		if err := rows.Scan(&b.BuildID, &b.Name, &b.State, &b.Detail, &b.TeamID, &created); err != nil {
			return nil, err
		}
		b.CreatedAt = created.Format(time.RFC3339)
		out = append(out, b)
	}
	return out, rows.Err()
}

// ResolveAPIKey maps a hashed key to its team. A miss is ("", false, nil) -- a genuine "no
// such key" the api turns into a 401; a DB failure is ("", false, err), distinct so the api
// can answer 500 rather than wrongly reject a valid key.
func (s *postgresStore) ResolveAPIKey(keyHash string) (string, bool, error) {
	var team string
	err := s.db.QueryRow(`SELECT team_id FROM api_keys WHERE key_hash = $1`, keyHash).Scan(&team)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return team, true, nil
}

// EnsureTeam creates a team if absent (idempotent via ON CONFLICT DO NOTHING).
func (s *postgresStore) EnsureTeam(id, name string) error {
	_, err := s.db.Exec(
		`INSERT INTO teams (id, name) VALUES ($1, $2) ON CONFLICT (id) DO NOTHING`, id, name)
	return err
}

// InsertAPIKey registers a hashed key for a team if absent (idempotent). Re-seeding the same
// dev key on every startup is a no-op.
func (s *postgresStore) InsertAPIKey(keyHash, teamID string) error {
	_, err := s.db.Exec(
		`INSERT INTO api_keys (key_hash, team_id) VALUES ($1, $2) ON CONFLICT (key_hash) DO NOTHING`,
		keyHash, teamID)
	return err
}
