package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nibzard/ohya/internal/observe"
)

type metricsEnvelope struct {
	SchemaVersion int         `json:"schema_version"`
	Command       string      `json:"command"`
	Data          metricsData `json:"data"`
	Error         *jsonError  `json:"error"`
}

func runMetricsForTest(t *testing.T, args ...string) (metricsEnvelope, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := runMetricsCommand(args, &stdout, &stderr)
	var envelope metricsEnvelope
	if jsonErr := json.Unmarshal(stdout.Bytes(), &envelope); jsonErr != nil && stdout.Len() > 0 {
		t.Fatalf("metrics output must decode as one JSON envelope: %v: %s", jsonErr, stdout.String())
	}
	return envelope, stdout.String(), err
}

func historySample(t time.Time, uptimeMS int64, mutate func(*observe.Snapshot)) observe.Snapshot {
	snapshot := observe.Snapshot{
		FormatVersion: observe.SnapshotFormatVersion,
		Instance: observe.Instance{
			InstanceID:  strings.Repeat("1", 32),
			PID:         os.Getpid(),
			Roles:       []string{"ingest"},
			StartWallMS: t.Add(-time.Duration(uptimeMS) * time.Millisecond).UnixMilli(),
		},
		UptimeMS:      uptimeMS,
		SampledWallMS: t.UnixMilli(),
		Counters:      map[string]uint64{},
		Gauges:        map[string]float64{},
		Durations:     map[string]observe.HistogramData{},
	}
	if mutate != nil {
		mutate(&snapshot)
	}
	return snapshot
}

