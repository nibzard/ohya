package hubsync

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/nibzard/ohya/internal/store"
)

func TestBatchWaitsForAnotherWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hub.db")
	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := startEndpoint(t, db, nil, Options{Retry: RetryPolicy{Base: time.Millisecond}})

	other, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	conn, err := other.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	hold := time.Second
	lockedAt := time.Now()
	release := time.AfterFunc(hold, func() {
		_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
	})
	defer release.Stop()

	recs := fixtureRecords(t)
	d := dial(t, s.addr)
	mustRegisterVia(t, d, deviceA, "src-1", "/p/s.jsonl")
	d.send(fixtureBatch(deviceA, "src-1", recs))
	d.mustAck(Ack{Cursor: 6528, Inserted: 16})
	if held := time.Since(lockedAt); held < hold {
		t.Fatalf("the batch was acknowledged %v after another writer took the write lock; want a commit after the lock was released", held)
	}
}
