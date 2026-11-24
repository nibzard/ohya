package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"
)

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
	wantVersions, err := SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	var versions int
	if err := readers.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&versions); err != nil || versions != wantVersions {
		t.Fatalf("reader saw %d applied migrations, %v; want %d", versions, err, wantVersions)
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

	hub, err = Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	wantVersions, err := SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	var sessions, versions int
	if err := hub.Writer().QueryRow(`SELECT count(*) FROM sessions`).Scan(&sessions); err != nil || sessions != 1 {
		t.Fatalf("sessions after reopen = %d, %v; want 1", sessions, err)
	}
	if err := hub.Writer().QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&versions); err != nil || versions != wantVersions {
		t.Fatalf("applied migrations after reopen = %d, %v; want %d", versions, err, wantVersions)
	}
}

func TestConcurrentOpenAppliesEachMigrationOnce(t *testing.T) {
	want, err := SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 6; round++ {
		path := filepath.Join(t.TempDir(), "hub.db")
		const openers = 6
		start := make(chan struct{})
		errs := make([]error, openers)
		var wg sync.WaitGroup
		for i := range errs {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				db, err := Open(context.Background(), path)
				if err != nil {
					errs[i] = err
					return
				}
				errs[i] = db.Close()
			}(i)
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("round %d: opener %d: %v", round, i, err)
			}
		}
		hub, err := Open(context.Background(), path)
		if err != nil {
			t.Fatalf("round %d: reopen: %v", round, err)
		}
		var versions, newest int
		err = hub.Writer().QueryRow(`SELECT count(*), coalesce(max(version), 0) FROM schema_migrations`).Scan(&versions, &newest)
		hub.Close()
		if err != nil || versions != want || newest != want {
			t.Fatalf("round %d: applied = %d through %d, %v; want %d through %d", round, versions, newest, err, want, want)
		}
	}
}

func busyError() error {
	return sqlite3.Error{Code: sqlite3.ErrBusy}
}

func TestRetryBusyRepeatsABusyAttempt(t *testing.T) {
	attempts := 0
	err := retryBusy(context.Background(), time.Second, func() error {
		attempts++
		if attempts < 3 {
			return busyError()
		}
		return nil
	})
	if err != nil || attempts != 3 {
		t.Fatalf("err = %v after %d attempts, want nil after 3", err, attempts)
	}
}

func TestRetryBusyReturnsOtherErrorsAtOnce(t *testing.T) {
	want := errors.New("store: migration 002_bad: no such table: missing")
	attempts := 0
	err := retryBusy(context.Background(), time.Second, func() error {
		attempts++
		return want
	})
	if !errors.Is(err, want) || attempts != 1 {
		t.Fatalf("err = %v after %d attempts, want the first error after 1", err, attempts)
	}
}

func TestRetryBusyGivesUpAfterTheWait(t *testing.T) {
	attempts := 0
	started := time.Now()
	err := retryBusy(context.Background(), 150*time.Millisecond, func() error {
		attempts++
		return busyError()
	})
	if !busyDatabase(err) || attempts < 2 || time.Since(started) < 150*time.Millisecond {
		t.Fatalf("err = %v after %d attempts in %s, want the busy error after a 150 ms wait", err, attempts, time.Since(started))
	}
}

func TestRetryBusyStopsWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempts := 0
	err := retryBusy(ctx, time.Second, func() error {
		attempts++
		cancel()
		return busyError()
	})
	if !errors.Is(err, context.Canceled) || attempts != 1 {
		t.Fatalf("err = %v after %d attempts, want context.Canceled after 1", err, attempts)
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

	if _, err := writer.Exec(`DELETE FROM events WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if n := match("hello"); n != 0 {
		t.Fatalf("MATCH hello after delete = %d, want 0", n)
	}

	if _, err := writer.Exec(`INSERT INTO events_fts(events_fts) VALUES ('integrity-check')`); err != nil {
		t.Fatalf("FTS integrity-check failed: %v", err)
	}
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
