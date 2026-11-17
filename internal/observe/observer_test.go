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
