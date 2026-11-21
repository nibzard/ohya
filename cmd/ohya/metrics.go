package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nibzard/ohya/internal/observe"
)

const (
	metricsFormatVersion    = 1
	metricsHistoryCadenceMS = int64(60_000)
)

var metricsSpec = commandSpec{
	name:      "ohya metrics",
	synopsis:  "report counter rates and durations over a time window",
	usageLine: "ohya metrics --window 1m|5m|1h|24h [flags]",
	description: "Reads the bounded history samples each publishing process appends once per minute under the state directory's observe/ tree. " +
		"Counters are deltas between the first and last sample inside the window; rates divide those deltas by the monotonic time the process actually ran, so a requested window is never manufactured from less data. " +
		"Quantiles are bucket estimates that need at least twenty observations, and percentiles are never averaged between instances. " +
		"Exit codes: 0 for a complete window, 3 for a valid but partial or sample-less result, 1 for failure, 2 for usage.",
	argsTitle: "Arguments",
	args:      "(none)",
	examples:  []string{"ohya metrics --window 5m --json", "ohya metrics --window 1h --role ingest"},
}

type metricsHistogram struct {
	BoundsMS []float64 `json:"bounds_ms"`
	Buckets  []uint64  `json:"buckets"`
	Count    uint64    `json:"count"`
	SumMS    float64   `json:"sum_ms"`
	P50      *float64  `json:"p50"`
	P95      *float64  `json:"p95"`
}

type metricsCompleteness struct {
	CoversWindow bool   `json:"covers_window"`
	Reason       string `json:"reason,omitempty"`
}

type instanceMetrics struct {
	InstanceID           string                      `json:"instance_id"`
	PID                  int                         `json:"pid,omitempty"`
	Roles                []string                    `json:"roles,omitempty"`
	SampleCount          int                         `json:"sample_count"`
	FirstWallMS          *int64                      `json:"first_wall_ms"`
	LastWallMS           *int64                      `json:"last_wall_ms"`
	ObservedMS           *int64                      `json:"observed_ms"`
	MonotonicElapsedMS   *int64                      `json:"monotonic_elapsed_ms"`
	Gaps                 *int                        `json:"gaps"`
	ExpectedSamples      *int                        `json:"expected_samples"`
	Counters             map[string]uint64           `json:"counters,omitempty"`
	UnavailableCounters  []string                    `json:"unavailable_counters,omitempty"`
	RatesPerSecond       map[string]float64          `json:"rates_per_second,omitempty"`
	Durations            map[string]metricsHistogram `json:"durations,omitempty"`
	UnavailableDurations []string                    `json:"unavailable_durations,omitempty"`
	CorruptSamples       int                         `json:"corrupt_samples"`
	DecodeError          string                      `json:"decode_error,omitempty"`
	Completeness         metricsCompleteness         `json:"completeness"`
}

type metricsData struct {
	FormatVersion   int               `json:"format_version"`
	Window          string            `json:"window"`
	RequestedMS     int64             `json:"requested_ms"`
	GeneratedWallMS int64             `json:"generated_wall_ms"`
	GeneratedUTC    string            `json:"generated_utc"`
	Instances       []instanceMetrics `json:"instances"`
	Findings        []statusFinding   `json:"findings"`
	Coverage        statusCoverage    `json:"coverage"`
}

type metricsOptions struct {
	stateDir   string
	role       string
	instanceID string
	window     string
	all        bool
	nowMS      func() int64
}

