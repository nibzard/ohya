package observe

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"
)

func testInstance(t *testing.T) Instance {
	t.Helper()
	instance, err := NewInstance("/home/example/.ohya", "d63898e", false, NewFakeClock(1794837600000), RoleIngest)
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	return instance
}

func testRecorder(t *testing.T) *Recorder {
	t.Helper()
	recorder, err := NewRecorder(testInstance(t), NewFakeClock(1794837600000))
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	return recorder
}

func TestNewInstanceIDIsUniqueHex(t *testing.T) {
	first, err := NewInstanceID()
	if err != nil {
		t.Fatalf("NewInstanceID: %v", err)
	}
	second, err := NewInstanceID()
	if err != nil {
		t.Fatalf("NewInstanceID: %v", err)
	}
	if first == second {
		t.Fatal("instance ids must be unique per process start")
	}
	if err := (Instance{InstanceID: first, PID: 1, Roles: []string{"ingest"}, StartWallMS: 1}).Validate(); err != nil {
		t.Fatalf("generated instance id rejected: %v", err)
	}
}

func TestNewInstanceRejectsUnknownRole(t *testing.T) {
	clock := NewFakeClock(1794837600000)
	if _, err := NewInstance("", "", false, clock, Role("scheduler")); err == nil {
		t.Fatal("unknown role must be rejected")
	}
	if _, err := NewInstance("", "", false, clock); err == nil {
		t.Fatal("missing roles must be rejected")
	}
	if _, err := NewInstance("", "", false, nil, RoleIngest); err == nil {
		t.Fatal("missing clock must be rejected")
	}
}

func TestRecorderCounterOutcomesExact(t *testing.T) {
	recorder := testRecorder(t)
	recorder.Add("ingest_events", 3, "inserted")
	recorder.Add("ingest_events", 4, "inserted")
	recorder.Add("ingest_events", 2, "duplicate")
	recorder.Add("discovery_candidates", 9)
	snapshot := recorder.Snapshot()
	want := map[string]uint64{
		"ingest_events{event_result=inserted}":  7,
		"ingest_events{event_result=duplicate}": 2,
		"discovery_candidates":                  9,
	}
	for key, value := range want {
		if snapshot.Counters[key] != value {
			t.Errorf("counter %s = %d, want %d", key, snapshot.Counters[key], value)
		}
	}
	if len(snapshot.Counters) != len(want) {
		t.Errorf("counter series count = %d, want %d: %v", len(snapshot.Counters), len(want), snapshot.Counters)
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("snapshot validate: %v", err)
	}
}

func TestRecorderGaugeAndDurationSeries(t *testing.T) {
	recorder := testRecorder(t)
	recorder.Set("watch_sources", 3, "active")
	recorder.Set("watch_sources", 1, "missing")
	recorder.Observe("parse_duration", 3*time.Millisecond, "codex")
	recorder.Observe("parse_duration", 900*time.Microsecond, "codex")
	snapshot := recorder.Snapshot()
	if snapshot.Gauges["watch_sources{state=active}"] != 3 {
		t.Errorf("gauge active = %v", snapshot.Gauges["watch_sources{state=active}"])
	}
	data := snapshot.Durations["parse_duration{agent=codex}"]
	if data.Count != 2 {
		t.Fatalf("duration count = %d, want 2", data.Count)
	}
	if data.SumMS < 3.899 || data.SumMS > 3.901 {
		t.Errorf("duration sum = %f, want about 3.9", data.SumMS)
	}
}

func TestRecorderDropsInvalidCalls(t *testing.T) {
	recorder := testRecorder(t)
	recorder.Add("not_a_metric", 1)
	recorder.Set("ingest_events", 5, "inserted")
	recorder.Add("ingest_events", 5, "inserted", "extra")
	recorder.Observe("parse_duration", -time.Second, "codex")
	if got := recorder.DroppedMetrics(); got != 4 {
		t.Fatalf("dropped = %d, want 4", got)
	}
	snapshot := recorder.Snapshot()
	if len(snapshot.Counters) != 0 || len(snapshot.Gauges) != 0 || len(snapshot.Durations) != 0 {
		t.Errorf("invalid calls must not create series: %v", snapshot)
	}
}

