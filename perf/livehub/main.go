package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nibzard/ohya/internal/observe"
)

const (
	captureFormatVersion = 1
	keepDefaultDays      = 35
	stateFileName        = "capture-state.json"
	capturesFileName     = "captures.jsonl"
	reportMDName         = "report.md"
	reportJSONName       = "report.json"

	sqlDeadlineMS     = 5000.0
	sqlScreeningP95MS = 250.0
	walCeilingBytes   = float64(64 << 20)
	walAllowedFactor  = 1.5
	bandFactor        = 2.0

	kindSample = "sample"
	kindUsage  = "usage"

	docsBeginMarker = "<!-- livehub:rows:begin -->"
	docsEndMarker   = "<!-- livehub:rows:end -->"
	docsSectionHead = "## Live-hub trend"
)

var trackedDurations = []string{
	"ingest_pass_duration",
	"sqlite_transaction_duration",
	"instrumentation_publish_duration",
	"runtime_sample_duration",
}

var trackedGaugePrefixes = []string{
	"sqlite_file_bytes",
	"watch_sources{state=active}",
	"watch_pending_complete_bytes",
}

var trackedCounterPrefixes = []string{
	"ingest_passes",
	"ingest_failures",
	"instrumentation_log_dropped",
	"instrumentation_log_coalesced",
	"instrumentation_metrics_dropped",
	"instrumentation_write_errors",
}

var trackedResources = []string{"rss_bytes", "cpu_seconds"}

var rssCeilingsByRole = map[string]float64{
	"serve": float64(192 << 20),
	"join":  float64(128 << 20),
}

type sampleRecord struct {
	Kind          string                           `json:"kind"`
	InstanceID    string                           `json:"instance_id"`
	Role          string                           `json:"role,omitempty"`
	BuildRevision string                           `json:"build_revision,omitempty"`
	WallMS        int64                            `json:"wall_ms"`
	UptimeMS      int64                            `json:"uptime_ms"`
	Durations     map[string]observe.HistogramData `json:"durations,omitempty"`
	Gauges        map[string]float64               `json:"gauges,omitempty"`
	Resources     map[string]observe.Reading       `json:"resources,omitempty"`
	Counters      map[string]uint64                `json:"counters,omitempty"`
}

type usageRecord struct {
	Kind        string  `json:"kind"`
	InstanceID  string  `json:"instance_id"`
	Role        string  `json:"role,omitempty"`
	Tool        string  `json:"tool"`
	Path        string  `json:"path"`
	Outcome     string  `json:"outcome"`
	TimeMS      int64   `json:"time_ms"`
	TotalMS     float64 `json:"total_ms"`
	FirstRowMS  float64 `json:"first_row_ms,omitempty"`
	IterationMS float64 `json:"iteration_ms,omitempty"`
	Rows        int64   `json:"rows"`
	ResultBytes int64   `json:"result_bytes"`
	FTS         bool    `json:"fts"`
}

type captureState struct {
	FormatVersion int              `json:"format_version"`
	HistoryLastMS map[string]int64 `json:"history_last_ms"`
	UsageLastMS   map[string]int64 `json:"usage_last_ms"`
}

type metricStat struct {
	N       uint64   `json:"n"`
	P50MS   *float64 `json:"p50_ms,omitempty"`
	P95MS   *float64 `json:"p95_ms,omitempty"`
	SumMS   float64  `json:"sum_ms"`
	Skipped uint64   `json:"skipped_deltas"`
}

type dayReport struct {
	Day          string                `json:"day"`
	Instances    map[string]int        `json:"instances_by_role"`
	Samples      int                   `json:"samples"`
	Metrics      map[string]metricStat `json:"metrics"`
	SQLN         int                   `json:"sql_n"`
	SQLP50MS     *float64              `json:"sql_p50_ms,omitempty"`
	SQLP95MS     *float64              `json:"sql_p95_ms,omitempty"`
	SQLMaxMS     float64               `json:"sql_max_ms"`
	SQLTimeouts  int                   `json:"sql_timeouts"`
	SQLSlow      int                   `json:"sql_over_deadline_ms"`
	WALMaxBytes  float64               `json:"wal_max_bytes"`
	DBMaxBytes   float64               `json:"db_max_bytes"`
	RSSMaxByRole map[string]float64    `json:"rss_max_bytes_by_role"`
	Findings     []string              `json:"findings,omitempty"`
	Verdict      string                `json:"verdict"`

	histographs map[string]observe.HistogramData
	sqlUsage    []usageRecord
	instanceIDs []string
}

