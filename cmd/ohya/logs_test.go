package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nibzard/ohya/internal/observe"
)

type logsEnvelope struct {
	SchemaVersion int        `json:"schema_version"`
	Command       string     `json:"command"`
	Data          logsData   `json:"data"`
	Error         *jsonError `json:"error"`
}

func seedLogInstance(t *testing.T, stateDir, id, role string, startMS int64, entries []observe.LogEntry) string {
	t.Helper()
	instanceDir := filepath.Join(stateDir, "observe", id)
	if err := os.MkdirAll(instanceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	snapshot := observe.Snapshot{
		FormatVersion: observe.SnapshotFormatVersion,
		Instance: observe.Instance{
			InstanceID:  id,
			PID:         os.Getpid(),
			Roles:       []string{role},
			StartWallMS: startMS,
		},
		UptimeMS:      1000,
		SampledWallMS: time.Now().UnixMilli(),
		Counters:      map[string]uint64{},
		Gauges:        map[string]float64{},
		Durations:     map[string]observe.HistogramData{},
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instanceDir, "snapshot.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	AppendLogEntries(t, instanceDir, "events.jsonl", entries)
	return instanceDir
}

func AppendLogEntries(t *testing.T, instanceDir, name string, entries []observe.LogEntry) {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(instanceDir, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	for _, entry := range entries {
		line, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(append(line, '\n')); err != nil {
			t.Fatal(err)
		}
	}
}

func logEntryAt(id, component string, level observe.Level, code observe.EventCode, at time.Time) observe.LogEntry {
	return observe.LogEntry{
		FormatVersion: observe.LogFormatVersion,
		TimeMS:        at.UnixMilli(),
		InstanceID:    id,
		Component:     component,
		Level:         level,
		EventCode:     string(code),
	}
}

func runLogsForTest(t *testing.T, args ...string) (logsEnvelope, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := runLogsCommand(args, &stdout, &stderr)
	var envelope logsEnvelope
	if jsonErr := json.Unmarshal(stdout.Bytes(), &envelope); jsonErr != nil && stdout.Len() > 0 {
		t.Fatalf("logs output must decode as one JSON envelope: %v: %s", jsonErr, stdout.String())
	}
	return envelope, stdout.String(), err
}

func TestLogsFiniteReportFiltersLevelsComponentsAndLimit(t *testing.T) {
	stateDir := t.TempDir()
	id := strings.Repeat("a", 32)
	now := time.Now()
	entries := []observe.LogEntry{
		logEntryAt(id, "ingest", observe.LevelInfo, observe.EventStartup, now.Add(-9*time.Minute)),
		logEntryAt(id, "ingest", observe.LevelWarn, observe.EventCheckpointFailed, now.Add(-5*time.Minute)),
		logEntryAt(id, "mcp", observe.LevelWarn, observe.EventQueryBusy, now.Add(-3*time.Minute)),
		logEntryAt(id, "observe", observe.LevelInfo, observe.EventHeartbeat, now.Add(-30*time.Minute)),
	}
	entries[0].Counts = map[string]int64{"roles": 1}
	seedLogInstance(t, stateDir, id, "ingest", now.UnixMilli(), entries)

	envelope, out, err := runLogsForTest(t, "--state", stateDir, "--since", "10m", "--json")
	if err != nil {
		t.Fatalf("healthy logs report failed: %v (%s)", err, out)
	}
	if len(envelope.Data.Entries) != 3 {
		t.Fatalf("entries = %d, want 3 inside the window: %s", len(envelope.Data.Entries), out)
	}

	envelope, _, err = runLogsForTest(t, "--state", stateDir, "--since", "1h", "--level", "warn", "--json")
	if err != nil {
		t.Fatalf("warn-filtered report failed: %v", err)
	}
	if len(envelope.Data.Entries) != 2 {
		t.Fatalf("warn entries = %d, want 2", len(envelope.Data.Entries))
	}
	for _, entry := range envelope.Data.Entries {
		if entry.Level != observe.LevelWarn {
			t.Fatalf("non-warn entry leaked through --level warn: %+v", entry)
		}
	}

	envelope, _, err = runLogsForTest(t, "--state", stateDir, "--since", "1h", "--component", "mcp", "--json")
	if err != nil {
		t.Fatalf("component-filtered report failed: %v", err)
	}
	if len(envelope.Data.Entries) != 1 || envelope.Data.Entries[0].EventCode != string(observe.EventQueryBusy) {
		t.Fatalf("component entries = %+v", envelope.Data.Entries)
	}

	envelope, _, err = runLogsForTest(t, "--state", stateDir, "--since", "1h", "--limit", "1", "--json")
	if err != nil {
		t.Fatalf("limited report failed: %v", err)
	}
	if len(envelope.Data.Entries) != 1 || envelope.Data.Entries[0].EventCode != string(observe.EventQueryBusy) {
		t.Fatalf("limited entries = %+v", envelope.Data.Entries)
	}
	if !envelope.Data.Truncated || !strings.Contains(envelope.Data.TruncationReason, "limit") {
		t.Fatalf("truncation = %t %q", envelope.Data.Truncated, envelope.Data.TruncationReason)
	}
}

func TestLogsSkipsCorruptAndOversizeLinesWithoutLeakingThem(t *testing.T) {
	stateDir := t.TempDir()
	id := strings.Repeat("b", 32)
	now := time.Now()
	instanceDir := seedLogInstance(t, stateDir, id, "ingest", now.UnixMilli(), []observe.LogEntry{
		logEntryAt(id, "ingest", observe.LevelInfo, observe.EventStartup, now.Add(-time.Minute)),
	})
	file, err := os.OpenFile(filepath.Join(instanceDir, "events.jsonl"), os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := []byte(`{"format_version":1,"note":"secret-sk-12345 in a torn line` + "\n")
	oversize := []byte(`{"pad":"` + strings.Repeat("x", 9<<10) + `"}` + "\n")
	if _, err := file.Write(append(append(corrupt, oversize...), '\n')); err != nil {
		t.Fatal(err)
	}
	file.Close()

	envelope, out, err := runLogsForTest(t, "--state", stateDir, "--since", "1h", "--json")
	if err != nil && err != errDegraded {
		t.Fatalf("report with corrupt lines failed: %v", err)
	}
	if len(envelope.Data.Entries) != 1 {
		t.Fatalf("entries = %d, want the single valid one: %s", len(envelope.Data.Entries), out)
	}
	if envelope.Data.Instances[0].SkippedLines != 2 {
		t.Fatalf("skipped = %d, want 2", envelope.Data.Instances[0].SkippedLines)
	}
	if strings.Contains(out, "secret-sk-12345") {
		t.Fatal("a corrupt log line leaked into the report")
	}
	var degraded bool
	for _, finding := range envelope.Data.Findings {
		if finding.Code == "log_lines_skipped" {
			degraded = true
		}
	}
	if !degraded {
		t.Fatalf("skipped lines produced no finding: %+v", envelope.Data.Findings)
	}
}

func TestLogsNoInstancesIsDegradedAndRoleFiltersSelectInstances(t *testing.T) {
	stateDir := t.TempDir()
	if _, _, err := runLogsForTest(t, "--state", stateDir, "--since", "1h", "--json"); err != errDegraded {
		t.Fatalf("empty state must be degraded, got %v", err)
	}
	ingest := strings.Repeat("c", 32)
	mcp := strings.Repeat("d", 32)
	now := time.Now()
	seedLogInstance(t, stateDir, ingest, "ingest", now.UnixMilli(), []observe.LogEntry{
		logEntryAt(ingest, "ingest", observe.LevelInfo, observe.EventStartup, now.Add(-time.Minute)),
	})
	seedLogInstance(t, stateDir, mcp, "mcp", now.UnixMilli(), []observe.LogEntry{
		logEntryAt(mcp, "mcp", observe.LevelInfo, observe.EventStartup, now.Add(-time.Minute)),
	})
	envelope, _, err := runLogsForTest(t, "--state", stateDir, "--since", "1h", "--role", "mcp", "--json")
	if err != nil {
		t.Fatalf("role-filtered report failed: %v", err)
	}
	if len(envelope.Data.Instances) != 1 || envelope.Data.Instances[0].InstanceID != mcp {
		t.Fatalf("instances = %+v", envelope.Data.Instances)
	}
	envelope, _, err = runLogsForTest(t, "--state", stateDir, "--since", "1h", "--instance", ingest, "--json")
	if err != nil {
		t.Fatalf("instance-filtered report failed: %v", err)
	}
	if len(envelope.Data.Instances) != 1 || envelope.Data.Instances[0].InstanceID != ingest {
		t.Fatalf("instances = %+v", envelope.Data.Instances)
	}
}

func TestLogsPayloadCapDropsOldestEntries(t *testing.T) {
	stateDir := t.TempDir()
	id := strings.Repeat("e", 32)
	now := time.Now()
	var entries []observe.LogEntry
	for i := 0; i < 400; i++ {
		entry := logEntryAt(id, "ingest", observe.LevelInfo, observe.EventSummary, now.Add(-time.Duration(400-i)*time.Second))
		entry.Counts = map[string]int64{}
		for k := 0; k < 80; k++ {
			entry.Counts[fmt.Sprintf("counter_name_number_%02d_with_padding", k)] = int64(i)
		}
		entries = append(entries, entry)
	}
	seedLogInstance(t, stateDir, id, "ingest", now.UnixMilli(), entries)
	envelope, out, err := runLogsForTest(t, "--state", stateDir, "--since", "1h", "--limit", "1000", "--json")
	if err != nil {
		t.Fatalf("capped report failed: %v", err)
	}
	if len(envelope.Data.Entries) >= len(entries) {
		t.Fatalf("payload cap never dropped entries: %d kept of %d", len(envelope.Data.Entries), len(entries))
	}
	if !envelope.Data.Truncated || !strings.Contains(envelope.Data.TruncationReason, "payload bound") {
		t.Fatalf("truncation = %t %q", envelope.Data.Truncated, envelope.Data.TruncationReason)
	}
	if len(out) > logsMaxPayloadBytes+(200<<10) {
		t.Fatalf("finite output grew to %d bytes", len(out))
	}
	newest := envelope.Data.Entries[len(envelope.Data.Entries)-1]
	if newest.TimeMS != entries[len(entries)-1].TimeMS {
		t.Fatal("the cap must keep the newest entries, not the oldest")
	}
}

type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestLogsFollowReplaysWindowThenStreamsAndSurvivesRotation(t *testing.T) {
	stateDir := t.TempDir()
	id := strings.Repeat("f", 32)
	now := time.Now()
	instanceDir := seedLogInstance(t, stateDir, id, "ingest", now.UnixMilli(), []observe.LogEntry{
		logEntryAt(id, "ingest", observe.LevelInfo, observe.EventStartup, now.Add(-time.Minute)),
	})
	var stdout syncBuf
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- followLogs(ctx, observe.InstanceView{InstanceID: id, Dir: instanceDir}, logsOptions{
			since: time.Hour,
			limit: 100,
			nowMS: func() int64 { return time.Now().UnixMilli() },
		}, &stdout)
	}()
	waitFor := func(substr string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if strings.Contains(stdout.String(), substr) {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("follow output never showed %q: %s", substr, stdout.String())
	}
	waitFor(string(observe.EventStartup))

	AppendLogEntries(t, instanceDir, "events.jsonl", []observe.LogEntry{
		logEntryAt(id, "uploader", observe.LevelWarn, observe.EventTransportOffline, time.Now()),
	})
	waitFor(string(observe.EventTransportOffline))

	if err := os.Rename(filepath.Join(instanceDir, "events.jsonl"), filepath.Join(instanceDir, "events.jsonl.1")); err != nil {
		t.Fatal(err)
	}
	fresh := []observe.LogEntry{logEntryAt(id, "ingest", observe.LevelInfo, observe.EventShutdown, time.Now())}
	AppendLogEntries(t, instanceDir, "events.jsonl", fresh)
	waitFor(string(observe.EventShutdown))
	if got := strings.Count(stdout.String(), string(observe.EventStartup)); got != 1 {
		t.Fatalf("rotation re-emitted the old file; startup appeared %d times: %s", got, stdout.String())
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cancelled follow returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled follow did not return")
	}
}

func startIngestChildCapturingStdout(t *testing.T, stateDir string, stdout *bytes.Buffer) *exec.Cmd {
	t.Helper()
	args := []string{
		"child-run", "ingest",
		"--db", filepath.Join(stateDir, "hub.db"),
		"--state", stateDir,
		"--allow-empty",
		"--interval", "50ms",
		"--claude-root", filepath.Join(stateDir, "projects"),
		"--codex-root", "",
		"--pi-root", "",
		"--antigravity-root", "",
	}
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "HOME="+stateDir)
	cmd.Stdout = stdout
	cmd.Stderr = stdout
	if err := cmd.Start(); err != nil {
		t.Fatalf("start ingest child: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	return cmd
}

func TestLogsChildWritesLifecycleEntriesAndKeepsStdoutClean(t *testing.T) {
	stateDir := t.TempDir()
	var childOutput bytes.Buffer
	child := startIngestChildCapturingStdout(t, stateDir, &childOutput)
	instanceDir := waitForInstance(t, stateDir, 5*time.Second)
	if instanceDir == "" {
		t.Fatal("the ingest child must publish an instance directory at startup")
	}
	if !waitForFile(t, filepath.Join(instanceDir, "events.jsonl"), 5*time.Second) {
		t.Fatal("the ingest child never wrote its startup log entry")
	}
	_ = child.Process.Signal(syscall.SIGTERM)
	_ = child.Wait()

	envelope, out, err := runLogsForTest(t, "--state", stateDir, "--since", "5m", "--json")
	if err != nil {
		t.Fatalf("logs after child shutdown failed: %v (%s)", err, out)
	}
	codes := map[string]bool{}
	for _, entry := range envelope.Data.Entries {
		codes[entry.EventCode] = true
	}
	if !codes[string(observe.EventStartup)] {
		t.Fatalf("startup entry missing: %s", out)
	}
	if !codes[string(observe.EventShutdown)] {
		t.Fatalf("shutdown entry missing: %s", out)
	}
	if strings.Contains(childOutput.String(), `"format_version":1,"time_ms"`) {
		t.Fatalf("application log entries polluted child stdout/stderr: %q", childOutput.String())
	}
	info, err := os.Stat(filepath.Join(instanceDir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("events.jsonl mode = %o, want 0600", info.Mode().Perm())
	}
}
