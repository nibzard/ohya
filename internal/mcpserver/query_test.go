package mcpserver

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nibzard/ohya/internal/store"
)

// openSeededHub opens a hub database with known rows and returns the hub and
// its reader pool; cleanup closes both.
func openSeededHub(t *testing.T) (*store.DB, *sql.DB) {
	t.Helper()
	hub, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hub.Close() })
	seed := []string{
		`INSERT INTO sessions(id,agent,device_id,native_id,first_prompt,cwd,model)
		 VALUES('claude:dev1:sess-1','claude','dev1','sess-1','Find primes','/repo','m1')`,
		`INSERT INTO sources(id,device_id,agent,path) VALUES('src-1','dev1','claude','/repo/a.jsonl')`,
		`INSERT INTO events(id,session_id,source_id,source_offset,time_ms,kind,role,text,raw_json)
		 VALUES(1,'claude:dev1:sess-1','src-1',0,1000,'message','user','hello authentication world','{}'),
		       (2,'claude:dev1:sess-1','src-1',50,2000,'message','assistant','refresh the token','{}')`,
		`INSERT INTO session_token_usage VALUES('claude:dev1:sess-1','msg-1',10,20,30,40)`,
		`INSERT INTO event_annotations VALUES(1,'security','review','{"status":"reviewed"}','dev1',1,1)`,
		`CREATE VIEW seeded_view AS SELECT 1`,
	}
	for _, q := range seed {
		if _, err := hub.Writer().Exec(q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	readers, err := hub.Reader()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { readers.Close() })
	return hub, readers
}

// newTestEngine returns an engine on a freshly seeded hub, writing as dev1.
func newTestEngine(t *testing.T, o EngineOptions) *Engine {
	t.Helper()
	hub, readers := openSeededHub(t)
	o.WriterID = "dev1"
	return NewEngine(readers, hub.Writer(), o)
}

// mustQuery runs a query expected to succeed and fails the test otherwise.
func mustQuery(t *testing.T, e *Engine, query string) Result {
	t.Helper()
	result, err := e.Query(context.Background(), query)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return result
}

func TestQuerySelectsAndSchemaInspection(t *testing.T) {
	e := newTestEngine(t, EngineOptions{})

	r := mustQuery(t, e, `SELECT 1 AS one, NULL AS nul, x'4142' AS blob, 9e999 AS inf`)
	if got := fmt.Sprint(r.Rows); got != `[[1 <nil> QUI= Infinity]]` {
		t.Fatalf("values row = %s", got)
	}
	if r.Truncated || r.RowCount != 1 || len(r.Columns) != 4 {
		t.Fatalf("result = %+v", r)
	}

	r = mustQuery(t, e, `SELECT e.id, s.first_prompt FROM events e
		JOIN sessions s ON s.id = e.session_id ORDER BY e.id`)
	if len(r.Rows) != 2 || r.Rows[0][0] != int64(1) {
		t.Fatalf("join rows = %v", r.Rows)
	}

	r = mustQuery(t, e, `SELECT e.id FROM events_fts JOIN events e ON e.id = events_fts.rowid
		WHERE events_fts MATCH 'authentication'`)
	if len(r.Rows) != 1 {
		t.Fatalf("FTS rows = %v", r.Rows)
	}

	r = mustQuery(t, e, `SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`)
	var tables []string
	for _, row := range r.Rows {
		tables = append(tables, row[0].(string))
	}
	for _, want := range []string{"event_annotations", "events", "events_fts", "schema_migrations", "session_token_usage", "sessions", "sources"} {
		found := false
		for _, name := range tables {
			if name == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("sqlite_master misses %s: %v", want, tables)
		}
	}

	for _, q := range []string{
		`PRAGMA table_info(events)`,
		`PRAGMA table_xinfo(events)`,
		`PRAGMA table_list`,
		`PRAGMA index_list(events)`,
		`PRAGMA index_info(events_session_time)`,
		`PRAGMA foreign_key_list(events)`,
		`PRAGMA database_list`,
		`PRAGMA function_list`,
		`PRAGMA pragma_list`,
		`EXPLAIN QUERY PLAN SELECT id FROM events WHERE id = 1`,
		`WITH RECURSIVE c(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM c WHERE n < 5) SELECT sum(n) FROM c`,
		`SELECT json_extract(value, '$.status') FROM event_annotations`,
		`SELECT 1;`,                    // one statement plus its terminator
		"SELECT 1 -- trailing comment", // comment after the terminator's place
		`SELECT 1; -- done`,            // terminator then comment only
		`/* leading */ SELECT 1 /* mid */ ; /* trailing */`,
		`SELECT ';' AS semi, 'a'';b' AS escaped`,
	} {
		mustQuery(t, e, q)
	}

	// Zero-row results still report their columns and encode rows as [].
	r = mustQuery(t, e, `SELECT id FROM events WHERE id > 100`)
	if len(r.Columns) != 1 || r.Rows == nil || len(r.Rows) != 0 || r.RowCount != 0 {
		t.Fatalf("empty result = %+v", r)
	}
}