type reportData struct {
	FormatVersion int         `json:"format_version"`
	GeneratedUTC  string      `json:"generated_utc"`
	StateDir      string      `json:"state_dir"`
	Days          []dayReport `json:"days"`
}

func main() {
	mode := flag.String("mode", "", "capture: append new observe records to the capture log; report: aggregate daily trend rows")
	state := flag.String("state", defaultStateDir(), "ohya state `directory` whose observe/ tree is tracked")
	out := flag.String("out", "", "tracking work `directory` (default <state>/perf)")
	docs := flag.String("docs", "", "docs/performance.md `path` whose livehub marker block receives the trend rows")
	keep := flag.Int("keep-days", keepDefaultDays, "drop captured records older than this many `days` during report")
	days := flag.Int("days", 0, "report only the newest N `days` (0 reports all)")
	flag.Parse()
	if *mode == "" {
		flag.Usage()
		os.Exit(2)
	}
	outDir := *out
	if outDir == "" {
		outDir = filepath.Join(*state, "perf")
	}
	var err error
	switch *mode {
	case "capture":
		err = runCapture(*state, outDir)
	case "report":
		var data reportData
		data, err = runReport(*state, outDir, *keep, *days)
		if err == nil {
			err = writeReportFiles(outDir, data)
			if err == nil && *docs != "" {
				err = spliceDocs(*docs, data)
			}
		}
	default:
		err = fmt.Errorf("unknown mode %q (want capture or report)", *mode)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "livehub:", err)
		os.Exit(1)
	}
}

func defaultStateDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".ohya"
	}
	return filepath.Join(home, ".ohya")
}

func runCapture(stateDir, outDir string) error {
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return fmt.Errorf("create tracking directory: %w", err)
	}
	statePath := filepath.Join(outDir, stateFileName)
	logPath := filepath.Join(outDir, capturesFileName)
	st, err := loadCaptureState(statePath, logPath)
	if err != nil {
		return err
	}
	views, _, err := observe.DiscoverInstances(filepath.Join(stateDir, "observe"))
	if err != nil {
		return fmt.Errorf("discover instances: %w", err)
	}
	var fresh []string
	for _, view := range views {
		role, revision := instanceLabels(view)
		samples, err := readHistorySamples(filepath.Join(view.Dir, "history.jsonl"))
		if err != nil {
			return err
		}
		for _, snapshot := range samples {
			if snapshot.SampledWallMS <= st.HistoryLastMS[view.InstanceID] {
				continue
			}
			record := sampleRecord{
				Kind:          kindSample,
				InstanceID:    view.InstanceID,
				Role:          role,
				BuildRevision: revision,
				WallMS:        snapshot.SampledWallMS,
				UptimeMS:      snapshot.UptimeMS,
				Durations:     trackedDurationsOf(snapshot),
				Gauges:        trackedGaugesOf(snapshot),
				Resources:     trackedResourcesOf(snapshot),
				Counters:      trackedCountersOf(snapshot),
			}
			line, err := json.Marshal(record)
			if err != nil {
				return fmt.Errorf("encode sample record: %w", err)
			}
			fresh = append(fresh, string(line))
			st.HistoryLastMS[view.InstanceID] = snapshot.SampledWallMS
		}
		usage, err := readUsageRecords(view.Dir)
		if err != nil {
			return err
		}
		for _, u := range usage {
			if u.TimeMS <= st.UsageLastMS[view.InstanceID] {
				continue
			}
			record := usageRecord{
				Kind: kindUsage, InstanceID: view.InstanceID, Role: role,
				Tool: u.Tool, Path: u.Path, Outcome: u.Outcome, TimeMS: u.TimeMS,
				TotalMS: u.PhaseMS.TotalMS, FirstRowMS: u.PhaseMS.FirstRowMS,
				IterationMS: u.PhaseMS.IterationMS, Rows: u.Rows,
				ResultBytes: u.ResultBytes, FTS: u.FTS,
			}
			line, err := json.Marshal(record)
			if err != nil {
				return fmt.Errorf("encode usage record: %w", err)
			}
			fresh = append(fresh, string(line))
			st.UsageLastMS[view.InstanceID] = u.TimeMS
		}
	}
	if err := appendLines(logPath, fresh); err != nil {
		return err
	}
	if err := saveCaptureState(statePath, st); err != nil {
		return err
	}
	fmt.Printf("livehub capture: %d new records from %d instances\n", len(fresh), len(views))
	return nil
}

