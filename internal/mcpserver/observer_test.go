package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/nibzard/ohya/internal/observe/observertest"
)

func waitForObserver(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func assertNoUnknownAdds(t *testing.T, obs *observertest.Recorder) {
	t.Helper()
	if got := obs.UnknownAddCalls(); got != 0 {
		t.Errorf("%d Add calls used unknown metric names or label arity", got)
	}
}

func TestObserverReportsSuccessfulQuery(t *testing.T) {
	obs := observertest.New()
	e := newTestEngine(t, EngineOptions{Observer: obs})

	r := mustQuery(t, e, `SELECT id, text FROM events ORDER BY id`)
	if r.RowCount != 2 {
		t.Fatalf("rows = %d, want 2", r.RowCount)
	}
	if got := obs.Counter("sql_requests", "sql", "ok"); got != 1 {
		t.Errorf("ok requests = %d, want 1", got)
	}
	if got := obs.Counter("sql_rows"); got != 2 {
		t.Errorf("rows counted = %d, want 2", got)
	}
	if got := obs.Counter("sql_result_bytes"); got != 55 {
		t.Errorf("result bytes = %d, want the 55 encoded row bytes", got)
	}
	for _, name := range []string{
		"sql_total_duration", "sql_reader_wait_duration", "sql_first_row_duration", "sql_iteration_duration",
	} {
		if got := obs.DurationCount(name); got != 1 {
			t.Errorf("%s samples = %d, want 1", name, got)
		}
	}
	if got := obs.Gauge("sql_active"); got != 0 {
		t.Errorf("active gauge = %v, want 0 after completion", got)
	}
	assertNoUnknownAdds(t, obs)
}

func TestObserverReportsBusyRejectionAndActiveGauge(t *testing.T) {
	obs := observertest.New()
	e := newTestEngine(t, EngineOptions{Observer: obs, MaxConcurrency: 1, Deadline: 20 * time.Second})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	slowDone := startSlowQuery(t, e, ctx)
	if _, err := e.Query(context.Background(), `SELECT 1`); !errors.Is(err, ErrBusy) {
		t.Fatalf("second query error = %v, want ErrBusy", err)
	}
	if got := obs.Counter("sql_busy_rejections"); got < 1 {
		t.Errorf("busy rejections = %d, want at least 1", got)
	}
	if got := obs.Counter("sql_requests", "sql", "busy"); got < 1 {
		t.Errorf("busy outcomes = %d, want at least 1", got)
	}
	waitForObserver(t, "the active gauge to hold the admitted query", func() bool {
		return obs.Gauge("sql_active") == 1
	})
	cancel()
	if err := <-slowDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("slow query error = %v, want context.Canceled", err)
	}
	waitForObserver(t, "the active gauge to release the cancelled query", func() bool {
		return obs.Gauge("sql_active") == 0
	})
	if got := obs.Counter("sql_requests", "sql", "cancelled"); got != 1 {
		t.Errorf("cancelled outcomes = %d, want 1", got)
	}
	assertNoUnknownAdds(t, obs)
}

func TestObserverReportsQueryFailuresWithoutRowCounts(t *testing.T) {
	obs := observertest.New()
	e := newTestEngine(t, EngineOptions{Observer: obs, Deadline: 300 * time.Millisecond})

	_, err := e.Query(context.Background(),
		`WITH RECURSIVE c(n) AS (VALUES(0) UNION ALL SELECT n+1 FROM c WHERE n < 1000000000) SELECT sum(n) FROM c`)
	if !errors.Is(err, ErrDeadline) {
		t.Fatalf("slow query error = %v, want ErrDeadline", err)
	}
	if got := obs.Counter("sql_requests", "sql", "timeout"); got != 1 {
		t.Errorf("timeout outcomes = %d, want 1", got)
	}

	if _, err := e.Query(context.Background(), `DELETE FROM events`); !errors.Is(err, ErrDenied) {
		t.Fatalf("write query error = %v, want ErrDenied", err)
	}
	if got := obs.Counter("sql_requests", "sql", "rejected"); got != 1 {
		t.Errorf("rejected outcomes = %d, want 1", got)
	}
	if got := obs.Counter("sql_rows"); got != 0 {
		t.Errorf("rows counted = %d, want 0: failed requests return no rows", got)
	}
	if got := obs.Counter("sql_result_bytes"); got != 0 {
		t.Errorf("result bytes = %d, want 0: failed requests return no bytes", got)
	}
	if got := obs.DurationCount("sql_iteration_duration"); got != 0 {
		t.Errorf("iteration samples = %d, want 0: no query reached iteration", got)
	}
	if got := obs.DurationCount("sql_first_row_duration"); got != 2 {
		t.Errorf("first-row samples = %d, want 2: both failures surfaced at execution", got)
	}
	if got := obs.Gauge("sql_active"); got != 0 {
		t.Errorf("active gauge = %v, want 0 after completion", got)
	}
	assertNoUnknownAdds(t, obs)
}

