package observe

import (
	"encoding/json"
	"testing"
	"time"
)

func TestDisabledObserverDiscardsCalls(t *testing.T) {
	Disabled.Add("parse_bytes", 10)
	Disabled.Set("watch_pending_complete_bytes", 5)
	Disabled.Observe("parse_duration", time.Millisecond, "claude-code")
	snapshot := testRecorder(t).Snapshot()
	if len(snapshot.Counters) != 0 || len(snapshot.Gauges) != 0 || len(snapshot.Durations) != 0 {
		t.Fatalf("disabled observer must not record: %v", snapshot)
	}
}

func TestRecorderSatisfiesObserver(t *testing.T) {
	recorder, err := NewRecorder(testInstance(t), NewSystemClock())
	if err != nil {
		t.Fatal(err)
	}
	var observer Observer = recorder
	observer.Add("parse_bytes", 3)
	observer.Observe("parse_duration", time.Millisecond, "codex")
	snapshot := observerRecordingSnapshot(t, observer)
	if snapshot.Counters["parse_bytes"] != 3 {
		t.Errorf("parse_bytes = %d, want 3", snapshot.Counters["parse_bytes"])
	}
	if snapshot.Durations["parse_duration{agent=codex}"].Count != 1 {
		t.Errorf("parse_duration missing: %v", snapshot.Durations)
	}
}

func TestCatalogCoversPassAndCheckpointIO(t *testing.T) {
	for _, name := range []string{
		"watch_read_bytes", "watch_checkpoint_saves", "watch_checkpoint_bytes_written",
		"ingest_passes", "ingest_pass_duration", "ingest_failures",
	} {
		if _, ok := MetricByName(name); !ok {
			t.Errorf("metric %s missing from the catalogue", name)
		}
	}
	set, ok := LabelSet("failure_kind")
	if !ok {
		t.Fatal("failure_kind label set missing")
	}
	if set.Fallback != "other" {
		t.Errorf("failure_kind fallback = %q, want other", set.Fallback)
	}
}

func observerRecordingSnapshot(t *testing.T, observer Observer) Snapshot {
	t.Helper()
	type snapshotter interface{ Snapshot() Snapshot }
	recorder, ok := observer.(snapshotter)
	if !ok {
		t.Fatal("observer cannot snapshot")
	}
	snapshot := recorder.Snapshot()
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var round Snapshot
	if err := json.Unmarshal(data, &round); err != nil {
		t.Fatal(err)
	}
	return round
}

func TestGaugeGroupSumsPerSlotWatchGauges(t *testing.T) {
	clock := NewFakeClock(1_000_000)
	clock.Advance(250 * time.Millisecond)
	inst, err := NewInstance("group-test", "", false, clock, RoleJoin)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := NewRecorder(inst, clock)
	if err != nil {
		t.Fatal(err)
	}
	group := NewGaugeGroup(rec)
	claude := group.Slot("claude-code")
	antigravity := group.Slot("antigravity")

	claude.Set("watch_sources", 100, "active")
	claude.Set("watch_sources", 34, "missing")
	claude.Set("watch_sources", 1, "stopped")
	claude.Set("watch_pending_complete_bytes", 512)
	claude.Set("watch_partial_tail_bytes", 64)
	claude.Set("upload_backlog_bytes", 2048, "claude-code")

	antigravity.Set("watch_sources", 6, "active")
	antigravity.Set("watch_sources", 0, "missing")
	antigravity.Set("watch_sources", 0, "stopped")
	antigravity.Set("watch_pending_complete_bytes", 0)
	antigravity.Set("watch_partial_tail_bytes", 16)
	RecordDiscoveryScan(antigravity, 6, 2*time.Millisecond)

	snap := rec.Snapshot()
	if got := snap.Gauges["watch_sources{state=active}"]; got != 106 {
		t.Errorf("active = %v, want 106", got)
	}
	if got := snap.Gauges["watch_sources{state=missing}"]; got != 34 {
		t.Errorf("missing = %v, want 34", got)
	}
	if got := snap.Gauges["watch_sources{state=stopped}"]; got != 1 {
		t.Errorf("stopped = %v, want 1 (must not be overwritten by later agent)", got)
	}
	if got := snap.Gauges["watch_pending_complete_bytes"]; got != 512 {
		t.Errorf("pending_complete_bytes = %v, want 512", got)
	}
	if got := snap.Gauges["watch_partial_tail_bytes"]; got != 80 {
		t.Errorf("partial_tail_bytes = %v, want 80", got)
	}
	if got := snap.Gauges["upload_backlog_bytes{agent=claude-code}"]; got != 2048 {
		t.Errorf("upload_backlog_bytes = %v, want 2048", got)
	}
	if got := snap.Gauges["discovery_last_scan_ms"]; got != 250 {
		t.Errorf("discovery_last_scan_ms = %v, want 250", got)
	}
	if got := snap.Counters["discovery_scans"]; got != 1 {
		t.Errorf("discovery_scans = %d, want 1", got)
	}
	if got := snap.Counters["discovery_candidates"]; got != 6 {
		t.Errorf("discovery_candidates = %d, want 6", got)
	}
}
