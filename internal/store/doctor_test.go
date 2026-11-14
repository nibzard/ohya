package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func rawSQLite(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestOpenReadOnlyDoesNotMigrate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.db")
	w, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	schema, err := SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	raw := rawSQLite(t, path)
	if _, err := raw.Exec(`DELETE FROM schema_migrations WHERE version = ?`, schema); err != nil {
		t.Fatal(err)
	}

	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if ro.Writer() != nil {
		t.Fatal("a read-only DB holds a writer")
	}
	version, found, err := ro.AppliedSchemaVersion(context.Background())
	if err != nil || !found || version != schema-1 {
		t.Fatalf("applied schema = %d, found %t, err %v; want %d behind the binary", version, found, err, schema)
	}
	var n int
	if err := raw.QueryRow(`SELECT count(*) FROM schema_migrations WHERE version = ?`, schema).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("migration %d applied by OpenReadOnly; it must never migrate", schema)
	}
}

func TestAppliedSchemaVersionRejectsForeignDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foreign.db")
	raw := rawSQLite(t, path)
	if _, err := raw.Exec(`CREATE TABLE other (x)`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	if _, err := rawSQLite(t, path).Exec(`PRAGMA journal_mode = WAL`); err != nil {
		t.Fatal(err)
	}
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if _, _, err := ro.AppliedSchemaVersion(context.Background()); !errors.Is(err, ErrNoSchemaMigrations) {
		t.Fatalf("error = %v; want ErrNoSchemaMigrations", err)
	}
}

func seedOneIndexedEvent(t *testing.T, db *sql.DB) {
	t.Helper()
	stmts := []string{
		`INSERT INTO sources (id, device_id, agent, path) VALUES ('s1', 'd1', 'claude-code', '/p')`,
		`INSERT INTO sessions (id, agent, device_id, native_id) VALUES ('x:claude-code:n', 'claude-code', 'd1', 'n')`,
		`INSERT INTO events (session_id, source_id, source_offset, kind, text, raw_json)
		 VALUES ('x:claude-code:n', 's1', 0, 'user', 'hello world', '{}')`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFTSIndexConsistentDetectsCorruption(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.db")
	w, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	seedOneIndexedEvent(t, w.Writer())

	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if err := ro.FTSIndexConsistent(context.Background()); err != nil {
		t.Fatalf("consistent index reported: %v", err)
	}

	if _, err := w.Writer().Exec(`INSERT INTO events_fts(events_fts) VALUES ('delete-all')`); err != nil {
		t.Fatal(err)
	}
	if err := ro.FTSIndexConsistent(context.Background()); err == nil {
		t.Fatal("corrupt index passed the integrity-check")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}