func runMetricsCommand(args []string, stdout, stderr io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	c := newCommandFlags(metricsSpec)
	window := c.fs.String("window", "5m", "measurement `window`: 1m, 5m, 1h, or 24h")
	db := c.fs.String("db", filepath.Join(home, ".ohya", "hub.db"),
		"hub database `path` used to resolve the default state directory")
	state := c.fs.String("state", "",
		"state `directory` whose observe/ tree holds history samples; defaults to the database directory")
	role := c.fs.String("role", "",
		"restrict the report to instances carrying this `role` (serve, join, ingest, mcp)")
	instance := c.fs.String("instance", "",
		"restrict the report to one instance `id`")
	all := c.fs.Bool("all", false,
		"include retained terminated instances with no samples in the window")
	asJSON := c.fs.Bool("json", false, "print one JSON object instead of the text report (docs/adr/0005)")
	if err := c.parse(args, stdout); err != nil {
		return err
	}
	if c.fs.NArg() > 0 {
		return c.usageErrorf("unexpected argument %q", c.fs.Arg(0))
	}
	requestedMS, ok := parseMetricsWindow(*window)
	if !ok {
		return c.usageErrorf("--window must be one of 1m, 5m, 1h, 24h")
	}
	if *role != "" {
		if _, ok := observe.ParseRole(*role); !ok {
			return c.usageErrorf("--role must be one of serve, join, ingest, mcp")
		}
	}
	stateDir, _, err := resolveObserveStateDir(*state, *db)
	if err != nil {
		return jsonFailurePrint(*asJSON, stdout, metricsSpec.name, err)
	}
	o := metricsOptions{
		stateDir:   stateDir,
		role:       *role,
		instanceID: *instance,
		window:     *window,
		all:        *all,
		nowMS:      func() int64 { return time.Now().UnixMilli() },
	}
	data, err := buildMetrics(o, requestedMS)
	if err != nil {
		return jsonFailurePrint(*asJSON, stdout, metricsSpec.name, err)
	}
	if *asJSON {
		if err := printJSON(stdout, metricsSpec.name, data); err != nil {
			return err
		}
	} else {
		writeMetricsText(stdout, data)
	}
	if metricsDegraded(data) {
		return errDegraded
	}
	return nil
}

func parseMetricsWindow(window string) (int64, bool) {
	switch window {
	case "1m":
		return 60_000, true
	case "5m":
		return 300_000, true
	case "1h":
		return 3_600_000, true
	case "24h":
		return 86_400_000, true
	}
	return 0, false
}

func metricsDegraded(data metricsData) bool {
	for _, f := range data.Findings {
		if f.Severity == sevWarn || f.Severity == sevError {
			return true
		}
	}
	return false
}