func writeHistory(t *testing.T, stateDir, id string, samples []observe.Snapshot) {
	t.Helper()
	instanceDir := filepath.Join(stateDir, "observe", id)
	if err := os.MkdirAll(instanceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(filepath.Join(instanceDir, "history.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	enc := json.NewEncoder(file)
	for _, sample := range samples {
		if err := enc.Encode(sample); err != nil {
			t.Fatal(err)
		}
	}
}

func historyHistogram(count uint64, bucket int, sumMS float64) observe.HistogramData {
	data := observe.HistogramData{
		BoundsMS: append([]float64(nil), observe.DurationBoundsMS...),
		Buckets:  make([]uint64, len(observe.DurationBoundsMS)+1),
		Count:    count,
		SumMS:    sumMS,
	}
	data.Buckets[bucket] = count
	return data
}

func TestMetricsWindowDeltasRatesAndPartialCoverage(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "hub.db")
	now := time.Now()
	writeHistory(t, dir, strings.Repeat("1", 32), []observe.Snapshot{
		historySample(now.Add(-3*time.Minute), 60_000, func(s *observe.Snapshot) {
			s.Counters["ingest_events{event_result=inserted}"] = 10
			s.Counters["parse_bytes"] = 1000
			s.Durations["parse_duration{agent=codex}"] = historyHistogram(5, 3, 10)
		}),
		historySample(now.Add(-2*time.Minute), 120_000, func(s *observe.Snapshot) {
			s.Counters["ingest_events{event_result=inserted}"] = 16
			s.Counters["parse_bytes"] = 1600
			s.Durations["parse_duration{agent=codex}"] = historyHistogram(9, 3, 20)
		}),
		historySample(now.Add(-1*time.Minute), 180_000, func(s *observe.Snapshot) {
			s.Counters["ingest_events{event_result=inserted}"] = 22
			s.Counters["parse_bytes"] = 2200
			s.Durations["parse_duration{agent=codex}"] = historyHistogram(13, 3, 30)
		}),
	})
	envelope, raw, err := runMetricsForTest(t, "--state", dir, "--db", dbPath, "--window", "5m", "--json")
	if err != errDegraded {
		t.Fatalf("three of five minutes observed must degrade, not fail: %v (%s)", err, raw)
	}
	if envelope.Error != nil {
		t.Fatalf("a partial window stays parseable with no command error: %+v", envelope.Error)
	}
	instance := envelope.Data.Instances[0]
	if instance.SampleCount != 3 {
		t.Errorf("sample count = %d, want 3", instance.SampleCount)
	}
	if instance.Counters["ingest_events{event_result=inserted}"] != 12 {
		t.Errorf("counter delta = %d, want 12", instance.Counters["ingest_events{event_result=inserted}"])
	}
	if instance.MonotonicElapsedMS == nil || *instance.MonotonicElapsedMS != 120_000 {
		t.Errorf("monotonic elapsed = %+v, want 120000ms", instance.MonotonicElapsedMS)
	}
	if rate := instance.RatesPerSecond["parse_bytes"]; rate != 10 {
		t.Errorf("parse_bytes rate = %f, want 10/s over 120s", rate)
	}
	if instance.Durations["parse_duration{agent=codex}"].Count != 8 {
		t.Errorf("duration delta count = %d, want 8", instance.Durations["parse_duration{agent=codex}"].Count)
	}
	if instance.Durations["parse_duration{agent=codex}"].P50 != nil {
		t.Error("quantiles below twenty observations must be null")
	}
	if *instance.Gaps != 0 {
		t.Errorf("gaps = %d, want 0", *instance.Gaps)
	}
	if !hasFindingCode(envelope.Data.Findings, "window_partial", sevWarn) {
		t.Errorf("the short window must be reported as partial: %+v", envelope.Data.Findings)
	}
}

func TestMetricsQuantilesAppearAboveMinimumSamples(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "hub.db")
	now := time.Now()
	writeHistory(t, dir, strings.Repeat("2", 32), []observe.Snapshot{
		historySample(now.Add(-2*time.Minute), 60_000, func(s *observe.Snapshot) {
			s.Durations["parse_duration{agent=pi}"] = historyHistogram(2, 3, 4)
		}),
		historySample(now.Add(-1*time.Minute), 180_000, func(s *observe.Snapshot) {
			s.Durations["parse_duration{agent=pi}"] = historyHistogram(30, 3, 60)
		}),
	})
	envelope, raw, err := runMetricsForTest(t, "--state", dir, "--db", dbPath, "--window", "5m", "--json")
	if err != nil && err != errDegraded {
		t.Fatalf("metrics must not hard-fail: %v (%s)", err, raw)
	}
	delta := envelope.Data.Instances[0].Durations["parse_duration{agent=pi}"]
	if delta.Count != 28 {
		t.Fatalf("delta count = %d, want 28", delta.Count)
	}
	if delta.P50 == nil || delta.P95 == nil {
		t.Fatalf("quantiles above the minimum must be present: %+v", delta)
	}
	if *delta.P50 < 1 || *delta.P50 > 2 {
		t.Errorf("p50 = %f, want inside bucket 3 (1..2ms)", *delta.P50)
	}
}

func TestMetricsGapsAndCorruptSamples(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "hub.db")
	now := time.Now()
	writeHistory(t, dir, strings.Repeat("3", 32), []observe.Snapshot{
		historySample(now.Add(-4*time.Minute), 60_000, nil),
		historySample(now.Add(-1*time.Minute), 240_000, nil),
	})
	instanceDir := filepath.Join(dir, "observe", strings.Repeat("3", 32))
	file, err := os.OpenFile(filepath.Join(instanceDir, "history.jsonl"), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("{corrupt\n"); err != nil {
		t.Fatal(err)
	}
	file.Close()
	envelope, raw, err := runMetricsForTest(t, "--state", dir, "--db", dbPath, "--window", "5m", "--json")
	if err != errDegraded {
		t.Fatalf("a gapped window must degrade: %v (%s)", err, raw)
	}
	instance := envelope.Data.Instances[0]
	if *instance.Gaps != 2 {
		t.Errorf("gaps = %d, want 2", *instance.Gaps)
	}
	if instance.CorruptSamples != 1 {
		t.Errorf("corrupt samples = %d, want 1", instance.CorruptSamples)
	}
}

func TestMetricsNoSamplesAndDisabledObservation(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "hub.db")
	instanceDir := filepath.Join(dir, "observe", strings.Repeat("4", 32))
	if err := os.MkdirAll(instanceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	envelope, raw, err := runMetricsForTest(t, "--state", dir, "--db", dbPath, "--window", "5m", "--json")
	if err != errDegraded {
		t.Fatalf("an empty observe tree must degrade: %v (%s)", err, raw)
	}
	if !hasFindingCode(envelope.Data.Findings, "no_history_samples", sevWarn) {
		t.Errorf("missing history must be its own finding: %+v", envelope.Data.Findings)
	}
}

func TestMetricsInstancesStaySeparate(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "hub.db")
	now := time.Now()
	first := historySample(now.Add(-2*time.Minute), 60_000, nil)
	first.Instance.InstanceID = strings.Repeat("5", 32)
	first.Counters["parse_records{parse_result=normalized}"] = 10
	second := historySample(now.Add(-1*time.Minute), 120_000, nil)
	second.Instance.InstanceID = strings.Repeat("5", 32)
	second.Counters["parse_records{parse_result=normalized}"] = 20
	writeHistory(t, dir, strings.Repeat("5", 32), []observe.Snapshot{first, second})
	third := historySample(now.Add(-2*time.Minute), 60_000, nil)
	third.Instance.InstanceID = strings.Repeat("6", 32)

	third.Counters["parse_records{parse_result=normalized}"] = 0
	fourth := historySample(now.Add(-1*time.Minute), 120_000, nil)
	fourth.Instance.InstanceID = strings.Repeat("6", 32)

	fourth.Counters["parse_records{parse_result=normalized}"] = 5
	writeHistory(t, dir, strings.Repeat("6", 32), []observe.Snapshot{third, fourth})
	envelope, raw, err := runMetricsForTest(t, "--state", dir, "--db", dbPath, "--window", "5m", "--json")
	if err != nil && err != errDegraded {
		t.Fatalf("metrics must not hard-fail: %v (%s)", err, raw)
	}
	if len(envelope.Data.Instances) != 2 {
		t.Fatalf("instances = %d, want 2", len(envelope.Data.Instances))
	}
	deltas := map[string]uint64{}
	for _, instance := range envelope.Data.Instances {
		deltas[instance.InstanceID] = instance.Counters["parse_records{parse_result=normalized}"]
	}
	if deltas[strings.Repeat("5", 32)] != 10 || deltas[strings.Repeat("6", 32)] != 5 {
		t.Errorf("per-instance deltas must never merge: %v", deltas)
	}
	if strings.Contains(raw, "\"quantiles\"") {
		t.Error("no cross-instance percentile field may exist")
	}
}