// TestQueryDeniesAdversarialSQL pins the read-only boundary. Every input
// must fail, and after the whole battery the data must be unchanged.
func TestQueryDeniesAdversarialSQL(t *testing.T) {
	e := newTestEngine(t, EngineOptions{})

	denied := []string{
		// Data changes, in every shape an agent might try.
		`INSERT INTO events(id,session_id,source_id,source_offset,kind,raw_json) VALUES(9,'claude:dev1:sess-1','src-1',99,'x','{}')`,
		`INSERT OR REPLACE INTO sessions(id,agent,device_id,native_id) VALUES('s2','a','d','n')`,
		`UPDATE events SET text = 'changed'`,
		`DELETE FROM events`,
		`DELETE FROM events WHERE id = 1`,
		// Schema changes, including temporary and virtual objects.
		`CREATE TABLE t(x)`,
		`CREATE TEMP TABLE t(x)`,
		`CREATE VIEW v AS SELECT 1`,
		`CREATE INDEX i ON events(kind)`,
		// The trigger body's semicolons trip the one-statement check before
		// the authorizer sees the DDL; either rejection is correct, and the
		// only SQL statements with embedded semicolons are denied DDL.
		`CREATE TRIGGER tr AFTER INSERT ON events BEGIN SELECT 1; END`,
		`DROP TABLE events`,
		`DROP INDEX sessions_recent`,
		`DROP VIEW seeded_view`,
		`ALTER TABLE events ADD COLUMN extra TEXT`,
		`CREATE VIRTUAL TABLE v2 USING fts5(text)`,
		// Reaching outside this database or loading code.
		`ATTACH '/tmp/other.db' AS other`,
		`DETACH main`,
		`SELECT load_extension('evil')`,
		`SELECT fts3_tokenizer('simple')`,
		// Transactions and savepoints.
		`BEGIN`,
		`BEGIN DEFERRED`,
		`COMMIT`,
		`SAVEPOINT s`,
		`RELEASE s`,
		`ROLLBACK`,
		// Pragmas that change connection or database state.
		`PRAGMA journal_mode = DELETE`,
		`PRAGMA query_only = 1`,
		`PRAGMA busy_timeout = 0`,
		`PRAGMA foreign_keys = OFF`,
		`PRAGMA writable_schema = 1`,
		`PRAGMA synchronous = OFF`,
		`PRAGMA trusted_schema = OFF`,
		`PRAGMA user_version = 7`,
		// Maintenance that rewrites the file.
		`VACUUM`,
		`REINDEX`,
		`ANALYZE`,
		// FTS5 commands ride on INSERT, which the authorizer denies.
		`INSERT INTO events_fts(events_fts) VALUES('rebuild')`,
		`INSERT INTO events_fts(events_fts) VALUES('integrity-check')`,
		// A write hidden behind a function call or CTE still prepares a write.
		`SELECT load_extension('evil', 'entry')`,
	}
	for _, q := range denied {
		_, err := e.Query(context.Background(), q)
		if err == nil {
			t.Fatalf("adversarial query accepted: %q", q)
		}
		if strings.Contains(q, "CREATE TRIGGER") {
			if !errors.Is(err, ErrDenied) && !errors.Is(err, ErrMultipleStatements) {
				t.Fatalf("query %q: error = %v, want ErrDenied or ErrMultipleStatements", q, err)
			}
			continue
		}
		if !errors.Is(err, ErrDenied) {
			t.Fatalf("query %q: error = %v, want ErrDenied", q, err)
		}
	}

	// Nothing changed: the same rows, cursors and schema remain.
	r := mustQuery(t, e, `SELECT (SELECT count(*) FROM events),
		(SELECT count(*) FROM sessions), (SELECT count(*) FROM event_annotations)`)
	if fmt.Sprint(r.Rows) != "[[2 1 1]]" {
		t.Fatalf("data changed under denial: %v", r.Rows)
	}
	mustQuery(t, e, `SELECT sql FROM sqlite_master WHERE name = 'events'`)

	// A denial leaves the engine usable: the pool is not poisoned.
	mustQuery(t, e, `SELECT 1`)
}