func TestRecorderMapsUnknownLabelValueToFallback(t *testing.T) {
	recorder := testRecorder(t)
	recorder.Observe("parse_duration", time.Millisecond, "some-brand-new-agent")
	recorder.Observe("parse_duration", time.Millisecond, "codex")
	recorder.Add("ingest_events", 1, "evicted")
	snapshot := recorder.Snapshot()
	if snapshot.Durations["parse_duration{agent=unknown}"].Count != 1 {
		t.Errorf("unknown agent must fold into the fallback label value: %v", snapshot.Durations)
	}
	if snapshot.Durations["parse_duration{agent=codex}"].Count != 1 {
		t.Errorf("known agent must keep its own series: %v", snapshot.Durations)
	}
	if snapshot.Counters["ingest_events{event_result=other}"] != 1 {
		t.Errorf("unknown result must fold into the fallback label value: %v", snapshot.Counters)
	}
	if recorder.DroppedMetrics() != 0 {
		t.Errorf("fallback mapping must not count as dropped: %d", recorder.DroppedMetrics())
	}
}

func TestRecorderSeriesBoundExact(t *testing.T) {
	recorder := testRecorder(t)
	for _, metric := range Catalog {
		sets := make([][]string, 0, len(metric.Labels))
		for _, labelName := range metric.Labels {
			sets = append(sets, labelSets[labelName].Values)
		}
		var combinations [][]string
		if len(sets) == 0 {
			combinations = [][]string{nil}
		} else {
			combinations = product(sets)
		}
		for _, combo := range combinations {
			switch metric.Kind {
			case Counter:
				recorder.Add(metric.Name, 1, combo...)
			case Gauge:
				recorder.Set(metric.Name, 1, combo...)
			case Duration:
				recorder.Observe(metric.Name, time.Millisecond, combo...)
			}
		}
	}
	snapshot := recorder.Snapshot()
	series := len(snapshot.Counters) + len(snapshot.Gauges) + len(snapshot.Durations)
	if series != SeriesBound() {
		t.Fatalf("series = %d, want catalogue bound %d", series, SeriesBound())
	}
}

func TestRecorderJunkLabelsStayBounded(t *testing.T) {
	recorder := testRecorder(t)
	random := rand.New(rand.NewSource(7))
	for i := 0; i < 5000; i++ {
		for _, metric := range Catalog {
			values := make([]string, len(metric.Labels))
			for j := range values {
				values[j] = fmt.Sprintf("junk-%d", random.Intn(64))
			}
			switch metric.Kind {
			case Counter:
				recorder.Add(metric.Name, 1, values...)
			case Gauge:
				recorder.Set(metric.Name, 1, values...)
			case Duration:
				recorder.Observe(metric.Name, time.Millisecond, values...)
			}
		}
	}
	snapshot := recorder.Snapshot()
	series := len(snapshot.Counters) + len(snapshot.Gauges) + len(snapshot.Durations)
	if series > SeriesBound() {
		t.Fatalf("series = %d exceeds catalogue bound %d", series, SeriesBound())
	}
}

func product(sets [][]string) [][]string {
	out := [][]string{nil}
	for _, set := range sets {
		next := make([][]string, 0, len(out)*len(set))
		for _, prefix := range out {
			for _, value := range set {
				combination := make([]string, 0, len(prefix)+1)
				combination = append(combination, prefix...)
				combination = append(combination, value)
				next = append(next, combination)
			}
		}
		out = next
	}
	return out
}

func TestRecorderConcurrentUseIsExact(t *testing.T) {
	recorder := testRecorder(t)
	const goroutines = 8
	const iterations = 500
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				recorder.Add("parse_records", 1, "normalized")
				recorder.Observe("parse_duration", 2*time.Millisecond, "codex")
				recorder.Set("watch_sources", 3, "active")
				recorder.Resource(ResourceGoroutines, Supported(float64(goroutines)))
				if i%100 == 0 {
					_ = recorder.Snapshot()
				}
			}
		}()
	}
	wg.Wait()
	snapshot := recorder.Snapshot()
	if got := snapshot.Counters["parse_records{parse_result=normalized}"]; got != goroutines*iterations {
		t.Errorf("counter = %d, want %d", got, goroutines*iterations)
	}
	if got := snapshot.Durations["parse_duration{agent=codex}"].Count; got != goroutines*iterations {
		t.Errorf("duration count = %d, want %d", got, goroutines*iterations)
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("snapshot validate: %v", err)
	}
}

