package hubsync

import (
	"context"
	"testing"

	"github.com/nibzard/ohya/internal/mcpserver"
	"github.com/nibzard/ohya/internal/observe/observertest"
)

func TestRemoteCallsReportEngineMetricsOnTheHub(t *testing.T) {
	obs := observertest.New()
	s, _, _ := remoteFixtureHub(t, mcpserver.EngineOptions{Observer: obs})
	auth := s.ep.auth.(*peerAuth)
	b := dialDevice(t, s, auth, deviceB)
	ctx := context.Background()

	if _, err := b.Query(ctx, `SELECT count(*) FROM events`); err != nil {
		t.Fatal(err)
	}
	if got := obs.Counter("sql_requests", "sql", "ok"); got != 1 {
		t.Errorf("ok requests = %d, want 1", got)
	}
	if got := obs.Counter("sql_rows"); got != 1 {
		t.Errorf("rows counted = %d, want 1", got)
	}
	if got := obs.DurationCount("sql_first_row_duration"); got != 1 {
		t.Errorf("first-row samples = %d, want 1", got)
	}

	if _, err := b.Annotate(ctx, []byte(
		`{"operations":[{"op":"upsert","event_id":1,"namespace":"usage","key":"probe","value":1}]}`)); err != nil {
		t.Fatal(err)
	}
	if got := obs.Counter("sql_requests", "annotate", "ok"); got != 1 {
		t.Errorf("ok annotate requests = %d, want 1", got)
	}
	if got := obs.Counter("annotate_operations", "upsert", "applied"); got != 1 {
		t.Errorf("upserts applied = %d, want 1", got)
	}
	if got := obs.DurationCount("annotate_duration"); got != 1 {
		t.Errorf("annotate transaction durations = %d, want 1", got)
	}
	if got := obs.Gauge("sql_active"); got != 0 {
		t.Errorf("active gauge = %v, want 0 after completion", got)
	}
	if got := obs.Counter("hub_failures", "other"); got != 0 {
		t.Errorf("hub failures = %d, want 0: successful exchanges are not failures", got)
	}
	if got := obs.UnknownAddCalls(); got != 0 {
		t.Errorf("%d Add calls used unknown metric names or label arity", got)
	}
}