func buildMetrics(o metricsOptions, requestedMS int64) (metricsData, error) {
	nowMS := o.nowMS()
	data := metricsData{
		FormatVersion:   metricsFormatVersion,
		Window:          o.window,
		RequestedMS:     requestedMS,
		GeneratedWallMS: nowMS,
		GeneratedUTC:    time.UnixMilli(nowMS).UTC().Format(time.RFC3339),
		Instances:       []instanceMetrics{},
		Findings:        []statusFinding{},
	}
	views, truncated, err := observe.DiscoverInstances(filepath.Join(o.stateDir, "observe"))
	if err != nil {
		return metricsData{}, err
	}
	if truncated {
		data.Coverage.TruncatedDiscovery = true
		data.Findings = append(data.Findings, statusFinding{
			Code: "discovery_truncated", Severity: sevWarn,
			Evidence: fmt.Sprintf("more than %d live instances publish under %s", observe.MaxLiveInstances, o.stateDir),
			Hint:     "pass --instance or --role to select one instance explicitly",
		})
	}
	if o.instanceID != "" {
		view, err := observe.LoadInstanceView(filepath.Join(o.stateDir, "observe"), o.instanceID)
		if err != nil {
			return metricsData{}, err
		}
		views = []observe.InstanceView{view}
	}
	sinceMS := nowMS - requestedMS
	seen := 0
	omittedRetained := 0
	for _, view := range views {
		if o.role != "" && !view.HasRole(o.role) {
			continue
		}
		m := buildInstanceMetrics(view, sinceMS, nowMS, requestedMS)
		if o.instanceID == "" && !o.all && m.SampleCount == 0 && view.Snapshot != nil && view.DecodeErr == "" &&
			(view.Snapshot.Stopped || observe.ProbePID(view.Snapshot.Instance.PID) == "dead") {
			omittedRetained++
			continue
		}
		seen++
		data.Instances = append(data.Instances, m)
	}
	if omittedRetained > 0 {
		data.Coverage.Notes = append(data.Coverage.Notes,
			fmt.Sprintf("%d retained terminated instances with no samples in window omitted; pass --all to include", omittedRetained))
	}
	if seen == 0 && o.instanceID == "" {
		data.Coverage.Missing = append(data.Coverage.Missing, "instances")
		data.Findings = append(data.Findings, statusFinding{
			Code: "no_instances", Severity: sevWarn,
			Evidence: fmt.Sprintf("no instance publishes history under %s", filepath.Join(o.stateDir, "observe")),
			Hint:     "no long-running ohya process may be running in this state directory, or one runs with --observe off",
		})
	}
	liveByRole := map[string]int{}
	for _, instance := range data.Instances {
		if instance.DecodeError == "" && instance.SampleCount > 0 {
			for _, role := range instance.Roles {
				liveByRole[role]++
			}
		}
		if instance.SampleCount == 0 {
			reason := "no history samples inside the window"
			if instance.DecodeError != "" {
				reason = instance.DecodeError
			}
			data.Findings = append(data.Findings, statusFinding{
				Code: "no_history_samples", Severity: sevWarn,
				Evidence: fmt.Sprintf("instance %s: %s", shortID(instance.InstanceID), reason),
				Hint:     "history gains its first sample at process startup and one sample per minute after",
			})
			data.Coverage.Stale = append(data.Coverage.Stale, instance.InstanceID)
			continue
		}
		if !instance.Completeness.CoversWindow {
			data.Findings = append(data.Findings, statusFinding{
				Code: "window_partial", Severity: sevWarn,
				Evidence: fmt.Sprintf("instance %s: %s", shortID(instance.InstanceID), instance.Completeness.Reason),
				Hint:     "the observed span is reported next to the requested window; rates use only the observed span",
			})
		}
	}
	for _, role := range sortedKeys(liveByRole) {
		count := liveByRole[role]
		if count > 1 && role != string(observe.RoleMCP) {
			data.Findings = append(data.Findings, statusFinding{
				Code: "multiple_instances", Severity: sevWarn,
				Evidence: fmt.Sprintf("%d instances with role %s publish in this state directory; per-instance results are never merged", count, role),
				Hint:     "pass --instance to select one instance explicitly",
			})
		}
		data.Coverage.Present = append(data.Coverage.Present, role)
	}
	sortStrings(data.Coverage.Present)
	sortStrings(data.Coverage.Stale)
	data.Coverage.Notes = append(data.Coverage.Notes,
		"rate denominator is monotonic elapsed time within one process",
		"samples are one bounded aggregate per minute per instance",
		"quantiles are histogram bucket estimates")
	if !metricsDegraded(data) {
		data.Findings = append(data.Findings, statusFinding{
			Code: "window_complete", Severity: sevOK,
			Evidence: "every selected instance covers the requested window without gaps",
		})
	}
	return data, nil
}