func TestQueryOneStatementPerCall(t *testing.T) {
	e := newTestEngine(t, EngineOptions{})

	multiple := []string{
		`SELECT 1; SELECT 2`,
		`SELECT 1;
		 SELECT 2`,
		`SELECT 1;;`,
		`;SELECT 1`,
		`SELECT 1; DELETE FROM events`,
		`DELETE FROM events; SELECT 1`,
		`SELECT 1; ATTACH '/tmp/x' AS x`,
		`SELECT ';' AS a; SELECT 1`,       // the quoted ; is content, the last one terminates
		"SELECT 1 -- comment\n; SELECT 2", // terminator after comment, then a statement
		`SELECT 1 /* ; */; SELECT 2`,
	}
	for _, q := range multiple {
		if _, err := e.Query(context.Background(), q); !errors.Is(err, ErrMultipleStatements) {
			t.Fatalf("query %q: error = %v, want ErrMultipleStatements", q, err)
		}
	}
	for _, q := range []string{"", "   ", "\n\t", ";", "-- only a comment", "/* only a comment */"} {
		if _, err := e.Query(context.Background(), q); !errors.Is(err, ErrEmptyQuery) {
			t.Fatalf("query %q: error = %v, want ErrEmptyQuery", q, err)
		}
	}
	oversized := "SELECT '" + strings.Repeat("a", 64<<10) + "'"
	if _, err := e.Query(context.Background(), oversized); err == nil ||
		!strings.Contains(err.Error(), "input limit") {
		t.Fatalf("oversized query error = %v", err)
	}
	// The rejected write inside the multi-statement input never ran.
	r := mustQuery(t, e, `SELECT count(*) FROM events`)
	if fmt.Sprint(r.Rows) != "[[2]]" {
		t.Fatalf("events after rejected batch = %v", r.Rows)
	}
}

func TestQueryLimitsAndExplicitTruncation(t *testing.T) {
	countUpTo := func(n int) string {
		return fmt.Sprintf(`WITH RECURSIVE c(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM c WHERE n < %d) SELECT n FROM c`, n)
	}

	t.Run("row limit", func(t *testing.T) {
		e := newTestEngine(t, EngineOptions{MaxRows: 5})
		r := mustQuery(t, e, countUpTo(100))
		if len(r.Rows) != 5 || !r.Truncated || r.TruncationReason != truncatedByRows {
			t.Fatalf("result = %d rows, truncated=%v reason=%q", len(r.Rows), r.Truncated, r.TruncationReason)
		}
		if fmt.Sprint(r.Rows[4]) != "[5]" {
			t.Fatalf("fifth row = %v", r.Rows[4])
		}
	})

	t.Run("row limit exact fit is not truncation", func(t *testing.T) {
		e := newTestEngine(t, EngineOptions{MaxRows: 5})
		r := mustQuery(t, e, countUpTo(5))
		if len(r.Rows) != 5 || r.Truncated {
			t.Fatalf("result = %d rows, truncated=%v", len(r.Rows), r.Truncated)
		}
	})

	t.Run("byte limit drops the crossing row whole", func(t *testing.T) {
		e := newTestEngine(t, EngineOptions{MaxOutputBytes: 30})
		r := mustQuery(t, e, `SELECT 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' AS wide`)
		if len(r.Rows) != 0 || !r.Truncated || r.TruncationReason != truncatedByBytes {
			t.Fatalf("result = %d rows, truncated=%v reason=%q", len(r.Rows), r.Truncated, r.TruncationReason)
		}
		// A 14-byte row JSON (`["aaaaaaaaaa"]`) fits twice in 30 bytes; the
		// third row crosses the bound and is dropped whole.
		e2 := newTestEngine(t, EngineOptions{MaxOutputBytes: 30})
		r = mustQuery(t, e2, `WITH RECURSIVE c(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM c WHERE n < 10)
			SELECT 'aaaaaaaaaa' FROM c`)
		if len(r.Rows) != 2 || !r.Truncated || r.TruncationReason != truncatedByBytes {
			t.Fatalf("result = %d rows (%v), truncated=%v reason=%q", len(r.Rows), r.Rows, r.Truncated, r.TruncationReason)
		}
		if r.RowCount != 2 {
			t.Fatalf("row_count = %d, want the rows actually returned", r.RowCount)
		}
	})

	t.Run("byte limit exact fit is not truncation", func(t *testing.T) {
		e := newTestEngine(t, EngineOptions{MaxOutputBytes: 4}) // one row `[1]` = 3 bytes
		r := mustQuery(t, e, `SELECT 1`)
		if len(r.Rows) != 1 || r.Truncated {
			t.Fatalf("result = %d rows, truncated=%v", len(r.Rows), r.Truncated)
		}
	})
}

