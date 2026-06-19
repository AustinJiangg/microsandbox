package store

import (
	"path/filepath"
	"testing"
)

// CRUD against a real SQLite file in a temp dir -- no VM/KVM, so it runs anywhere Go
// is installed (the cgo-free modernc.org/sqlite driver needs no C toolchain either).

func TestStoreCRUD(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	// A fresh database lists nothing.
	rows, err := st.ListSandboxes()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("fresh db: want 0 rows, got %d", len(rows))
	}

	// Insert two; each gets status "running" and a non-empty created_at from the DB.
	if err := st.InsertSandbox("sb_1", "default"); err != nil {
		t.Fatalf("insert sb_1: %v", err)
	}
	if err := st.InsertSandbox("sb_2", "ml-env"); err != nil {
		t.Fatalf("insert sb_2: %v", err)
	}
	rows, err = st.ListSandboxes()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	for _, sb := range rows {
		if sb.Status != "running" {
			t.Errorf("%s: status = %q, want running", sb.ID, sb.Status)
		}
		if sb.CreatedAt == "" {
			t.Errorf("%s: empty created_at", sb.ID)
		}
	}

	// Delete one; the other remains.
	if err := st.DeleteSandbox("sb_1"); err != nil {
		t.Fatalf("delete sb_1: %v", err)
	}
	rows, _ = st.ListSandboxes()
	if len(rows) != 1 || rows[0].ID != "sb_2" {
		t.Fatalf("after delete: %+v", rows)
	}

	// Deleting an absent id is idempotent (not an error).
	if err := st.DeleteSandbox("sb_missing"); err != nil {
		t.Errorf("idempotent delete errored: %v", err)
	}

	// A duplicate id violates the primary key.
	if err := st.InsertSandbox("sb_2", "x"); err == nil {
		t.Error("duplicate insert: want a primary-key error, got nil")
	}
}

func TestStoreBuildsCRUD(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	if rows, err := st.ListBuilds(); err != nil || len(rows) != 0 {
		t.Fatalf("fresh db: want 0 builds (err=%v), got %d", err, len(rows))
	}

	// Insert starts in "building"; created_at comes from the DB.
	if err := st.InsertBuild("bld_1", "demo"); err != nil {
		t.Fatalf("insert bld_1: %v", err)
	}
	rows, err := st.ListBuilds()
	if err != nil || len(rows) != 1 {
		t.Fatalf("want 1 build (err=%v), got %d", err, len(rows))
	}
	if rows[0].State != "building" || rows[0].Name != "demo" || rows[0].CreatedAt == "" {
		t.Fatalf("unexpected build row: %+v", rows[0])
	}

	// Update records the terminal state + detail.
	if err := st.UpdateBuild("bld_1", "failed", "docker build: boom"); err != nil {
		t.Fatalf("update bld_1: %v", err)
	}
	rows, _ = st.ListBuilds()
	if rows[0].State != "failed" || rows[0].Detail != "docker build: boom" {
		t.Fatalf("after update: %+v", rows[0])
	}

	// Updating an absent build is idempotent (zero rows affected, not an error).
	if err := st.UpdateBuild("bld_missing", "success", ""); err != nil {
		t.Errorf("idempotent update errored: %v", err)
	}

	// A duplicate build id violates the primary key.
	if err := st.InsertBuild("bld_1", "demo"); err == nil {
		t.Error("duplicate build insert: want a primary-key error, got nil")
	}
}