func TestMetricsWindowAndRoleValidation(t *testing.T) {
	var stdout bytes.Buffer
	if err := runMetricsCommand([]string{"--window", "7m", "--state", t.TempDir()}, &stdout, &stdout); err == nil {
		t.Error("an unknown window must be a usage error")
	}
	if err := runMetricsCommand([]string{"--role", "cron", "--state", t.TempDir()}, &stdout, &stdout); err == nil {
		t.Error("an unknown role must be a usage error")
	}
	if err := runMetricsCommand([]string{"--window", "1m", "--instance", "short", "--state", t.TempDir()}, &stdout, &stdout); err == nil {
		t.Error("an invalid instance id must be a usage error")
	}
}

func TestMetricsTextOutputShowsNullQuantiles(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "hub.db")
	now := time.Now()
	writeHistory(t, dir, strings.Repeat("7", 32), []observe.Snapshot{
		historySample(now.Add(-2*time.Minute), 60_000, func(s *observe.Snapshot) {
			s.Durations["parse_duration{agent=pi}"] = historyHistogram(2, 3, 4)
		}),
		historySample(now.Add(-1*time.Minute), 120_000, func(s *observe.Snapshot) {
			s.Durations["parse_duration{agent=pi}"] = historyHistogram(4, 3, 8)
		}),
	})
	var stdout bytes.Buffer
	err := runMetricsCommand([]string{"--state", dir, "--db", dbPath, "--window", "5m"}, &stdout, &stdout)
	if err != errDegraded {
		t.Fatalf("a partial window must exit three: %v", err)
	}
	text := stdout.String()
	if !strings.Contains(text, "parse_duration{agent=pi}") || !strings.Contains(text, "  2  ") || !strings.Contains(text, "  -  ") {
		t.Errorf("text output must show null quantiles as dashes: %s", text)
	}
	if !strings.Contains(text, "window_partial") {
		t.Errorf("text output must print the partial finding: %s", text)
	}
}