func TestObserverReportsTruncations(t *testing.T) {
	countUpTo := func(n int) string {
		return fmt.Sprintf(`WITH RECURSIVE c(n) AS (VALUES(1) UNION ALL SELECT n+1 FROM c WHERE n < %d) SELECT n FROM c`, n)
	}

	t.Run("row limit", func(t *testing.T) {
		obs := observertest.New()
		e := newTestEngine(t, EngineOptions{Observer: obs, MaxRows: 5})
		mustQuery(t, e, countUpTo(100))
		if got := obs.Counter("sql_requests", "sql", "truncated_rows"); got != 1 {
			t.Errorf("truncated_rows outcomes = %d, want 1", got)
		}
		if got := obs.Counter("sql_truncations", "row_limit"); got != 1 {
			t.Errorf("row-limit truncations = %d, want 1", got)
		}
		if got := obs.Counter("sql_truncations", "byte_limit"); got != 0 {
			t.Errorf("byte-limit truncations = %d, want 0", got)
		}
		assertNoUnknownAdds(t, obs)
	})

	t.Run("byte limit", func(t *testing.T) {
		obs := observertest.New()
		e := newTestEngine(t, EngineOptions{Observer: obs, MaxOutputBytes: 30})
		mustQuery(t, e, `SELECT 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' AS wide`)
		if got := obs.Counter("sql_requests", "sql", "truncated_bytes"); got != 1 {
			t.Errorf("truncated_bytes outcomes = %d, want 1", got)
		}
		if got := obs.Counter("sql_truncations", "byte_limit"); got != 1 {
			t.Errorf("byte-limit truncations = %d, want 1", got)
		}
		assertNoUnknownAdds(t, obs)
	})
}

func TestObserverReportsAnnotateOutcomes(t *testing.T) {
	obs := observertest.New()
	e := newTestEngine(t, EngineOptions{Observer: obs})
	ctx := context.Background()

	batch := `{"operations":[
		{"op":"upsert","event_id":2,"namespace":"tags","key":"flag","value":{"seen":true}},
		{"op":"delete","event_id":2,"namespace":"tags","key":"absent"}]}`
	for i := 0; i < 2; i++ {
		if _, err := e.Annotate(ctx, []byte(batch)); err != nil {
			t.Fatalf("annotate %d: %v", i, err)
		}
	}
	if got := obs.Counter("sql_requests", "annotate", "ok"); got != 2 {
		t.Errorf("ok annotate requests = %d, want 2", got)
	}
	if got := obs.Counter("annotate_operations", "upsert", "applied"); got != 1 {
		t.Errorf("upserts applied = %d, want 1", got)
	}
	if got := obs.Counter("annotate_operations", "upsert", "unchanged"); got != 1 {
		t.Errorf("upserts unchanged = %d, want 1", got)
	}
	if got := obs.Counter("annotate_operations", "delete", "applied"); got != 0 {
		t.Errorf("deletes applied = %d, want 0", got)
	}
	if got := obs.Counter("annotate_operations", "delete", "unchanged"); got != 2 {
		t.Errorf("deletes unchanged = %d, want 2", got)
	}
	if got := obs.DurationCount("annotate_duration"); got != 2 {
		t.Errorf("annotate transaction durations = %d, want 2", got)
	}
	assertNoUnknownAdds(t, obs)
}

func TestObserverReportsAnnotateValidationFailures(t *testing.T) {
	obs := observertest.New()
	e := newTestEngine(t, EngineOptions{Observer: obs})
	ctx := context.Background()

	if _, err := e.Annotate(ctx, []byte(
		`{"operations":[{"op":"merge","event_id":2,"namespace":"n","key":"k"}]}`)); !errors.Is(err, ErrAnnotateRejected) {
		t.Fatalf("unknown op error = %v, want ErrAnnotateRejected", err)
	}
	if _, err := e.Annotate(ctx, []byte(
		`{"operations":[{"op":"upsert","event_id":999,"namespace":"n","key":"k","value":1}]}`)); err == nil {
		t.Fatal("missing event annotate succeeded; want a store rejection")
	}
	if got := obs.Counter("annotate_validation_failures"); got != 2 {
		t.Errorf("validation failures = %d, want 2 (engine and store rejections)", got)
	}
	if got := obs.Counter("sql_requests", "annotate", "rejected"); got != 2 {
		t.Errorf("rejected annotate requests = %d, want 2", got)
	}
	if got := obs.Counter("annotate_operations", "upsert", "applied"); got != 0 {
		t.Errorf("upserts applied = %d, want 0: rejected batches apply nothing", got)
	}
	assertNoUnknownAdds(t, obs)
}

func TestObserverDisabledEngineSkipsTracing(t *testing.T) {
	obs := observertest.New()
	e := newTestEngine(t, EngineOptions{})
	if e.traced {
		t.Fatal("engine without usage or observer must take the untraced fast path")
	}
	mustQuery(t, e, `SELECT 1`)
	if got := obs.Counter("sql_requests", "sql", "ok"); got != 0 {
		t.Errorf("ok requests = %d, want 0: this engine holds no observer", got)
	}
}
