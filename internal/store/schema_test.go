package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"
)

func setup(t *testing.T) *sql.DB {
	t.Helper()
	hub, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hub.Close() })
	db := hub.Writer()
	for _, q := range []string{
		`INSERT INTO sessions(id,agent,device_id,native_id) VALUES('s','agent','d','n')`,
		`INSERT INTO sources(id,device_id,agent,path) VALUES('f','d','agent','log')`,
		`INSERT INTO events(id,session_id,source_id,source_offset,kind,raw_json,text) VALUES(1,'s','f',0,'message','{}','hello')`,
	} {
		if _, err = db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestAnnotationsAndFTS(t *testing.T) {
	db := setup(t)
	add := `INSERT INTO event_annotations VALUES(?, 'security', 'review', ?, ?, 1, 1)`
	for _, writer := range []string{"d", "other"} {
		if _, err := db.Exec(add, 1, "true", writer); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE event_annotations SET value='false',updated_at_ms=2 WHERE writer_id='d'`); err != nil {
		t.Fatal(err)
	}
	var value, raw string
	var count int
	if err := db.QueryRow(`SELECT value FROM event_annotations WHERE writer_id='other'`).Scan(&value); err != nil || value != "true" {
		t.Fatalf("other writer: %q, %v", value, err)
	}
	if err := db.QueryRow(`SELECT raw_json FROM events`).Scan(&raw); err != nil || raw != "{}" {
		t.Fatalf("raw: %q, %v", raw, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM events_fts WHERE events_fts MATCH 'hello'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("FTS: %d, %v", count, err)
	}
	if _, err := db.Exec(add, 1, "true", "d"); err == nil {
		t.Fatal("duplicate accepted")
	}
	for _, tc := range []struct {
		event int
		value string
	}{{99, "true"}, {1, "invalid"}} {
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(add, 1, "true", "first"); err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec(add, tc.event, tc.value, "bad"); err == nil {
			t.Fatal("invalid annotation accepted")
		}
		if err = tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		if err = db.QueryRow(`SELECT count(*) FROM event_annotations WHERE writer_id='first'`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("rollback: %d, %v", count, err)
		}
	}
}

func TestDriverAuthorizerAndCancellation(t *testing.T) {
	db := setup(t)
	conn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err = conn.Raw(func(c any) error {
		c.(*sqlite3.SQLiteConn).RegisterAuthorizer(func(op int, _, _, _ string) int {
			if op == sqlite3.SQLITE_DELETE {
				return sqlite3.SQLITE_DENY
			}
			return sqlite3.SQLITE_OK
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.ExecContext(context.Background(), `DELETE FROM events`); err == nil {
		t.Fatal("authorizer did not deny DELETE")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	var n int
	err = conn.QueryRowContext(ctx, `WITH RECURSIVE x(n) AS (VALUES(0) UNION ALL SELECT n+1 FROM x WHERE n<1000000000) SELECT sum(n) FROM x`).Scan(&n)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline, got %v", err)
	}
}