func TestMetricsFullWindowExitsZero(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "hub.db")
	now := time.Now()
	var samples []observe.Snapshot
	step := 0
	for offset := -6 * time.Minute; offset <= 20*time.Second; offset += 10 * time.Second {
		sample := historySample(now.Add(offset), int64(step)*10_000, nil)
		sample.Counters["ingest_passes"] = uint64(step + 1)
		samples = append(samples, sample)
		step++
	}
	writeHistory(t, dir, strings.Repeat("8", 32), samples)
	envelope, raw, err := runMetricsForTest(t, "--state", dir, "--db", dbPath, "--window", "5m", "--json")
	if err != nil {
		t.Fatalf("a complete window must exit zero: %v (%s)", err, raw)
	}
	if !hasFindingCode(envelope.Data.Findings, "window_complete", sevOK) {
		t.Errorf("a complete window carries the window_complete finding: %+v", envelope.Data.Findings)
	}
	instance := envelope.Data.Instances[0]
	if *instance.Gaps != 0 {
		t.Errorf("gaps = %d, want 0 at ten-second spacing", *instance.Gaps)
	}
}

func TestMetricsOmitsDeadRetainedInstancesWithNoSamplesUnlessAll(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "hub.db")
	now := time.Now()
	var samples []observe.Snapshot
	step := 0
	for offset := -6 * time.Minute; offset <= 20*time.Second; offset += 10 * time.Second {
		sample := historySample(now.Add(offset), int64(step)*10_000, nil)
		sample.Counters["ingest_passes"] = uint64(step + 1)
		samples = append(samples, sample)
		step++
	}
	writeHistory(t, dir, strings.Repeat("9", 32), samples)
	writeStatusFixture(t, dir, strings.Repeat("9", 32), os.Getpid(), time.Second, nil)
	writeStatusFixture(t, dir, strings.Repeat("e", 32), os.Getpid(), 2*time.Hour, func(s *observe.Snapshot) {
		s.Instance.Roles = []string{"mcp"}
		s.Stopped = true
	})

	envelope, raw, err := runMetricsForTest(t, "--state", dir, "--db", dbPath, "--window", "5m", "--json")
	if err != nil {
		t.Fatalf("healthy metrics with old stopped mcp must exit zero: %v (%s)", err, raw)
	}
	if len(envelope.Data.Instances) != 1 {
		t.Fatalf("instances = %d, want 1", len(envelope.Data.Instances))
	}
	if hasFindingCode(envelope.Data.Findings, "no_history_samples", sevWarn) {
		t.Fatalf("omitted dead instance must not emit no_history_samples: %+v", envelope.Data.Findings)
	}

	allEnv, _, err := runMetricsForTest(t, "--state", dir, "--db", dbPath, "--window", "5m", "--all", "--json")
	if err != errDegraded {
		t.Fatalf("--all with empty dead instance must degrade: %v", err)
	}
	if len(allEnv.Data.Instances) != 2 {
		t.Fatalf("--all instances = %d, want 2", len(allEnv.Data.Instances))
	}
}

func TestMetricsTextOutputShowsCounterDeltasAndRates(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "hub.db")
	now := time.Now()
	writeHistory(t, dir, strings.Repeat("8", 32), []observe.Snapshot{
		historySample(now.Add(-2*time.Minute), 60_000, func(s *observe.Snapshot) {
			s.Counters["parse_bytes"] = 1000
		}),
		historySample(now.Add(-1*time.Minute), 120_000, func(s *observe.Snapshot) {
			s.Counters["parse_bytes"] = 1600
		}),
	})
	var stdout bytes.Buffer
	if err := runMetricsCommand([]string{"--state", dir, "--db", dbPath, "--window", "5m"}, &stdout, &stdout); err != errDegraded {
		t.Fatalf("a partial window must exit three: %v", err)
	}
	text := stdout.String()
	for _, want := range []string{"COUNTER", "parse_bytes", "600", "10.00"} {
		if !strings.Contains(text, want) {
			t.Errorf("text output lacks %q:\n%s", want, text)
		}
	}
}

func TestDurationAndRateTextStayShort(t *testing.T) {
	cases := map[string]string{
		durationMSText(0.25):     "0.2ms",
		durationMSText(999.94):   "999.9ms",
		durationMSText(122126.3): "122.1s",
		rateText(1.934):          "1.93",
		rateText(2878848178.37):  "2878848178",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q; want %q", got, want)
		}
	}
}