func TestReadingUnavailableDiffersFromZero(t *testing.T) {
	zero, err := json.Marshal(Supported(0))
	if err != nil {
		t.Fatalf("marshal supported zero: %v", err)
	}
	unavailable, err := json.Marshal(Unsupported("rss unavailable on this platform"))
	if err != nil {
		t.Fatalf("marshal unsupported: %v", err)
	}
	if string(zero) == string(unavailable) {
		t.Fatalf("supported zero %s must differ from unsupported %s", zero, unavailable)
	}
	var unsupportedReading Reading
	if err := json.Unmarshal(unavailable, &unsupportedReading); err != nil {
		t.Fatalf("unmarshal unsupported: %v", err)
	}
	if unsupportedReading.Value != nil {
		t.Errorf("unsupported reading value = %v, want nil", *unsupportedReading.Value)
	}
	var zeroReading Reading
	if err := json.Unmarshal(zero, &zeroReading); err != nil {
		t.Fatalf("unmarshal zero: %v", err)
	}
	if zeroReading.Value == nil || *zeroReading.Value != 0 {
		t.Error("supported zero must decode as a present zero value")
	}
	if err := Unsupported("").Validate(); err == nil {
		t.Error("unsupported reading without a reason must fail validation")
	}
	if err := (Reading{Value: ptr(1), Reason: "why"}).Validate(); err == nil {
		t.Error("supported reading with a reason must fail validation")
	}
}

func ptr(v float64) *float64 { return &v }

func TestSnapshotMarshalIsDeterministic(t *testing.T) {
	recorder := testRecorder(t)
	recorder.Add("ingest_events", 1, "inserted")
	recorder.Add("ingest_events", 1, "duplicate")
	recorder.Add("parse_records", 1, "normalized")
	recorder.Observe("parse_duration", time.Millisecond, "pi")
	recorder.Set("sqlite_file_bytes", 1024, "wal")
	recorder.Resource(ResourceRSSBytes, Unsupported("not sampled"))
	first, err := json.Marshal(recorder.Snapshot())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	second, err := json.Marshal(recorder.Snapshot())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("snapshot bytes not deterministic:\n%s\n%s", first, second)
	}
}

func TestSnapshotValidateRejectsDegradedShapes(t *testing.T) {
	base := testRecorder(t).Snapshot()
	base.FormatVersion = 99
	if err := base.Validate(); err == nil {
		t.Error("unknown format version must fail")
	}
	bad := testRecorder(t).Snapshot()
	bad.SampledWallMS = 0
	if err := bad.Validate(); err == nil {
		t.Error("missing sample timestamp must fail")
	}
	badResource := testRecorder(t).Snapshot()
	badResource.Resources = map[string]Reading{"rss_bytes": Unsupported("")}
	if err := badResource.Validate(); err == nil {
		t.Error("reasonless unsupported resource must fail")
	}
}

func TestResolveDropsMetricWithUndeclaredLabel(t *testing.T) {
	catalogIndex["test_undeclared_label"] = Metric{Name: "test_undeclared_label", Kind: Counter, Labels: []string{"nowhere"}}
	defer delete(catalogIndex, "test_undeclared_label")
	recorder := testRecorder(t)
	recorder.Add("test_undeclared_label", 1, "any")
	if recorder.DroppedMetrics() != 1 {
		t.Fatal("an undeclared catalogue label must drop the call, not panic")
	}
	if series := len(recorder.Snapshot().Counters); series != 0 {
		t.Errorf("dropped call created %d series", series)
	}
}

func TestRecorderRejectsInvalidInstance(t *testing.T) {
	bad := Instance{InstanceID: "nothex", PID: 1, Roles: []string{"ingest"}, StartWallMS: 1}
	if _, err := NewRecorder(bad, NewFakeClock(1)); err == nil {
		t.Fatal("invalid instance must be rejected")
	}
	if _, err := NewRecorder(testInstance(t), nil); err == nil {
		t.Fatal("missing clock must be rejected")
	}
}