func buildInstanceMetrics(view observe.InstanceView, sinceMS, nowMS, requestedMS int64) instanceMetrics {
	instance := instanceMetrics{
		InstanceID:  view.InstanceID,
		DecodeError: view.DecodeErr,
	}
	if view.Snapshot != nil {
		instance.PID = view.Snapshot.Instance.PID
		instance.Roles = view.Snapshot.Instance.Roles
	}
	samples, corrupt := readHistorySamples(filepath.Join(view.Dir, "history.jsonl"))
	instance.CorruptSamples = corrupt
	var inWindow []observe.Snapshot
	for _, sample := range samples {
		if sample.SampledWallMS >= sinceMS && sample.SampledWallMS <= nowMS+observe.SnapshotStaleAge.Milliseconds() {
			inWindow = append(inWindow, sample)
		}
	}
	if len(inWindow) == 0 {
		instance.Completeness = metricsCompleteness{CoversWindow: false, Reason: "no samples inside the requested window"}
		return instance
	}
	first, last := inWindow[0], inWindow[len(inWindow)-1]
	instance.SampleCount = len(inWindow)
	instance.FirstWallMS = &first.SampledWallMS
	instance.LastWallMS = &last.SampledWallMS
	observed := last.SampledWallMS - first.SampledWallMS
	instance.ObservedMS = &observed
	monotonic := last.UptimeMS - first.UptimeMS
	if monotonic >= 0 {
		instance.MonotonicElapsedMS = &monotonic
	}
	gaps := historyGaps(inWindow)
	instance.Gaps = &gaps
	expected := int((requestedMS + metricsHistoryCadenceMS - 1) / metricsHistoryCadenceMS)
	if observed < requestedMS {
		expected = int((observed + metricsHistoryCadenceMS - 1) / metricsHistoryCadenceMS)
	}
	instance.ExpectedSamples = &expected
	window := observe.Delta(&last, &first)
	instance.Counters = window.Counters
	instance.UnavailableCounters = window.UnavailableCounters
	if monotonic > 0 && window.SameInstance {
		instance.RatesPerSecond = make(map[string]float64, len(window.Counters))
		seconds := float64(monotonic) / 1000
		for key, delta := range window.Counters {
			instance.RatesPerSecond[key] = float64(delta) / seconds
		}
	}
	instance.Durations, instance.UnavailableDurations = durationDeltas(first, last)
	reason := ""
	switch {
	case observed < requestedMS:
		reason = fmt.Sprintf("observed %s of the requested %s window", ageText(observed), ageText(requestedMS))
	case gaps > 0:
		reason = fmt.Sprintf("%d missing minute samples inside the window", gaps)
	case window.ElapsedMS == nil:
		reason = "monotonic elapsed time is unavailable across the samples"
	}
	instance.Completeness = metricsCompleteness{CoversWindow: reason == "", Reason: reason}
	return instance
}

func historyGaps(samples []observe.Snapshot) int {
	gaps := 0
	for i := 1; i < len(samples); i++ {
		steps := (samples[i].SampledWallMS - samples[i-1].SampledWallMS) / metricsHistoryCadenceMS
		if steps > 1 {
			gaps += int(steps - 1)
		}
	}
	return gaps
}

func durationDeltas(before, after observe.Snapshot) (map[string]metricsHistogram, []string) {
	out := make(map[string]metricsHistogram, len(after.Durations))
	var unavailable []string
	for key, later := range after.Durations {
		earlier, ok := before.Durations[key]
		if !ok || later.Count < earlier.Count || len(later.Buckets) != len(earlier.Buckets) {
			unavailable = append(unavailable, key)
			continue
		}
		delta := metricsHistogram{
			BoundsMS: later.BoundsMS,
			Buckets:  make([]uint64, len(later.Buckets)),
			Count:    later.Count - earlier.Count,
		}
		monotone := later.SumMS >= earlier.SumMS
		for i, value := range later.Buckets {
			if value < earlier.Buckets[i] {
				monotone = false
			}
			delta.Buckets[i] = value - earlier.Buckets[i]
		}
		if !monotone {
			unavailable = append(unavailable, key)
			continue
		}
		delta.SumMS = later.SumMS - earlier.SumMS
		if delta.Count >= observe.MinQuantileSamples {
			p50, ok := observe.HistogramData{BoundsMS: delta.BoundsMS, Buckets: delta.Buckets, Count: delta.Count, SumMS: delta.SumMS}.Quantile(0.5)
			if ok {
				delta.P50 = &p50
			}
			p95, ok := observe.HistogramData{BoundsMS: delta.BoundsMS, Buckets: delta.Buckets, Count: delta.Count, SumMS: delta.SumMS}.Quantile(0.95)
			if ok {
				delta.P95 = &p95
			}
		}
		out[key] = delta
	}
	sortStrings(unavailable)
	return out, unavailable
}

