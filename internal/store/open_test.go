package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// queryer is the read surface shared by pools and single connections.
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func openTestDB(t *testing.T) (*DB, *sql.DB) {
	t.Helper()
	hub, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hub.Close() })
	return hub, hub.Writer()
}

// checkConnectionPragmas asserts the settings every connection must carry.
func checkConnectionPragmas(t *testing.T, ctx context.Context, db queryer) {
	t.Helper()
	var journal string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journal); err != nil || journal != "wal" {
		t.Fatalf("journal_mode = %q, %v; want wal", journal, err)
	}
	var foreignKeys int
	if err := db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		t.Fatalf("foreign_keys = %d, %v; want 1", foreignKeys, err)
	}
	var busyTimeout int
	if err := db.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil || busyTimeout != busyTimeoutMs {
		t.Fatalf("busy_timeout = %d, %v; want %d", busyTimeout, err, busyTimeoutMs)
	}
}

func TestWriterIsSingleConnectionWithDurablePragmas(t *testing.T) {
	_, writer := openTestDB(t)
	if got := writer.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("writer allows %d connections, want exactly 1", got)
	}
	var synchronous int
	if err := writer.QueryRow(`PRAGMA synchronous`).Scan(&synchronous); err != nil || synchronous != synchronousFull {
		t.Fatalf("writer synchronous = %d, %v; want FULL (%d)", synchronous, err, synchronousFull)
	}
	checkConnectionPragmas(t, context.Background(), writer)
}

func TestReaderConnectionsAreRestricted(t *testing.T) {
	hub, _ := openTestDB(t)
	readers, err := hub.Reader()
	if err != nil {
		t.Fatal(err)
	}
	defer readers.Close()

	// Hold three connections open at once (one per probe below); each must
	// carry the settings and reject writes.
	ctx := context.Background()
	var conns []*sql.Conn
	for range 3 {
		conn, err := readers.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conns = append(conns, conn)
		checkConnectionPragmas(t, ctx, conn)
	}
	for i, query := range []string{
		`CREATE TABLE x(a)`,
		`INSERT INTO sessions(id,agent,device_id,native_id) VALUES('s','a','d','n')`,
		`PRAGMA journal_mode = DELETE`,
	} {
		if _, err := conns[i].ExecContext(ctx, query); err == nil {
			t.Fatalf("reader connection accepted %q", query)
		}
	}
	var versions int
	if err := readers.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&versions); err != nil || versions != 5 {
		t.Fatalf("reader saw %d applied migrations, %v; want 5", versions, err)
	}
}

func TestReaderCannotCreateDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.db")
	db, err := sql.Open("sqlite3", readerDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := verifyReader(db); err == nil {
		t.Fatal("read-only connection opened a nonexistent database")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("read-only open created the database file")
	}
}

func TestOpenAppliesRealMigrationsAndReopensCleanly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	hub, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	writer := hub.Writer()
	for _, q := range []string{
		`INSERT INTO sessions(id,agent,device_id,native_id) VALUES('s','agent','d','n')`,
		`INSERT INTO sources(id,device_id,agent,path) VALUES('f','d','agent','log')`,
	} {
		if _, err := writer.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err := hub.Close(); err != nil {
		t.Fatal(err)
	}

	// A later open finds the schema current, applies nothing, and sees the
	// committed data (the restart path).
	hub, err = Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	var sessions, versions int
	if err := hub.Writer().QueryRow(`SELECT count(*) FROM sessions`).Scan(&sessions); err != nil || sessions != 1 {
		t.Fatalf("sessions after reopen = %d, %v; want 1", sessions, err)
	}
	if err := hub.Writer().QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&versions); err != nil || versions != 5 {
		t.Fatalf("applied migrations after reopen = %d, %v; want 5", versions, err)
	}
}

func TestFTSConsistencyThroughReader(t *testing.T) {
	hub, writer := openTestDB(t)
	for _, q := range []string{
		`INSERT INTO sessions(id,agent,device_id,native_id) VALUES('s','agent','d','n')`,
		`INSERT INTO sources(id,device_id,agent,path) VALUES('f','d','agent','log')`,
		`INSERT INTO events(id,session_id,source_id,source_offset,kind,raw_json,text) VALUES(1,'s','f',0,'message','{}','hello world')`,
		`INSERT INTO events(id,session_id,source_id,source_offset,kind,raw_json,text) VALUES(2,'s','f',12,'message','{}',NULL)`,
		`INSERT INTO events(id,session_id,source_id,source_offset,kind,raw_json,text) VALUES(3,'s','f',30,'message','{}','')`,
		`INSERT INTO events(id,session_id,source_id,source_offset,kind,raw_json,text) VALUES(4,'s','f',31,'message','{}','authentication refresh token')`,
	} {
		if _, err := writer.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	readers, err := hub.Reader()
	if err != nil {
		t.Fatal(err)
	}
	defer readers.Close()

	match := func(term string) int {
		var n int
		if err := readers.QueryRow(`SELECT count(*) FROM events_fts WHERE events_fts MATCH ?`, term).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := match("hello"); n != 1 {
		t.Fatalf("MATCH hello = %d, want 1", n)
	}
	if n := match("authentication"); n != 1 {
		t.Fatalf("MATCH authentication = %d, want 1", n)
	}
	if n := match("token"); n != 1 {
		t.Fatalf("MATCH token = %d, want 1", n)
	}

	// Deleting an event removes its index entry through the trigger.
	if _, err := writer.Exec(`DELETE FROM events WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if n := match("hello"); n != 0 {
		t.Fatalf("MATCH hello after delete = %d, want 0", n)
	}

	// The external-content index still matches the events table exactly.
	if _, err := writer.Exec(`INSERT INTO events_fts(events_fts) VALUES ('integrity-check')`); err != nil {
		t.Fatalf("FTS integrity-check failed: %v", err)
	}
	// Only events with non-empty text carry index entries. count(*) on an
	// external-content table reads the content table, so enumerate through
	// MATCH instead and expect exactly the surviving indexed documents.
	rows, err := readers.Query(`SELECT rowid FROM events_fts WHERE events_fts MATCH 'hello OR world OR authentication OR refresh OR token' ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var indexed []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		indexed = append(indexed, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(indexed, []int{4}) {
		t.Fatalf("indexed rowids = %v, want [4] (NULL and empty text are never indexed, id 1 deleted)", indexed)
	}
}
