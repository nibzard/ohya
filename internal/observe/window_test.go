package observe

import (
	"testing"
	"time"
)

func snapshotAt(instance Instance, uptimeMS int64, wallMS int64, counters map[string]uint64) *Snapshot {
	return &Snapshot{
		FormatVersion: SnapshotFormatVersion,
		Instance:      instance,
		UptimeMS:      uptimeMS,
		SampledWallMS: wallMS,
		Counters:      counters,
		Gauges:        map[string]float64{},
		Durations:     map[string]HistogramData{},
		Resources:     map[string]Reading{},
	}
}

func TestDeltaIsExactWithinOneInstance(t *testing.T) {
	instance := testInstance(t)
	before := snapshotAt(instance, 10_000, 1794837600000, map[string]uint64{
		"parse_records{agent=claude-code}": 100,
		"ingest_events{result=inserted}":   40,
	})
	after := snapshotAt(instance, 70_000, 1794837660000, map[string]uint64{
		"parse_records{agent=claude-code}": 160,
		"ingest_events{result=inserted}":   61,
	})
	window := Delta(after, before)
	if !window.SameInstance {
		t.Fatal("same instance must produce a usable window")
	}
	if window.ElapsedMS == nil || *window.ElapsedMS != 60_000 {
		t.Fatalf("elapsed = %v, want 60000 from monotonic uptime", window.ElapsedMS)
	}
	if window.Counters["parse_records{agent=claude-code}"] != 60 {
		t.Errorf("parse delta = %d, want 60", window.Counters["parse_records{agent=claude-code}"])
	}
	if window.Counters["ingest_events{result=inserted}"] != 21 {
		t.Errorf("event delta = %d, want 21", window.Counters["ingest_events{result=inserted}"])
	}
	if len(window.UnavailableCounters) != 0 {
		t.Errorf("no unavailable series expected: %v", window.UnavailableCounters)
	}
}

func TestDeltaAcrossInstancesIsUnavailable(t *testing.T) {
	first := testInstance(t)
	second, err := NewInstance("/home/example/.ohya", "d63898e", false, NewFakeClock(1794837600000), RoleIngest)
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	before := snapshotAt(first, 10_000, 1794837600000, map[string]uint64{"parse_bytes": 5})
	after := snapshotAt(second, 90_000, 1794837690000, map[string]uint64{"parse_bytes": 500})
	window := Delta(after, before)
	if window.SameInstance {
		t.Fatal("different instances must not join counters")
	}
	if window.Counters != nil || window.ElapsedMS != nil {
		t.Errorf("restart deltas must be unavailable, not zero: %+v", window)
	}
}

func TestDeltaNeverReportsNegativeCounters(t *testing.T) {
	instance := testInstance(t)
	before := snapshotAt(instance, 10_000, 1794837600000, map[string]uint64{
		"parse_records{agent=codex}":     10,
		"ingest_events{result=inserted}": 4,
	})
	after := snapshotAt(instance, 20_000, 1794837610000, map[string]uint64{
		"parse_records{agent=codex}":     4,
		"ingest_events{result=inserted}": 9,
	})
	window := Delta(after, before)
	if window.Counters["parse_records{agent=codex}"] != 0 {
		t.Errorf("reset counter must not yield a delta: %+v", window.Counters)
	}
	if !contains(window.UnavailableCounters, "parse_records{agent=codex}") {
		t.Errorf("reset counter must be reported unavailable: %v", window.UnavailableCounters)
	}
	if window.Counters["ingest_events{result=inserted}"] != 5 {
		t.Errorf("healthy counter delta wrong: %+v", window.Counters)
	}
	for _, delta := range window.Counters {
		if delta < 0 {
			t.Fatalf("negative delta %d produced: %+v", delta, window.Counters)
		}
	}
}

func TestDeltaMissingSampleIsUnavailable(t *testing.T) {
	instance := testInstance(t)
	before := snapshotAt(instance, 10_000, 1794837600000, map[string]uint64{"parse_bytes": 5})
	after := snapshotAt(instance, 20_000, 1794837610000, map[string]uint64{
		"parse_bytes":    9,
		"upload_batches": 1,
	})
	window := Delta(after, before)
	if _, joined := window.Counters["upload_batches"]; joined {
		t.Error("series missing from the earlier sample must not be joined")
	}
	if !contains(window.UnavailableCounters, "upload_batches") {
		t.Errorf("missing series must be reported unavailable: %v", window.UnavailableCounters)
	}
	if window.Counters["parse_bytes"] != 4 {
		t.Errorf("observed series delta = %d, want 4", window.Counters["parse_bytes"])
	}
}

func TestDeltaMonotonicRegressionYieldsNoElapsed(t *testing.T) {
	instance := testInstance(t)
	before := snapshotAt(instance, 50_000, 1794837600000, map[string]uint64{"parse_bytes": 5})
	after := snapshotAt(instance, 40_000, 1794837590000, map[string]uint64{"parse_bytes": 9})
	window := Delta(after, before)
	if window.ElapsedMS != nil {
		t.Errorf("monotonic regression must yield unavailable elapsed, got %d", *window.ElapsedMS)
	}
	if window.Counters["parse_bytes"] != 4 {
		t.Errorf("counters remain exact without elapsed: %+v", window.Counters)
	}
}

func TestRecorderSurvivesBackwardWallClockJump(t *testing.T) {
	clock := NewFakeClock(1794837600000)
	recorder, err := NewRecorder(testInstance(t), clock)
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	recorder.Add("parse_records", 3, "normalized")
	before := recorder.Snapshot()
	clock.SetWallMS(before.SampledWallMS - int64(time.Hour.Milliseconds()))
	clock.Advance(30 * time.Second)
	recorder.Add("parse_records", 2, "normalized")
	after := recorder.Snapshot()
	if after.UptimeMS <= before.UptimeMS {
		t.Fatalf("uptime must be monotonic: %d then %d", before.UptimeMS, after.UptimeMS)
	}
	if after.SampledWallMS >= before.SampledWallMS {
		t.Fatalf("test setup must move the wall clock backwards: %d then %d", before.SampledWallMS, after.SampledWallMS)
	}
	window := Delta(&after, &before)
	if window.ElapsedMS == nil || *window.ElapsedMS != 30_000 {
		t.Fatalf("elapsed = %v, want 30000 from monotonic time despite the wall jump", window.ElapsedMS)
	}
	if window.Counters["parse_records{parse_result=normalized}"] != 2 {
		t.Errorf("counter delta = %d, want 2", window.Counters["parse_records{parse_result=normalized}"])
	}
	if err := after.Validate(); err != nil {
		t.Fatalf("snapshot after a wall jump stays valid: %v", err)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
