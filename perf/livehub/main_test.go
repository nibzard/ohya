package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/nibzard/ohya/internal/observe"
)

func histObservations(values []float64) observe.HistogramData {
	h := observe.HistogramData{
		BoundsMS: append([]float64(nil), observe.DurationBoundsMS...),
		Buckets:  make([]uint64, len(observe.DurationBoundsMS)+1),
	}
	for _, v := range values {
		h.Buckets[sort.SearchFloat64s(observe.DurationBoundsMS, v)]++
		h.Count++
		h.SumMS += v
	}
	return h
}

func repeatValues(count int, values ...float64) []float64 {
	var out []float64
	for i := 0; i < count; i++ {
		out = append(out, values...)
	}
	return out
}

func testSnapshot(instanceID string, roles []string, wallMS int64, durations map[string]observe.HistogramData, gauges map[string]float64, rss float64) observe.Snapshot {
	s := observe.Snapshot{
		FormatVersion: observe.SnapshotFormatVersion,
		Instance: observe.Instance{
			InstanceID: instanceID, PID: 4242, Roles: roles,
			StartWallMS: wallMS - 3_600_000, BuildRevision: "testrev",
		},
		UptimeMS:      3_600_000,
		SampledWallMS: wallMS,
		Durations:     durations,
		Gauges:        gauges,
	}
	if rss > 0 {
		v := rss
		s.Resources = map[string]observe.Reading{"rss_bytes": {Value: &v}}
	}
	return s
}

func writeTestSnapshot(t *testing.T, dir, instanceID string, roles []string, wallMS int64) {
	t.Helper()
	raw, err := json.Marshal(testSnapshot(instanceID, roles, wallMS, nil, nil, 0))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "observe", instanceID, "snapshot.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func appendTestHistory(t *testing.T, dir, instanceID string, snapshots ...observe.Snapshot) {
	t.Helper()
	path := filepath.Join(dir, "observe", instanceID, "history.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, s := range snapshots {
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(append(raw, '\n')); err != nil {
			t.Fatal(err)
		}
	}
}