func instanceLabels(view observe.InstanceView) (role, revision string) {
	if view.Snapshot == nil {
		return "", ""
	}
	return strings.Join(view.Snapshot.Instance.Roles, ","), view.Snapshot.Instance.BuildRevision
}

func loadCaptureState(statePath, logPath string) (captureState, error) {
	st := captureState{FormatVersion: captureFormatVersion, HistoryLastMS: map[string]int64{}, UsageLastMS: map[string]int64{}}
	raw, err := os.ReadFile(statePath)
	if err == nil {
		if err := json.Unmarshal(raw, &st); err != nil {
			return captureState{}, fmt.Errorf("decode capture state: %w", err)
		}
		if st.HistoryLastMS == nil {
			st.HistoryLastMS = map[string]int64{}
		}
		if st.UsageLastMS == nil {
			st.UsageLastMS = map[string]int64{}
		}
		return st, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return captureState{}, fmt.Errorf("read capture state: %w", err)
	}
	samples, usage, err := readCaptureLog(logPath)
	if err != nil {
		return captureState{}, err
	}
	for _, s := range samples {
		if s.WallMS > st.HistoryLastMS[s.InstanceID] {
			st.HistoryLastMS[s.InstanceID] = s.WallMS
		}
	}
	for _, u := range usage {
		if u.TimeMS > st.UsageLastMS[u.InstanceID] {
			st.UsageLastMS[u.InstanceID] = u.TimeMS
		}
	}
	return st, nil
}

func saveCaptureState(statePath string, st captureState) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("encode capture state: %w", err)
	}
	return writeFileAtomic(statePath, raw)
}

func readHistorySamples(path string) ([]observe.Snapshot, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open history: %w", err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(io.LimitReader(file, observe.MaxHistoryFileBytes+observe.MaxSnapshotFileBytes+1))
	scanner.Buffer(make([]byte, 0, 64*1024), int(observe.MaxSnapshotFileBytes)+1024)
	var samples []observe.Snapshot
	for scanner.Scan() {
		var snapshot observe.Snapshot
		if err := json.Unmarshal(scanner.Bytes(), &snapshot); err != nil || snapshot.Validate() != nil {
			continue
		}
		samples = append(samples, snapshot)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read history %s: %w", path, err)
	}
	return samples, nil
}

func readUsageRecords(dir string) ([]observe.UsageRecord, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "usage.jsonl*"))
	if err != nil {
		return nil, err
	}
	var records []observe.UsageRecord
	for _, path := range matches {
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(io.LimitReader(file, 64<<20))
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			var record observe.UsageRecord
			if err := json.Unmarshal(scanner.Bytes(), &record); err != nil || record.Validate() != nil {
				continue
			}
			records = append(records, record)
		}
		file.Close()
	}
	sort.Slice(records, func(i, j int) bool { return records[i].TimeMS < records[j].TimeMS })
	return records, nil
}

func trackedDurationsOf(s observe.Snapshot) map[string]observe.HistogramData {
	out := map[string]observe.HistogramData{}
	for _, name := range trackedDurations {
		if d, ok := s.Durations[name]; ok {
			out[name] = d
		}
	}
	return out
}

func trackedGaugesOf(s observe.Snapshot) map[string]float64 {
	out := map[string]float64{}
	for key, value := range s.Gauges {
		for _, prefix := range trackedGaugePrefixes {
			if strings.HasPrefix(key, prefix) {
				out[key] = value
				break
			}
		}
	}
	return out
}

func trackedResourcesOf(s observe.Snapshot) map[string]observe.Reading {
	out := map[string]observe.Reading{}
	for _, name := range trackedResources {
		if r, ok := s.Resources[name]; ok {
			out[name] = r
		}
	}
	return out
}

func trackedCountersOf(s observe.Snapshot) map[string]uint64 {
	out := map[string]uint64{}
	for key, value := range s.Counters {
		for _, prefix := range trackedCounterPrefixes {
			if strings.HasPrefix(key, prefix) {
				out[key] = value
				break
			}
		}
	}
	return out
}

