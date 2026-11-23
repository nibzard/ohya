package main

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/nibzard/ohya/internal/claudecode"
	"github.com/nibzard/ohya/internal/mcpserver"
	"github.com/nibzard/ohya/internal/observe"
	"github.com/nibzard/ohya/internal/setup"
)

const (
	diagnoseFormatVersion         = 1
	diagnoseMaxBytes              = 2 << 20
	diagnoseMaxLogEntries         = 100
	diagnoseDefaultIngestInterval = 2 * time.Second
)

var diagnoseSpec = commandSpec{
	name:      "ohya diagnose",
	synopsis:  "export one private, sanitized diagnostics artifact",
	usageLine: "ohya diagnose --output FILE [flags]",
	description: "Writes one JSON artifact with local status, 1m/5m/1h metrics, recent warnings and errors, build identity, and configured limits. " +
		"The artifact never contains transcripts, SQL text, annotation values, credentials, environment dumps, or source paths; entries carry curated codes and counts only. " +
		"It is capped at 2 MiB and lists any section dropped for that cap. " +
		"It refuses to overwrite an existing file unless --force and rejects symlink destinations. " +
		"The file is created 0600 and never uploaded. " +
		"With --remote this build records the unsupported remote fetch as a finding and still exports the local sections. " +
		"Exit codes: 0 for a complete export, 3 for a partial export, 1 for failure, 2 for usage.",
	examples: []string{"ohya diagnose --output ./ohya-diagnostics.json", "ohya diagnose --output ./d.json --force"},
}

type diagnoseLimits struct {
	IngestIntervalMS           int64 `json:"ingest_interval_ms"`
	MaxRecordBytes             int64 `json:"max_record_bytes"`
	QueryDeadlineMS            int64 `json:"query_deadline_ms"`
	QueryMaxRows               int   `json:"query_max_rows"`
	QueryMaxOutputBytes        int   `json:"query_max_output_bytes"`
	QueryMaxQueryBytes         int   `json:"query_max_query_bytes"`
	QueryMaxConcurrency        int   `json:"query_max_concurrency"`
	AnnotateMaxRequestBytes    int64 `json:"annotate_max_request_bytes"`
	SnapshotMaxBytes           int64 `json:"snapshot_max_bytes"`
	HistoryMaxBytes            int64 `json:"history_max_bytes"`
	LogFileMaxBytes            int64 `json:"log_file_max_bytes"`
	LogRotatedFiles            int   `json:"log_rotated_files"`
	LogQueueDepth              int   `json:"log_queue_depth"`
	DiagnosticArtifactMaxBytes int64 `json:"diagnostic_artifact_max_bytes"`
	DiagnosticMaxLogEntries    int   `json:"diagnostic_max_log_entries"`
}

type diagnoseCapabilities struct {
	LocalStatus      bool `json:"local_status"`
	LocalMetrics     bool `json:"local_metrics"`
	LocalLogs        bool `json:"local_logs"`
	RemoteAggregates bool `json:"remote_aggregates"`
}

type diagnoseData struct {
	FormatVersion   int                     `json:"format_version"`
	GeneratedWallMS int64                   `json:"generated_wall_ms"`
	GeneratedUTC    string                  `json:"generated_utc"`
	Build           statusIdentity          `json:"build"`
	Limits          diagnoseLimits          `json:"limits"`
	Capabilities    diagnoseCapabilities    `json:"capabilities"`
	Status          *statusData             `json:"status,omitempty"`
	Metrics         map[string]*metricsData `json:"metrics,omitempty"`
	Logs            []observe.LogEntry      `json:"logs,omitempty"`
	Omitted         []string                `json:"omitted,omitempty"`
	Dropped         []string                `json:"dropped_for_size,omitempty"`
	Findings        []statusFinding         `json:"findings"`
}

type diagnoseOptions struct {
	stateDir   string
	setupState string
	dbPath     string
	home       string
	remote     bool
	nowMS      func() int64
}

