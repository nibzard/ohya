package observe

import (
	"testing"
	"time"
)

func TestRecorderHotPathAllocations(t *testing.T) {
	clock := NewSystemClock()
	instance, err := NewInstance("alloc-test", "", false, clock, RoleIngest)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := NewRecorder(instance, clock)
	if err != nil {
		t.Fatal(err)
	}
	rec.Observe("ingest_pass_duration", time.Millisecond)
	allocs := testing.AllocsPerRun(500, func() {
		rec.Add("ingest_passes", 1)
		rec.Observe("ingest_pass_duration", time.Millisecond)
		rec.Add("parse_records", 1, "normalized")
		rec.Set("watch_sources", 3, "active")
	})
	if allocs > 6 {
		t.Fatalf("recorder hot path allocates %.0f times per pass-shaped call set, want at most 6 (docs/performance.md overhead baseline)", allocs)
	}
	if got := rec.DroppedMetrics(); got != 0 {
		t.Fatalf("hot-path calls dropped %d samples, want 0", got)
	}
}