func TestQueryDeadlineInterruptsExecution(t *testing.T) {
	e := newTestEngine(t, EngineOptions{Deadline: 300 * time.Millisecond})
	before := time.Now()
	_, err := e.Query(context.Background(),
		`WITH RECURSIVE c(n) AS (VALUES(0) UNION ALL SELECT n+1 FROM c WHERE n < 1000000000) SELECT sum(n) FROM c`)
	if !errors.Is(err, ErrDeadline) {
		t.Fatalf("error = %v, want ErrDeadline", err)
	}
	if errors.Is(err, ErrDenied) {
		t.Fatalf("a slow query is not a policy violation: %v", err)
	}
	if elapsed := time.Since(before); elapsed > 5*time.Second {
		t.Fatalf("query ran %s past its deadline", elapsed)
	}
	// The interrupted connection returns to the pool and keeps working.
	mustQuery(t, e, `SELECT 1`)
}

func TestQueryBusyFailsFastAndRecovers(t *testing.T) {
	e := newTestEngine(t, EngineOptions{MaxConcurrency: 1, Deadline: 20 * time.Second})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	slow := `WITH RECURSIVE c(n) AS (VALUES(0) UNION ALL SELECT n+1 FROM c WHERE n < 1000000000) SELECT sum(n) FROM c`
	started := make(chan struct{})
	go func() {
		close(started)
		_, _ = e.Query(ctx, slow) // runs until canceled
	}()
	<-started
	// The only slot is taken once the slow query is executing; a second call
	// must fail fast rather than queue behind it.
	deadline := time.Now().Add(5 * time.Second)
	var busy error
	for time.Now().Before(deadline) {
		_, busy = e.Query(context.Background(), `SELECT 1`)
		if errors.Is(busy, ErrBusy) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !errors.Is(busy, ErrBusy) {
		t.Fatalf("second query error = %v, want ErrBusy", busy)
	}
	cancel()
	// The slot frees once the slow query unwinds.
	time.Sleep(100 * time.Millisecond)
	mustQuery(t, e, `SELECT 1`)
}

func TestQueryDeniedTables(t *testing.T) {
	e := newTestEngine(t, EngineOptions{DenyTables: []string{"events"}})
	if _, err := e.Query(context.Background(), `SELECT id FROM events`); !errors.Is(err, ErrDenied) {
		t.Fatalf("denied table error = %v", err)
	}
	if _, err := e.Query(context.Background(), `SELECT id FROM sessions`); err != nil {
		t.Fatalf("allowed table error = %v", err)
	}
}

// TestQueryIsolationAcrossConnections checks that every pooled connection
// carries the authorizer: concurrent calls on distinct connections deny the
// same writes and cannot disturb each other's reads. Concurrency is set high
// enough that the pool opens a connection per call.
func TestQueryIsolationAcrossConnections(t *testing.T) {
	e := newTestEngine(t, EngineOptions{MaxConcurrency: 16})

	var wg sync.WaitGroup
	errs := make([]error, 16)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				_, errs[i] = e.Query(context.Background(), `DELETE FROM events`)
			} else {
				_, errs[i] = e.Query(context.Background(), `SELECT count(*) FROM events`)
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if i%2 == 0 && !errors.Is(err, ErrDenied) {
			t.Fatalf("write %d error = %v, want ErrDenied", i, err)
		}
		if i%2 == 1 && err != nil {
			t.Fatalf("read %d error = %v", i, err)
		}
	}
	r := mustQuery(t, e, `SELECT count(*) FROM events`)
	if fmt.Sprint(r.Rows) != "[[2]]" {
		t.Fatalf("events after concurrent denial = %v", r.Rows)
	}
}

// TestQueryReaderPoolIsReadOnly proves the defense in depth: a raw
// connection from the engine's pool, with no authorizer installed by the
// engine, still cannot write because the pool is opened mode=ro.
func TestQueryReaderPoolIsReadOnly(t *testing.T) {
	e := newTestEngine(t, EngineOptions{})
	ctx := context.Background()
	conn, err := e.readers.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `DELETE FROM events`); err == nil {
		t.Fatal("write on a raw reader connection succeeded; mode=ro is not active")
	}
}

func TestOptionsDefaults(t *testing.T) {
	got := EngineOptions{WriterID: "w"}.withDefaults()
	if got.WriterID != "w" || got.MaxRows != DefaultMaxRows ||
		got.MaxOutputBytes != DefaultMaxOutputBytes || got.MaxQueryBytes != DefaultMaxQueryBytes ||
		got.Deadline != DefaultDeadline || got.MaxConcurrency != DefaultMaxConcurrency ||
		got.MaxAnnotateRequestBytes != DefaultMaxAnnotateRequestBytes ||
		len(got.DenyTables) != 0 {
		t.Fatalf("defaults = %+v", got)
	}
}
