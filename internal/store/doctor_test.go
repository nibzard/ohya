package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/mattn/go-sqlite3"
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
	err = ro.FTSIndexConsistent(context.Background())
	if !errors.Is(err, ErrFTSIndexInconsistent) {
		t.Fatalf("corrupt index reported %v; want ErrFTSIndexInconsistent", err)
	}
	var sqliteErr sqlite3.Error
	if !errors.As(err, &sqliteErr) || sqliteErr.Code != sqlite3.ErrCorrupt {
		t.Fatalf("corrupt index reported %v; want the SQLite corrupt code", err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFTSIndexConsistentFinishesWhileAnotherConnectionCommits(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.db")
	w, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	seedOneIndexedEvent(t, w.Writer())
	if _, err := w.Writer().Exec(`CREATE TABLE integrity_probe (padding TEXT); INSERT INTO integrity_probe VALUES (hex(zeroblob(33554432)))`); err != nil {
		t.Fatal(err)
	}

	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()

	writer := rawSQLite(t, path)
	if _, err := writer.Exec(`PRAGMA synchronous = OFF`); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	commits := make(chan int, 1)
	go func() {
		n := 0
		for {
			select {
			case <-stop:
				commits <- n
				return
			default:
			}
			if _, err := writer.Exec(`INSERT INTO integrity_probe VALUES ('x')`); err != nil {
				t.Error(err)
				commits <- n
				return
			}
			n++
			time.Sleep(100 * time.Microsecond)
		}
	}()
	err = ro.FTSIndexConsistent(ctx)
	close(stop)
	written := <-commits
	if err != nil {
		t.Fatalf("check with %d commits during the copy: %v", written, err)
	}
	if written == 0 {
		t.Fatal("no commit landed during the check")
	}
}

func TestFTSIndexConsistentRespectsCanceledContext(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.db")
	w, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	seedOneIndexedEvent(t, w.Writer())

	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ro.FTSIndexConsistent(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("FTSIndexConsistent with canceled ctx = %v, want context.Canceled", err)
	}
}