func runDiagnoseCommand(args []string, stdout, stderr io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	c := newCommandFlags(diagnoseSpec)
	db := c.fs.String("db", filepath.Join(home, ".ohya", "hub.db"),
		"hub database `path` used to resolve the default state directory")
	state := c.fs.String("state", "",
		"state `directory` whose observe/ tree holds snapshots, history, and logs; defaults to the database directory")
	remote := c.fs.Bool("remote", false,
		"also export hub-side aggregates; this build records the capability gap and exports the local sections")
	output := c.fs.String("output", "",
		"`path` of the JSON artifact to write (required)")
	force := c.fs.Bool("force", false,
		"overwrite an existing output file")
	if err := c.parse(args, stdout); err != nil {
		return err
	}
	if c.fs.NArg() > 0 {
		return c.usageErrorf("unexpected argument %q", c.fs.Arg(0))
	}
	if *output == "" {
		return c.usageErrorf("--output is required")
	}
	stateDir := *state
	setupState := stateDir
	if stateDir == "" {
		setupState = filepath.Dir(*db)
	}
	connection, err := setup.LoadConnection(setupState)
	if err != nil {
		return err
	}
	if stateDir == "" && connection.Mode == "remote" {
		stateDir = filepath.Join(setupState, "join")
	}
	if stateDir == "" {
		stateDir = setupState
	}
	o := diagnoseOptions{
		stateDir:   stateDir,
		setupState: setupState,
		dbPath:     *db,
		home:       home,
		remote:     *remote,
		nowMS:      func() int64 { return time.Now().UnixMilli() },
	}
	data := buildDiagnose(context.Background(), o, diagnoseMaxBytes)
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if err := writeDiagnoseArtifact(*output, raw, *force); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "ohya: diagnostics written to %s (%d bytes)\n", *output, len(raw))
	if diagnosePartial(data) {
		return errDegraded
	}
	return nil
}

func diagnosePartial(data diagnoseData) bool {
	for _, f := range data.Findings {
		if f.Severity == sevWarn || f.Severity == sevError {
			return true
		}
	}
	return false
}

func buildDiagnose(ctx context.Context, o diagnoseOptions, maxBytes int) diagnoseData {
	nowMS := o.nowMS()
	data := diagnoseData{
		FormatVersion:   diagnoseFormatVersion,
		GeneratedWallMS: nowMS,
		GeneratedUTC:    time.UnixMilli(nowMS).UTC().Format(time.RFC3339),
		Build:           statusCLIIdentity(),
		Limits: diagnoseLimits{
			IngestIntervalMS:           diagnoseDefaultIngestInterval.Milliseconds(),
			MaxRecordBytes:             claudecode.DefaultMaxRecordBytes,
			QueryDeadlineMS:            mcpserver.DefaultDeadline.Milliseconds(),
			QueryMaxRows:               mcpserver.DefaultMaxRows,
			QueryMaxOutputBytes:        mcpserver.DefaultMaxOutputBytes,
			QueryMaxQueryBytes:         mcpserver.DefaultMaxQueryBytes,
			QueryMaxConcurrency:        mcpserver.DefaultMaxConcurrency,
			AnnotateMaxRequestBytes:    mcpserver.DefaultMaxAnnotateRequestBytes,
			SnapshotMaxBytes:           observe.MaxSnapshotFileBytes,
			HistoryMaxBytes:            observe.MaxHistoryFileBytes,
			LogFileMaxBytes:            observe.LogMaxFileBytes,
			LogRotatedFiles:            observe.LogRotatedFiles,
			LogQueueDepth:              observe.LogQueueDepth,
			DiagnosticArtifactMaxBytes: diagnoseMaxBytes,
			DiagnosticMaxLogEntries:    diagnoseMaxLogEntries,
		},
		Capabilities: diagnoseCapabilities{LocalStatus: true, LocalMetrics: true, LocalLogs: true},
		Findings:     []statusFinding{},
	}
	if statusData, err := buildStatus(ctx, statusOptions{
		dbPath:     o.dbPath,
		stateDir:   o.stateDir,
		setupState: o.setupState,
		home:       o.home,
		nowMS:      o.nowMS,
		probePID:   observe.ProbePID,
		loadedRun:  setup.RunServiceLoaded,
		checkRun:   setup.RunServiceCommand,
	}); err != nil {
		data.Findings = append(data.Findings, statusFinding{
			Code:     "status_unavailable",
			Severity: sevWarn,
			Evidence: "the local status section could not be assembled",
			Hint:     "run ohya status for the concrete failure; the artifact keeps the remaining sections",
		})
	} else {
		data.Status = &statusData
	}
	data.Metrics = map[string]*metricsData{}
	for _, window := range []string{"1m", "5m", "1h"} {
		requestedMS, _ := parseMetricsWindow(window)
		metricsData, err := buildMetrics(metricsOptions{
			stateDir: o.stateDir,
			window:   window,
			nowMS:    o.nowMS,
		}, requestedMS)
		if err != nil {
			continue
		}
		data.Metrics[window] = &metricsData
	}
	if len(data.Metrics) == 0 {
		data.Omitted = append(data.Omitted, "metrics")
	}
	data.Logs = recentWarningEntries(o.stateDir, diagnoseMaxLogEntries)
	if len(data.Logs) == 0 {
		data.Omitted = append(data.Omitted, "logs")
	}
	if o.remote {
		data.Capabilities.RemoteAggregates = false
		data.Findings = append(data.Findings, statusFinding{
			Code:     "remote_diagnostics_unsupported",
			Severity: sevWarn,
			Evidence: "this build cannot fetch hub-side aggregates over the sync connection yet",
			Hint:     "remote aggregate delivery is plan 001 milestone 5; run ohya diagnose on the hub host until then",
		})
	}
	data.Dropped = dropOversize(&data, maxBytes)
	return data
}

