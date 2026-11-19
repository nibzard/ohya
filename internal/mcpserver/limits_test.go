package mcpserver

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestQueryEnforcesSQLiteRuntimeLimits(t *testing.T) {
	e := newTestEngine(t, EngineOptions{})
	ctx := context.Background()

	if _, err := e.Query(ctx, `SELECT zeroblob(1000000000)`); err == nil {
		t.Fatal("oversized zeroblob succeeded; want SQLITE_LIMIT_LENGTH error")
	}

	doubling := `WITH RECURSIVE c(n, s) AS (VALUES(0, 'aaaaaaaa') UNION ALL SELECT n+1, s || s FROM c WHERE n < 25) SELECT s FROM c WHERE n = 25`
	if _, err := e.Query(ctx, doubling); err == nil {
		t.Fatal("recursive string doubling bomb succeeded; want SQLITE_LIMIT_LENGTH error")
	}

	longLike := `SELECT 'a' LIKE '` + strings.Repeat("%", 300) + `'`
	if _, err := e.Query(ctx, longLike); err == nil {
		t.Fatal("oversized LIKE pattern succeeded; want SQLITE_LIMIT_LIKE_PATTERN_LENGTH error")
	}

	res, err := e.Query(ctx, `SELECT zeroblob(500000)`)
	if err != nil {
		t.Fatalf("bounded blob query failed: %v", err)
	}
	if !res.Truncated || res.TruncationReason != TruncationReasonBytes || res.RowCount != 0 {
		t.Fatalf("oversized single blob row = %+v, want byte_limit truncation with 0 rows", res)
	}
}

func TestQueryDeniesPurgedSourcesByDefault(t *testing.T) {
	e := newTestEngine(t, EngineOptions{})
	ctx := context.Background()

	for _, q := range []string{
		`SELECT * FROM purged_sources`,
		`SELECT count(*) FROM purged_sources`,
		`PRAGMA table_info(purged_sources)`,
		"SELECT 1\x00; DROP TABLE events",
	} {
		_, err := e.Query(ctx, q)
		if !errors.Is(err, ErrDenied) {
			t.Errorf("%q error = %v, want ErrDenied", q, err)
		}
	}
}

func TestAnnotateStrictTrailingTokensAndConcurrencySlot(t *testing.T) {
	e := newTestEngine(t, EngineOptions{WriterID: "dev1", MaxConcurrency: 1})
	ctx := context.Background()

	if _, err := e.Annotate(ctx, []byte(`{"operations":[]}}`)); !errors.Is(err, ErrAnnotateRejected) {
		t.Fatalf("trailing brace error = %v, want ErrAnnotateRejected", err)
	}

	e.slots <- struct{}{}
	defer func() { <-e.slots }()
	if _, err := e.Annotate(ctx, []byte(`{"operations":[{"op":"delete","event_id":1,"namespace":"ns","key":"k"}]}`)); !errors.Is(err, ErrBusy) {
		t.Fatalf("saturated annotate error = %v, want ErrBusy", err)
	}
}