func writeTestUsage(t *testing.T, dir, instanceID, name string, records []observe.UsageRecord) {
	t.Helper()
	path := filepath.Join(dir, "observe", instanceID, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, r := range records {
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(append(raw, '\n')); err != nil {
			t.Fatal(err)
		}
	}
}

func sqlUsage(instanceID string, timeMS int64, totalMS float64, outcome string) observe.UsageRecord {
	return observe.UsageRecord{
		SchemaVersion: observe.UsageFormatVersion,
		TimeBucket:    observe.HourBucket(timeMS),
		TimeMS:        timeMS,
		InstanceID:    instanceID,
		Tool:          observe.UsageToolSQL,
		Client:        observe.UsageClientClaudeCode,
		Path:          observe.UsagePathLocal,
		Outcome:       outcome,
		PhaseMS:       observe.UsagePhases{TotalMS: totalMS},
	}
}

func reading(v float64) observe.Reading {
	value := v
	return observe.Reading{Value: &value}
}

func expectFinding(t *testing.T, d dayReport, substr string) {
	t.Helper()
	for _, f := range d.Findings {
		if strings.Contains(f, substr) {
			return
		}
	}
	t.Fatalf("findings %v lack %q", d.Findings, substr)
}

func TestCaptureAppendsNewRecordsOnce(t *testing.T) {
	state := t.TempDir()
	day := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	ingestID := strings.Repeat("a", 30) + "01"
	writeTestSnapshot(t, state, ingestID, []string{"ingest"}, day.UnixMilli())
	appendTestHistory(t, state, ingestID,
		testSnapshot(ingestID, []string{"ingest"}, day.UnixMilli(),
			map[string]observe.HistogramData{"ingest_pass_duration": histObservations([]float64{5, 7})},
			map[string]float64{"sqlite_file_bytes{file=wal}": 3 << 20}, 0),
		testSnapshot(ingestID, []string{"ingest"}, day.Add(time.Minute).UnixMilli(),
			map[string]observe.HistogramData{"ingest_pass_duration": histObservations([]float64{5, 7, 9})},
			map[string]float64{"sqlite_file_bytes{file=wal}": 4 << 20}, 0))
	mcpID := strings.Repeat("b", 30) + "02"
	writeTestSnapshot(t, state, mcpID, []string{"mcp"}, day.Add(2*time.Minute).UnixMilli())
	writeTestUsage(t, state, mcpID, "usage.jsonl", []observe.UsageRecord{
		sqlUsage(mcpID, day.Add(30*time.Second).UnixMilli(), 3.5, observe.UsageOutcomeOK),
	})

	out := t.TempDir()
	if err := runCapture(state, out); err != nil {
		t.Fatal(err)
	}
	samples, usage, err := readCaptureLog(filepath.Join(out, capturesFileName))
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 2 || len(usage) != 1 {
		t.Fatalf("captured %d samples and %d usage records, want 2 and 1", len(samples), len(usage))
	}
	if usage[0].TotalMS != 3.5 || usage[0].Role != "mcp" {
		t.Fatalf("usage record not mapped: %+v", usage[0])
	}
	if samples[0].BuildRevision != "testrev" || samples[0].Role != "ingest" {
		t.Fatalf("sample labels not carried: %+v", samples[0])
	}
	if _, ok := samples[0].Gauges["sqlite_file_bytes{file=wal}"]; !ok {
		t.Fatalf("tracked gauge missing: %+v", samples[0].Gauges)
	}

	appendTestHistory(t, state, ingestID,
		testSnapshot(ingestID, []string{"ingest"}, day.Add(2*time.Minute).UnixMilli(),
			map[string]observe.HistogramData{"ingest_pass_duration": histObservations([]float64{5, 7, 9, 11})},
			map[string]float64{"sqlite_file_bytes{file=wal}": 4 << 20}, 0))
	writeTestUsage(t, state, mcpID, "usage.jsonl", []observe.UsageRecord{
		sqlUsage(mcpID, day.Add(90*time.Second).UnixMilli(), 8.0, observe.UsageOutcomeOK),
	})
	if err := runCapture(state, out); err != nil {
		t.Fatal(err)
	}
	samples, usage, err = readCaptureLog(filepath.Join(out, capturesFileName))
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 3 {
		t.Fatalf("second capture holds %d samples, want 3", len(samples))
	}
	if len(usage) != 2 {
		t.Fatalf("second capture holds %d usage records, want 2", len(usage))
	}
}

func TestCaptureReadsRotatedUsageFiles(t *testing.T) {
	state := t.TempDir()
	day := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	id := strings.Repeat("c", 32)
	writeTestSnapshot(t, state, id, []string{"mcp"}, day.UnixMilli())
	writeTestUsage(t, state, id, "usage.jsonl.1", []observe.UsageRecord{
		sqlUsage(id, day.Add(time.Minute).UnixMilli(), 1.0, observe.UsageOutcomeOK),
	})
	writeTestUsage(t, state, id, "usage.jsonl", []observe.UsageRecord{
		sqlUsage(id, day.Add(2*time.Minute).UnixMilli(), 2.0, observe.UsageOutcomeOK),
	})
	out := t.TempDir()
	if err := runCapture(state, out); err != nil {
		t.Fatal(err)
	}
	_, usage, err := readCaptureLog(filepath.Join(out, capturesFileName))
	if err != nil {
		t.Fatal(err)
	}
	if len(usage) != 2 {
		t.Fatalf("captured %d usage records from rotated files, want 2", len(usage))
	}
}

func TestReportAggregatesDailyHistogramDeltas(t *testing.T) {
	day := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	ingestID := strings.Repeat("d", 32)
	base := histObservations(repeatValues(10, 6.0))
	grown := histObservations(append(repeatValues(10, 6.0), repeatValues(5, 30.0, 90.0)...))
	final := histObservations(append(append(repeatValues(10, 6.0), repeatValues(5, 30.0, 90.0)...), repeatValues(10, 120.0)...))
	samples := []sampleRecord{
		{Kind: kindSample, InstanceID: ingestID, Role: "ingest", WallMS: day.UnixMilli(),
			Durations: map[string]observe.HistogramData{"ingest_pass_duration": base}},
		{Kind: kindSample, InstanceID: ingestID, Role: "ingest", WallMS: day.Add(time.Minute).UnixMilli(),
			Durations: map[string]observe.HistogramData{"ingest_pass_duration": grown}},
		{Kind: kindSample, InstanceID: ingestID, Role: "ingest", WallMS: day.Add(2 * time.Hour).UnixMilli(),
			Durations: map[string]observe.HistogramData{"ingest_pass_duration": final}},
	}
	combined := histObservations(append(repeatValues(5, 30.0, 90.0), repeatValues(10, 120.0)...))
	days := aggregateDays(samples, nil, 0)
	if len(days) != 1 {
		t.Fatalf("aggregated %d days, want 1", len(days))
	}
	stat := days[0].Metrics["ingest_pass_duration"]
	if stat.N != combined.Count {
		t.Fatalf("delta count %d, want %d", stat.N, combined.Count)
	}
	if stat.SumMS != combined.SumMS {
		t.Fatalf("delta sum %v, want %v", stat.SumMS, combined.SumMS)
	}
	wantP50, _ := combined.Quantile(0.5)
	wantP95, _ := combined.Quantile(0.95)
	if stat.P50MS == nil || stat.P95MS == nil {
		t.Fatalf("quantiles missing for n=%d", stat.N)
	}
	if *stat.P50MS != wantP50 || *stat.P95MS != wantP95 {
		t.Fatalf("p50/p95 = %v/%v, want %v/%v", *stat.P50MS, *stat.P95MS, wantP50, wantP95)
	}
	if days[0].Instances["ingest"] != 1 {
		t.Fatalf("instance count %d, want 1", days[0].Instances["ingest"])
	}
}

func TestReportSkipsNonMonotoneDeltasAndInstanceResets(t *testing.T) {
	day := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	idA := strings.Repeat("e", 31) + "1"
	idB := strings.Repeat("e", 31) + "2"
	samples := []sampleRecord{
		{Kind: kindSample, InstanceID: idA, Role: "ingest", WallMS: day.UnixMilli(),
			Durations: map[string]observe.HistogramData{"sqlite_transaction_duration": histObservations([]float64{6.0, 7.0})}},
		{Kind: kindSample, InstanceID: idA, Role: "ingest", WallMS: day.Add(time.Minute).UnixMilli(),
			Durations: map[string]observe.HistogramData{"sqlite_transaction_duration": histObservations([]float64{6.0, 7.0, 40.0})}},
		{Kind: kindSample, InstanceID: idA, Role: "ingest", WallMS: day.Add(2 * time.Minute).UnixMilli(),
			Durations: map[string]observe.HistogramData{"sqlite_transaction_duration": histObservations([]float64{6.0})}},
		{Kind: kindSample, InstanceID: idB, Role: "ingest", WallMS: day.Add(3 * time.Minute).UnixMilli(),
			Durations: map[string]observe.HistogramData{"sqlite_transaction_duration": histObservations([]float64{8.0})}},
		{Kind: kindSample, InstanceID: idB, Role: "ingest", WallMS: day.Add(4 * time.Minute).UnixMilli(),
			Durations: map[string]observe.HistogramData{"sqlite_transaction_duration": histObservations([]float64{8.0, 60.0})}},
	}
	days := aggregateDays(samples, nil, 0)
	if len(days) != 1 {
		t.Fatalf("aggregated %d days, want 1", len(days))
	}
	stat := days[0].Metrics["sqlite_transaction_duration"]
	if stat.N != 2 {
		t.Fatalf("delta count %d, want 2 (one per instance)", stat.N)
	}
	if stat.Skipped != 1 {
		t.Fatalf("skipped %d deltas, want 1 (count reset)", stat.Skipped)
	}
	if days[0].Instances["ingest"] != 2 {
		t.Fatalf("instance count %d, want 2", days[0].Instances["ingest"])
	}
}

func TestReportSqlLatencyTimeoutsAndScreening(t *testing.T) {
	day := time.Date(2026, 11, 16, 15, 0, 0, 0, time.UTC)
	id := strings.Repeat("f", 32)
	var usage []usageRecord
	for i := 0; i < 8; i++ {
		usage = append(usage, usageRecord{
			Kind: kindUsage, InstanceID: id, Role: "mcp", Tool: observe.UsageToolSQL,
			Path: observe.UsagePathLocal, Outcome: observe.UsageOutcomeOK,
			TimeMS: day.Add(time.Duration(i) * time.Minute).UnixMilli(), TotalMS: float64(i) * 10,
		})
	}
	usage = append(usage, usageRecord{
		Kind: kindUsage, InstanceID: id, Role: "mcp", Tool: observe.UsageToolSQL,
		Path: observe.UsagePathLocal, Outcome: observe.UsageOutcomeTimeout,
		TimeMS: day.Add(9 * time.Minute).UnixMilli(), TotalMS: 5100,
	})
	days := aggregateDays(nil, usage, 0)
	applyBudgets(days)
	d := days[0]
	if d.SQLN != 9 {
		t.Fatalf("sql n = %d, want 9", d.SQLN)
	}
	if d.SQLTimeouts != 1 {
		t.Fatalf("timeouts = %d, want 1", d.SQLTimeouts)
	}
	if d.SQLP95MS == nil || *d.SQLP95MS != 5100 {
		t.Fatalf("sql p95 = %v, want 5100", d.SQLP95MS)
	}
	if d.Verdict != "regression" {
		t.Fatalf("verdict %q, want regression (timeout)", d.Verdict)
	}
	expectFinding(t, d, "timed out")
	expectFinding(t, d, "screening budget")
}

func TestReportBudgetVerdicts(t *testing.T) {
	day := time.Date(2026, 11, 16, 16, 0, 0, 0, time.UTC)
	day2 := day.AddDate(0, 0, 1)
	joinID := strings.Repeat("1", 32)
	serveID := strings.Repeat("2", 32)
	samples := []sampleRecord{
		{Kind: kindSample, InstanceID: joinID, Role: "join", WallMS: day.UnixMilli(),
			Gauges:    map[string]float64{"sqlite_file_bytes{file=wal}": 100 << 20, "sqlite_file_bytes{file=db}": 1 << 30},
			Resources: map[string]observe.Reading{"rss_bytes": reading(200 << 20)}},
		{Kind: kindSample, InstanceID: serveID, Role: "serve", WallMS: day.Add(time.Minute).UnixMilli(),
			Gauges:    map[string]float64{"sqlite_file_bytes{file=wal}": 70 << 20},
			Resources: map[string]observe.Reading{"rss_bytes": reading(60 << 20)}},
		{Kind: kindSample, InstanceID: serveID, Role: "serve", WallMS: day2.UnixMilli(),
			Gauges:    map[string]float64{"sqlite_file_bytes{file=wal}": 10 << 20},
			Resources: map[string]observe.Reading{"rss_bytes": reading(61 << 20)}},
	}
	days := aggregateDays(samples, nil, 0)
	applyBudgets(days)
	if days[0].Verdict != "regression" {
		t.Fatalf("day1 verdict %q, want regression (WAL beyond variation, join RSS over ceiling)", days[0].Verdict)
	}
	expectFinding(t, days[0], "beyond the 64 MiB ceiling plus the 50% allowed variation")
	expectFinding(t, days[0], "join max RSS")
	if days[1].Verdict != "pass" {
		t.Fatalf("day2 verdict %q, want pass", days[1].Verdict)
	}
	if days[0].WALMaxBytes != 100<<20 || days[0].DBMaxBytes != 1<<30 {
		t.Fatalf("size maxima not tracked: wal %v db %v", days[0].WALMaxBytes, days[0].DBMaxBytes)
	}
	if days[0].RSSMaxByRole["serve"] != 60<<20 {
		t.Fatalf("serve rss max %v, want 60 MiB", days[0].RSSMaxByRole["serve"])
	}
}

func TestReportBandRegressionAgainstDeploymentMinimum(t *testing.T) {
	base := time.Date(2026, 11, 15, 6, 0, 0, 0, time.UTC)
	id := strings.Repeat("3", 32)
	day1 := repeatValues(11, 10.0, 12.0, 14.0, 16.0, 18.0, 20.0, 22.0, 24.0, 26.0, 28.0, 30.0)
	day2 := repeatValues(20, 100.0)
	samples := []sampleRecord{
		{Kind: kindSample, InstanceID: id, Role: "ingest", WallMS: base.UnixMilli(),
			Durations: map[string]observe.HistogramData{"ingest_pass_duration": histObservations(nil)}},
		{Kind: kindSample, InstanceID: id, Role: "ingest", WallMS: base.Add(time.Minute).UnixMilli(),
			Durations: map[string]observe.HistogramData{"ingest_pass_duration": histObservations(day1)}},
		{Kind: kindSample, InstanceID: id, Role: "ingest", WallMS: base.AddDate(0, 0, 1).UnixMilli(),
			Durations: map[string]observe.HistogramData{"ingest_pass_duration": histObservations(append(append([]float64(nil), day1...), day2...))}},
	}
	days := aggregateDays(samples, nil, 0)
	applyBudgets(days)
	if len(days) != 2 {
		t.Fatalf("aggregated %d days, want 2", len(days))
	}
	if days[0].Verdict != "pass" {
		t.Fatalf("baseline day verdict %q, want pass", days[0].Verdict)
	}
	if days[1].Verdict != "regression" {
		t.Fatalf("second day verdict %q, want regression (p95 beyond twice the daily minimum)", days[1].Verdict)
	}
	expectFinding(t, days[1], "exceeds twice the deployment's recorded daily minimum")
}

func TestReportNotesMissingServeInstrumentation(t *testing.T) {
	day := time.Date(2026, 11, 16, 5, 0, 0, 0, time.UTC)
	samples := []sampleRecord{
		{Kind: kindSample, InstanceID: strings.Repeat("4", 32), Role: "ingest", WallMS: day.UnixMilli()},
	}
	days := aggregateDays(samples, nil, 0)
	applyBudgets(days)
	expectFinding(t, days[0], "no serve instance published history")
}

func TestReportPrunesOldRecords(t *testing.T) {
	out := t.TempDir()
	old := time.Now().UTC().AddDate(0, 0, -40)
	recent := time.Now().UTC().Add(-time.Hour)
	id := strings.Repeat("5", 32)
	logPath := filepath.Join(out, capturesFileName)
	var lines []string
	for _, s := range []sampleRecord{
		{Kind: kindSample, InstanceID: id, Role: "ingest", WallMS: old.UnixMilli()},
		{Kind: kindSample, InstanceID: id, Role: "ingest", WallMS: recent.UnixMilli()},
	} {
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(raw))
	}
	if err := appendLines(logPath, lines); err != nil {
		t.Fatal(err)
	}
	data, err := runReport(t.TempDir(), out, 35, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(data.Days) != 1 || data.Days[0].Samples != 1 {
		t.Fatalf("pruned report holds %v, want one recent sample", data.Days)
	}
	samples, _, err := readCaptureLog(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 || samples[0].WallMS != recent.UnixMilli() {
		t.Fatalf("capture log not pruned: %d samples", len(samples))
	}
}

func TestSpliceDocsIdempotentPerDate(t *testing.T) {
	docsPath := filepath.Join(t.TempDir(), "performance.md")
	if err := os.WriteFile(docsPath, []byte("# perf\n\nintro\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 11, 16, 4, 0, 0, 0, time.UTC)
	data := reportData{Days: []dayReport{{Day: day.Format("2006-01-02"), Verdict: "pass"}}}
	if err := spliceDocs(docsPath, data); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(docsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), docsSectionHead) || !strings.Contains(string(raw), day.Format("2006-01-02")) {
		t.Fatalf("docs missing section after first splice:\n%s", raw)
	}
	data.Days = append(data.Days, dayReport{Day: day.AddDate(0, 0, 1).Format("2006-01-02"), Verdict: "pass"})
	if err := spliceDocs(docsPath, data); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(docsPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(raw), docsBeginMarker); got != 1 {
		t.Fatalf("marker appears %d times after second splice, want 1", got)
	}
	if got := strings.Count(string(raw), "| 2026-11-16 "); got != 1 {
		t.Fatalf("day row appears %d times, want 1 (row replaced)", got)
	}
	if got := strings.Count(string(raw), "| 2026-11-17 "); got != 1 {
		t.Fatalf("new day row missing")
	}
	if !strings.Contains(string(raw), "intro") {
		t.Fatalf("splice clobbered surrounding document content")
	}
}

func TestCaptureThenReportEndToEnd(t *testing.T) {
	state := t.TempDir()
	out := t.TempDir()
	day := time.Date(2026, 11, 16, 20, 0, 0, 0, time.UTC)
	ingestID := strings.Repeat("6", 32)
	writeTestSnapshot(t, state, ingestID, []string{"ingest"}, day.UnixMilli())
	pass := repeatValues(11, 5.0, 8.0, 9.0, 12.0, 15.0, 18.0, 21.0, 25.0, 30.0, 35.0, 40.0)
	txn := repeatValues(11, 2.0, 3.0, 4.0, 5.0, 6.0, 8.0, 9.0, 12.0, 15.0, 20.0, 25.0)
	pub := repeatValues(22, 0.3, 0.7)
	appendTestHistory(t, state, ingestID,
		testSnapshot(ingestID, []string{"ingest"}, day.UnixMilli(), map[string]observe.HistogramData{
			"ingest_pass_duration":             histObservations(nil),
			"sqlite_transaction_duration":      histObservations(nil),
			"instrumentation_publish_duration": histObservations(nil),
		}, nil, 0),
		testSnapshot(ingestID, []string{"ingest"}, day.Add(time.Minute).UnixMilli(), map[string]observe.HistogramData{
			"ingest_pass_duration":             histObservations(pass),
			"sqlite_transaction_duration":      histObservations(txn),
			"instrumentation_publish_duration": histObservations(pub),
		}, map[string]float64{"sqlite_file_bytes{file=wal}": 5 << 20}, 101<<20))
	mcpID := strings.Repeat("7", 32)
	writeTestSnapshot(t, state, mcpID, []string{"mcp"}, day.UnixMilli())
	writeTestUsage(t, state, mcpID, "usage.jsonl", []observe.UsageRecord{
		sqlUsage(mcpID, day.Add(time.Minute).UnixMilli(), 4.0, observe.UsageOutcomeOK),
		sqlUsage(mcpID, day.Add(2*time.Minute).UnixMilli(), 900.0, observe.UsageOutcomeOK),
	})
	if err := runCapture(state, out); err != nil {
		t.Fatal(err)
	}
	data, err := runReport(state, out, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(data.Days) != 1 {
		t.Fatalf("days %d, want 1", len(data.Days))
	}
	d := data.Days[0]
	if d.Metrics["ingest_pass_duration"].N != uint64(len(pass)) {
		t.Fatalf("ingest pass n = %d, want %d", d.Metrics["ingest_pass_duration"].N, len(pass))
	}
	if d.SQLN != 2 || d.SQLP95MS == nil || *d.SQLP95MS != 900 {
		t.Fatalf("sql stats wrong: %+v", d)
	}
	if d.WALMaxBytes != 5<<20 || d.RSSMaxByRole["ingest"] != 101<<20 {
		t.Fatalf("gauge/resource maxima wrong: wal %v rss %v", d.WALMaxBytes, d.RSSMaxByRole)
	}
	if err := writeReportFiles(out, data); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(out, reportMDName))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{"2026-11-16", "ingest 1", "sql", "watch:", "no serve instance"} {
		if !strings.Contains(text, want) {
			t.Fatalf("report markdown missing %q:\n%s", want, text)
		}
	}
	expectFinding(t, d, "screening budget")
}

func TestPercentileMatchesRankRule(t *testing.T) {
	values := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}
	if got := percentile(values, 0.50); got != 10 {
		t.Fatalf("p50 of 1..20 = %v, want 10", got)
	}
	if got := percentile(values, 0.95); got != 19 {
		t.Fatalf("p95 of 1..20 = %v, want 19", got)
	}
	if got := percentile([]float64{42}, 0.95); got != 42 {
		t.Fatalf("p95 of single value = %v, want 42", got)
	}
}
