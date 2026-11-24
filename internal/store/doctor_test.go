package store

import (
	"context"
	"database/sql"
	"errors"
	"maps"
	"path/filepath"
	"strings"
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

func TestMaxParserVersionsReadsTheNewestVersionOfEachSource(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "hub.db")
	w, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for _, q := range []string{
		`INSERT INTO sources (id, device_id, agent, path) VALUES ('s-a', 'd1', 'claude-code', '/a')`,
		`INSERT INTO sources (id, device_id, agent, path) VALUES ('s-b', 'd1', 'claude-code', '/b')`,
		`INSERT INTO sources (id, device_id, agent, path) VALUES ('s-c', 'd1', 'codex', '/c')`,
		`INSERT INTO sources (id, device_id, agent, path) VALUES ('s-idle', 'd1', 'pi', '/idle')`,
		`INSERT INTO sessions (id, agent, device_id, native_id) VALUES ('x:claude-code:n', 'claude-code', 'd1', 'n')`,
		`INSERT INTO sessions (id, agent, device_id, native_id) VALUES ('x:codex:n', 'codex', 'd1', 'n')`,
		`INSERT INTO events (session_id, source_id, source_offset, kind, raw_json, parser_version) VALUES
			('x:claude-code:n', 's-a', 0, 'user', '{}', 1),
			('x:claude-code:n', 's-a', 1, 'user', '{}', 3),
			('x:claude-code:n', 's-b', 0, 'user', '{}', 2),
			('x:codex:n', 's-c', 0, 'response_item', '{}', 4)`,
	} {
		if _, err := w.Writer().ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	got, err := ro.MaxParserVersions(ctx)
	if err != nil || !maps.Equal(got, map[string]int{"claude-code": 3, "codex": 4}) {
		t.Fatalf("parser versions = %v, %v; want the newest version of each agent and no entry for a source without events", got, err)
	}
	plan, indexed := explainParserVersions(t, ro.readers)
	if !indexed {
		t.Fatalf("the query does not read the index events_source_parser_version:\n%s", strings.Join(plan, "\n"))
	}
}

func explainParserVersions(t *testing.T, db *sql.DB) (plan []string, indexed bool) {
	t.Helper()
	rows, err := db.Query(`EXPLAIN QUERY PLAN ` + parserVersionsQuery)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var node, parent, unused int
		var detail string
		if err := rows.Scan(&node, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
		indexed = indexed || strings.Contains(detail, "events_source_parser_version")
	}
	return plan, indexed
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