func dropOversize(data *diagnoseData, maxBytes int) []string {
	var dropped []string
	trims := []struct {
		name string
		cut  func(d *diagnoseData)
	}{
		{"metrics.1h", func(d *diagnoseData) { delete(d.Metrics, "1h") }},
		{"metrics.5m", func(d *diagnoseData) { delete(d.Metrics, "5m") }},
		{"metrics.1m", func(d *diagnoseData) { d.Metrics = nil }},
		{"logs", func(d *diagnoseData) { d.Logs = nil }},
		{"status.instance_details", func(d *diagnoseData) {
			if d.Status != nil {
				d.Status.Instances = nil
			}
		}},
	}
	for _, trim := range trims {
		raw, err := json.Marshal(data)
		if err != nil || len(raw) <= maxBytes {
			break
		}
		trim.cut(data)
		dropped = append(dropped, trim.name)
	}
	return dropped
}

func recentWarningEntries(stateDir string, limit int) []observe.LogEntry {
	views, _, err := observe.DiscoverInstances(filepath.Join(stateDir, "observe"))
	if err != nil {
		return nil
	}
	var all []observe.LogEntry
	for _, view := range views {
		entries, _, _, _ := readInstanceLogs(view.Dir)
		for _, entry := range entries {
			if entry.Level == observe.LevelWarn || entry.Level == observe.LevelError {
				all = append(all, entry)
			}
		}
	}
	if len(all) == 0 {
		return nil
	}
	slices.SortStableFunc(all, func(a, b observe.LogEntry) int {
		return cmp.Compare(a.TimeMS, b.TimeMS)
	})
	if len(all) > limit {
		all = all[len(all)-limit:]
	}
	return all
}

func writeDiagnoseArtifact(path string, raw []byte, force bool) error {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("output %s is a symlink; choose a regular file path", path)
		}
		if !force {
			return fmt.Errorf("output %s already exists; pass --force to overwrite", path)
		}
	}
	dir := filepath.Dir(path)
	if _, err := os.Lstat(dir); err != nil {
		return fmt.Errorf("output directory %s does not exist: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".ohya-diagnose-*")
	if err != nil {
		return fmt.Errorf("create temporary artifact: %w", err)
	}
	name := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return fmt.Errorf("write artifact: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return fmt.Errorf("restrict artifact: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("close artifact: %w", err)
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return fmt.Errorf("publish artifact: %w", err)
	}
	return nil
}