func readHistorySamples(path string) ([]observe.Snapshot, int) {
	file, err := os.Open(path)
	if err != nil {
		return nil, 0
	}
	defer file.Close()
	scanner := bufio.NewScanner(io.LimitReader(file, observe.MaxHistoryFileBytes+observe.MaxSnapshotFileBytes+1))
	scanner.Buffer(make([]byte, 0, 64*1024), int(observe.MaxSnapshotFileBytes)+1024)
	var samples []observe.Snapshot
	corrupt := 0
	for scanner.Scan() {
		var sample observe.Snapshot
		if err := json.Unmarshal(scanner.Bytes(), &sample); err != nil || sample.Validate() != nil {
			corrupt++
			continue
		}
		samples = append(samples, sample)
	}
	return samples, corrupt
}

func writeMetricsText(w io.Writer, data metricsData) {
	s := styleFor(w)
	sep := " " + s.bullet() + " "
	fmt.Fprintf(w, "%s  %s\n", s.bold("ohya metrics"), s.dim(fmt.Sprintf("window %s%srequested %s%s%s",
		data.Window, sep, ageText(data.RequestedMS), sep, data.GeneratedUTC)))
	for _, instance := range data.Instances {
		if instance.DecodeError != "" {
			fmt.Fprintf(w, "\n%s %s: %s\n", s.red("unreadable"), s.bold(shortID(instance.InstanceID)), instance.DecodeError)
			continue
		}
		role := strings.Join(instance.Roles, ",")
		if role == "" {
			role = "unknown"
		}
		var meta []string
		meta = append(meta, fmt.Sprintf("%d samples", instance.SampleCount))
		if instance.ObservedMS != nil {
			meta = append(meta, "observed "+ageText(*instance.ObservedMS))
		}
		if instance.MonotonicElapsedMS != nil {
			meta = append(meta, "monotonic "+ageText(*instance.MonotonicElapsedMS))
		}
		if instance.Gaps != nil && *instance.Gaps > 0 {
			meta = append(meta, s.yellow(fmt.Sprintf("%d gaps", *instance.Gaps)))
		}
		header := fmt.Sprintf("%s %s %s  %s", s.cyan(role), s.bold(shortID(instance.InstanceID)), s.dim(fmt.Sprintf("pid %d", instance.PID)), strings.Join(meta, sep))
		if !instance.Completeness.CoversWindow {
			header += " " + s.yellow("("+instance.Completeness.Reason+")")
		}
		fmt.Fprintf(w, "\n%s\n", header)

		tbl := cliTable{
			indent:     "  ",
			headers:    []string{"METRIC", "COUNT", "P50", "P95", "SUM"},
			rightAlign: []bool{false, true, true, true, true},
		}
		for _, key := range sortedKeys(instance.Durations) {
			delta := instance.Durations[key]
			if delta.Count == 0 {
				continue
			}
			p50, p95 := s.dim("-"), s.dim("-")
			if delta.P50 != nil {
				p50 = fmt.Sprintf("%.1fms", *delta.P50)
			}
			if delta.P95 != nil {
				p95 = fmt.Sprintf("%.1fms", *delta.P95)
			}
			tbl.addRow(key, fmt.Sprintf("%d", delta.Count), p50, p95, fmt.Sprintf("%.1fms", delta.SumMS))
		}
		if len(tbl.rows) > 0 {
			tbl.render(w, s)
		}
		if len(instance.UnavailableCounters) > 0 {
			fmt.Fprintf(w, "  %s\n", s.dim("unavailable counters: "+strings.Join(instance.UnavailableCounters, ", ")))
		}
	}
	if data.Coverage.TruncatedDiscovery {
		fmt.Fprintf(w, "\n%s\n", s.yellow("coverage: discovery truncated at the live-instance bound"))
	}
	writeStatusFindingsText(w, s, data.Findings)
}