func appendLines(path string, lines []string) error {
	if len(lines) == 0 {
		return nil
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open capture log: %w", err)
	}
	defer file.Close()
	for _, line := range lines {
		if _, err := file.WriteString(line + "\n"); err != nil {
			return fmt.Errorf("append capture log: %w", err)
		}
	}
	return nil
}

func writeFileAtomic(path string, data []byte) error {
	base := filepath.Base(path)
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+strings.TrimSuffix(base, filepath.Ext(base))+"-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("replace file: %w", err)
	}
	return nil
}

func readCaptureLog(path string) ([]sampleRecord, []usageRecord, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("open capture log: %w", err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)
	var samples []sampleRecord
	var usage []usageRecord
	for scanner.Scan() {
		line := scanner.Bytes()
		var head struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal(line, &head) != nil {
			continue
		}
		switch head.Kind {
		case kindSample:
			var r sampleRecord
			if json.Unmarshal(line, &r) == nil && r.WallMS > 0 {
				samples = append(samples, r)
			}
		case kindUsage:
			var r usageRecord
			if json.Unmarshal(line, &r) == nil && r.TimeMS > 0 {
				usage = append(usage, r)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("read capture log: %w", err)
	}
	dedupSamples(&samples)
	dedupUsage(&usage)
	return samples, usage, nil
}

func dedupSamples(samples *[]sampleRecord) {
	seen := map[string]map[int64]bool{}
	out := (*samples)[:0]
	for _, s := range *samples {
		if seen[s.InstanceID] == nil {
			seen[s.InstanceID] = map[int64]bool{}
		}
		if seen[s.InstanceID][s.WallMS] {
			continue
		}
		seen[s.InstanceID][s.WallMS] = true
		out = append(out, s)
	}
	*samples = out
}

func dedupUsage(usage *[]usageRecord) {
	seen := map[string]map[int64]bool{}
	out := (*usage)[:0]
	for _, u := range *usage {
		if seen[u.InstanceID] == nil {
			seen[u.InstanceID] = map[int64]bool{}
		}
		if seen[u.InstanceID][u.TimeMS] {
			continue
		}
		seen[u.InstanceID][u.TimeMS] = true
		out = append(out, u)
	}
	*usage = out
}

func runReport(stateDir, outDir string, keepDays, lastN int) (reportData, error) {
	samples, usage, err := readCaptureLog(filepath.Join(outDir, capturesFileName))
	if err != nil {
		return reportData{}, err
	}
	cutoffMS := int64(0)
	if keepDays > 0 {
		cutoffMS = time.Now().UTC().AddDate(0, 0, -keepDays).UnixMilli()
	}
	if err := pruneCaptureLog(filepath.Join(outDir, capturesFileName), samples, usage, cutoffMS); err != nil {
		return reportData{}, err
	}
	days := aggregateDays(samples, usage, cutoffMS)
	applyBudgets(days)
	if lastN > 0 && len(days) > lastN {
		days = days[len(days)-lastN:]
	}
	return reportData{
		FormatVersion: captureFormatVersion,
		GeneratedUTC:  time.Now().UTC().Format(time.RFC3339),
		StateDir:      stateDir,
		Days:          days,
	}, nil
}

func pruneCaptureLog(path string, samples []sampleRecord, usage []usageRecord, cutoffMS int64) error {
	var kept []string
	for _, s := range samples {
		if s.WallMS < cutoffMS {
			continue
		}
		line, err := json.Marshal(s)
		if err != nil {
			return err
		}
		kept = append(kept, string(line))
	}
	for _, u := range usage {
		if u.TimeMS < cutoffMS {
			continue
		}
		line, err := json.Marshal(u)
		if err != nil {
			return err
		}
		kept = append(kept, string(line))
	}
	if len(kept) == len(samples)+len(usage) {
		return nil
	}
	var buf strings.Builder
	for _, line := range kept {
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	return writeFileAtomic(path, []byte(buf.String()))
}

func aggregateDays(samples []sampleRecord, usage []usageRecord, cutoffMS int64) []dayReport {
	byDay := map[string]*dayReport{}
	day := func(ms int64) *dayReport {
		key := time.UnixMilli(ms).UTC().Format("2006-01-02")
		d := byDay[key]
		if d == nil {
			d = &dayReport{
				Day: key, Instances: map[string]int{},
				Metrics: map[string]metricStat{}, RSSMaxByRole: map[string]float64{},
				histographs: map[string]observe.HistogramData{},
			}
			byDay[key] = d
		}
		return d
	}
	byInstance := map[string][]sampleRecord{}
	for _, s := range samples {
		if s.WallMS < cutoffMS {
			continue
		}
		byInstance[s.InstanceID] = append(byInstance[s.InstanceID], s)
	}
	for _, list := range byInstance {
		sort.Slice(list, func(i, j int) bool { return list[i].WallMS < list[j].WallMS })
		role := list[0].Role
		for _, s := range list {
			d := day(s.WallMS)
			d.Samples++
			if role != "" && !instanceCounted(d, s) {
				d.Instances[role]++
			}
			absorbGauges(d, s)
		}
		for i := 1; i < len(list); i++ {
			earlier, later := list[i-1], list[i]
			d := day(later.WallMS)
			for _, name := range trackedDurations {
				before, ok1 := earlier.Durations[name]
				after, ok2 := later.Durations[name]
				if !ok1 || !ok2 {
					continue
				}
				delta, ok := histogramDelta(before, after)
				if !ok {
					stat := d.Metrics[name]
					stat.Skipped++
					d.Metrics[name] = stat
					continue
				}
				stat := d.Metrics[name]
				stat.N += delta.Count
				stat.SumMS += delta.SumMS
				d.Metrics[name] = stat
				d.addHistogram(name, delta)
			}
		}
	}
	for _, u := range usage {
		if u.TimeMS < cutoffMS || u.Tool != observe.UsageToolSQL {
			continue
		}
		day(u.TimeMS).sqlUsage = append(day(u.TimeMS).sqlUsage, u)
	}
	names := make([]string, 0, len(byDay))
	for name := range byDay {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]dayReport, 0, len(names))
	for _, name := range names {
		d := byDay[name]
		finalizeDay(d)
		out = append(out, *d)
	}
	return out
}

func instanceCounted(d *dayReport, s sampleRecord) bool {
	for i := range d.instanceIDs {
		if d.instanceIDs[i] == s.InstanceID {
			return true
		}
	}
	d.instanceIDs = append(d.instanceIDs, s.InstanceID)
	return false
}

func (d *dayReport) addHistogram(name string, delta observe.HistogramData) {
	h, ok := d.histographs[name]
	if !ok {
		h = observe.HistogramData{
			BoundsMS: delta.BoundsMS,
			Buckets:  append([]uint64(nil), delta.Buckets...),
			Count:    delta.Count,
			SumMS:    delta.SumMS,
		}
		d.histographs[name] = h
		return
	}
	for i, value := range delta.Buckets {
		if i < len(h.Buckets) {
			h.Buckets[i] += value
		}
	}
	h.Count += delta.Count
	h.SumMS += delta.SumMS
	d.histographs[name] = h
}

func histogramDelta(before, after observe.HistogramData) (observe.HistogramData, bool) {
	if len(before.Buckets) != len(after.Buckets) || len(before.BoundsMS) != len(after.BoundsMS) {
		return observe.HistogramData{}, false
	}
	if after.Count < before.Count || after.SumMS < before.SumMS {
		return observe.HistogramData{}, false
	}
	delta := observe.HistogramData{BoundsMS: after.BoundsMS, Buckets: make([]uint64, len(after.Buckets))}
	for i, value := range after.Buckets {
		if value < before.Buckets[i] {
			return observe.HistogramData{}, false
		}
		delta.Buckets[i] = value - before.Buckets[i]
	}
	delta.Count = after.Count - before.Count
	delta.SumMS = after.SumMS - before.SumMS
	return delta, true
}

func absorbGauges(d *dayReport, s sampleRecord) {
	for key, value := range s.Gauges {
		switch {
		case key == "sqlite_file_bytes{file=wal}" && value > d.WALMaxBytes:
			d.WALMaxBytes = value
		case key == "sqlite_file_bytes{file=db}" && value > d.DBMaxBytes:
			d.DBMaxBytes = value
		}
	}
	if r, ok := s.Resources["rss_bytes"]; ok && r.Value != nil && s.Role != "" && *r.Value > d.RSSMaxByRole[s.Role] {
		d.RSSMaxByRole[s.Role] = *r.Value
	}
}

func finalizeDay(d *dayReport) {
	for name, h := range d.histographs {
		stat := d.Metrics[name]
		if p50, ok := h.Quantile(0.5); ok {
			p50v := p50
			stat.P50MS = &p50v
		}
		if p95, ok := h.Quantile(0.95); ok {
			p95v := p95
			stat.P95MS = &p95v
		}
		d.Metrics[name] = stat
	}
	if len(d.sqlUsage) == 0 {
		return
	}
	totals := make([]float64, 0, len(d.sqlUsage))
	for _, u := range d.sqlUsage {
		totals = append(totals, u.TotalMS)
		switch {
		case u.Outcome == observe.UsageOutcomeTimeout:
			d.SQLTimeouts++
		case u.TotalMS > sqlDeadlineMS:
			d.SQLSlow++
		}
		if u.TotalMS > d.SQLMaxMS {
			d.SQLMaxMS = u.TotalMS
		}
	}
	sort.Float64s(totals)
	p50 := percentile(totals, 0.50)
	p95 := percentile(totals, 0.95)
	d.SQLN = len(totals)
	d.SQLP50MS = &p50
	d.SQLP95MS = &p95
}

func percentile(sorted []float64, fraction float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(fraction * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

func applyBudgets(days []dayReport) {
	prior := map[string][]float64{}
	for i := range days {
		d := &days[i]
		if d.SQLTimeouts > 0 {
			d.finding(fmt.Sprintf("regression: %d sql calls timed out at the %.0f ms deadline (round-8 loaded-query budget: 0)", d.SQLTimeouts, sqlDeadlineMS))
		}
		if d.SQLP95MS != nil && *d.SQLP95MS > sqlScreeningP95MS {
			d.finding(fmt.Sprintf("watch: sql p95 %.0f ms above the %.0f ms round-8 wide-match screening budget (agent-driven mix, not the fixed matrix patterns; n %d)", *d.SQLP95MS, sqlScreeningP95MS, d.SQLN))
		}
		if d.WALMaxBytes > walCeilingBytes {
			if d.WALMaxBytes > walCeilingBytes*walAllowedFactor {
				d.finding(fmt.Sprintf("regression: peak WAL %.1f MiB beyond the 64 MiB ceiling plus the 50%% allowed variation (T070/T074 spread 27-65 MiB)", d.WALMaxBytes/(1<<20)))
			} else {
				d.finding(fmt.Sprintf("note: peak WAL %.1f MiB over the 64 MiB ceiling but inside the 50%% allowed variation (T070/T074 spread 27-65 MiB)", d.WALMaxBytes/(1<<20)))
			}
		}
		for role, rss := range d.RSSMaxByRole {
			if ceiling, ok := rssCeilingsByRole[role]; ok && rss > ceiling {
				d.finding(fmt.Sprintf("regression: %s max RSS %.1f MiB over the %.0f MiB round-8 ceiling", role, rss/(1<<20), ceiling/(1<<20)))
			}
		}
		for _, name := range trackedDurations {
			stat := d.Metrics[name]
			if stat.P95MS == nil {
				continue
			}
			if mins := prior[name]; len(mins) > 0 {
				min := mins[0]
				for _, v := range mins {
					if v < min {
						min = v
					}
				}
				if *stat.P95MS > bandFactor*min {
					d.finding(fmt.Sprintf("regression: %s p95 %.1f ms exceeds twice the deployment's recorded daily minimum (%.1f ms)", name, *stat.P95MS, min))
				}
			}
			prior[name] = append(prior[name], *stat.P95MS)
		}
		if _, ok := d.Instances["serve"]; !ok {
			d.finding("note: no serve instance published history this day; hub-side metrics absent until the hub process runs an instrumented build")
		}
		d.Verdict = "pass"
		for _, f := range d.Findings {
			if strings.HasPrefix(f, "regression") {
				d.Verdict = "regression"
			}
		}
	}
}

func (d *dayReport) finding(text string) {
	d.Findings = append(d.Findings, text)
}

func writeReportFiles(outDir string, data reportData) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(outDir, reportJSONName), raw); err != nil {
		return err
	}
	var md strings.Builder
	md.WriteString("# ohya live-hub performance trend\n\n")
	md.WriteString(fmt.Sprintf("Generated %s from %s. Duration percentiles are histogram bucket estimates; sql percentiles are exact per-call values from usage records.\n\n", data.GeneratedUTC, filepath.Join(data.StateDir, "perf", capturesFileName)))
	md.WriteString(renderDocsBlock(data))
	return writeFileAtomic(filepath.Join(outDir, reportMDName), []byte(md.String()))
}

func renderDocsBlock(data reportData) string {
	var b strings.Builder
	b.WriteString(docsBeginMarker + "\n\n")
	b.WriteString("| Date | Coverage | Ingest pass p50/p95 ms (n) | SQLite txn p50/p95 ms (n) | Publish p50/p95 ms (n) | sql p50/p95 ms (n; timeouts) | Peak WAL MiB | Max RSS MiB (role) | Verdict |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|\n")
	for i := range data.Days {
		d := &data.Days[i]
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s | %s | %s |\n",
			d.Day, coverageText(d),
			durationCell(d, "ingest_pass_duration"),
			durationCell(d, "sqlite_transaction_duration"),
			durationCell(d, "instrumentation_publish_duration"),
			sqlCell(d),
			sizeCell(d.WALMaxBytes),
			rssCell(d),
			d.Verdict,
		)
	}
	b.WriteString("\n")
	for i := range data.Days {
		d := &data.Days[i]
		for _, f := range d.Findings {
			fmt.Fprintf(&b, "- %s: %s\n", d.Day, f)
		}
	}
	b.WriteString("\n" + docsEndMarker + "\n")
	return b.String()
}

func coverageText(d *dayReport) string {
	roles := make([]string, 0, len(d.Instances))
	for role := range d.Instances {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	parts := make([]string, 0, len(roles)+1)
	for _, role := range roles {
		parts = append(parts, fmt.Sprintf("%s %d", role, d.Instances[role]))
	}
	parts = append(parts, fmt.Sprintf("%d samples", d.Samples))
	return strings.Join(parts, ", ")
}

func durationCell(d *dayReport, name string) string {
	stat, ok := d.Metrics[name]
	if !ok || stat.N == 0 {
		return "-"
	}
	p50, p95 := "-", "-"
	if stat.P50MS != nil {
		p50 = trimMS(*stat.P50MS)
	}
	if stat.P95MS != nil {
		p95 = trimMS(*stat.P95MS)
	}
	cell := fmt.Sprintf("%s / %s (n %d)", p50, p95, stat.N)
	if stat.Skipped > 0 {
		cell += fmt.Sprintf(", %d skipped", stat.Skipped)
	}
	return cell
}

func sqlCell(d *dayReport) string {
	if d.SQLN == 0 {
		return "-"
	}
	return fmt.Sprintf("%s / %s (n %d; %d)", trimMS(*d.SQLP50MS), trimMS(*d.SQLP95MS), d.SQLN, d.SQLTimeouts)
}

func sizeCell(bytes float64) string {
	if bytes == 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f", bytes/(1<<20))
}

func rssCell(d *dayReport) string {
	if len(d.RSSMaxByRole) == 0 {
		return "-"
	}
	var parts []string
	for _, role := range sortedKeysOf(d.RSSMaxByRole) {
		parts = append(parts, fmt.Sprintf("%s %.1f", role, d.RSSMaxByRole[role]/(1<<20)))
	}
	return strings.Join(parts, ", ")
}

func sortedKeysOf(m map[string]float64) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func trimMS(v float64) string {
	if v >= 100 {
		return fmt.Sprintf("%.0f", v)
	}
	if v >= 10 {
		return fmt.Sprintf("%.1f", v)
	}
	return fmt.Sprintf("%.2f", v)
}

func spliceDocs(path string, data reportData) error {
	block := renderDocsBlock(data)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return os.WriteFile(path, []byte("\n"+docsSectionHead+"\n\n"+block), 0o644)
	}
	if err != nil {
		return fmt.Errorf("read docs: %w", err)
	}
	text := string(raw)
	begin := strings.Index(text, docsBeginMarker)
	end := strings.Index(text, docsEndMarker)
	if begin < 0 || end < 0 || end < begin {
		if len(text) > 0 && !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		text += "\n" + docsSectionHead + "\n\n" + block
	} else {
		after := text[end+len(docsEndMarker):]
		text = text[:begin] + strings.TrimRight(block, "\n") + after
	}
	return os.WriteFile(path, []byte(text), 0o644)
}
