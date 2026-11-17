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
	"testing"
	"time"

	"github.com/nibzard/ohya/internal/observe"
)

type statusEnvelope struct {
	SchemaVersion int        `json:"schema_version"`
	Command       string     `json:"command"`
	Data          statusData `json:"data"`
	Error         *jsonError `json:"error"`
}

func runStatusForTest(t *testing.T, args ...string) (statusEnvelope, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := runStatusCommand(args, &stdout, &stderr)
	var envelope statusEnvelope
	if jsonErr := json.Unmarshal(stdout.Bytes(), &envelope); jsonErr != nil && stdout.Len() > 0 {
		t.Fatalf("status output must decode as one JSON envelope: %v: %s", jsonErr, stdout.String())
	}
	return envelope, stdout.String(), err
}

func writeStatusFixture(t *testing.T, stateDir, id string, pid int, sampleAge time.Duration, mutate func(*observe.Snapshot)) string {
	t.Helper()
	instanceDir := filepath.Join(stateDir, "observe", id)
	if err := os.MkdirAll(instanceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	snapshot := observe.Snapshot{
		FormatVersion: observe.SnapshotFormatVersion,
		Instance: observe.Instance{
			InstanceID:    id,
			PID:           pid,
			Roles:         []string{"ingest"},
			StartWallMS:   now.Add(-time.Minute).UnixMilli(),
			BuildRevision: "d63898e",
			Scope:         stateDir,
		},
		UptimeMS:      int64(time.Minute / time.Millisecond),
		SampledWallMS: now.Add(-sampleAge).UnixMilli(),
		Counters:      map[string]uint64{"ingest_events{event_result=inserted}": 5},
		Gauges: map[string]float64{
			"watch_sources{state=active}":  3,
			"watch_sources{state=stopped}": 0,
			"watch_pending_complete_bytes": 0,
		},
		Durations: map[string]observe.HistogramData{},
		Resources: map[string]observe.Reading{
			observe.ResourceGoroutines: observe.Supported(0),
			observe.ResourceRSSBytes:   observe.Unsupported("resident set size is unavailable on this platform"),
		},
	}
	if mutate != nil {
		mutate(&snapshot)
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instanceDir, "snapshot.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return instanceDir
}

func statusArgs(stateDir, dbPath string, extra ...string) []string {
	return append([]string{"--state", stateDir, "--db", dbPath, "--json"}, extra...)
}

func TestStatusHealthyInstanceExitsZero(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "hub.db")
	if err := os.WriteFile(dbPath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeStatusFixture(t, dir, strings.Repeat("1", 32), os.Getpid(), time.Second, nil)
	envelope, raw, err := runStatusForTest(t, statusArgs(dir, dbPath)...)
	if err != nil {
		t.Fatalf("healthy status must exit zero: %v (%s)", err, raw)
	}
	if envelope.SchemaVersion != jsonSchemaVersion || envelope.Command != statusSpec.name || envelope.Error != nil {
		t.Fatalf("envelope wrong: %+v", envelope)
	}
	if envelope.Data.FormatVersion != statusFormatVersion {
		t.Errorf("data.format_version = %d", envelope.Data.FormatVersion)
	}
	if len(envelope.Data.Instances) != 1 {
		t.Fatalf("instances = %d, want 1", len(envelope.Data.Instances))
	}
	instance := envelope.Data.Instances[0]
	if !instance.Fresh || instance.Process != "running" {
		t.Errorf("instance must be fresh and running: %+v", instance)
	}
	if instance.SampleAgeMS == nil || *instance.SampleAgeMS > int64(observe.SnapshotStaleAge) {
		t.Errorf("sample age must be reported and fresh: %+v", instance.SampleAgeMS)
	}
	if instance.Sources.Active == nil || *instance.Sources.Active != 3 {
		t.Errorf("source summary must come from gauges: %+v", instance.Sources)
	}
	if instance.Counters["ingest_events{event_result=inserted}"] != 5 {
		t.Errorf("counters must carry through: %+v", instance.Counters)
	}
	if envelope.Data.Storage.Location != "local" || envelope.Data.Storage.Files["db"] != 1 {
		t.Errorf("storage must report the local database size: %+v", envelope.Data.Storage)
	}
	if !hasFindingCode(envelope.Data.Findings, "observation_current", sevOK) {
		t.Errorf("a healthy report carries the observation_current finding: %+v", envelope.Data.Findings)
	}
}

func TestStatusStaleKilledInstanceExitsThree(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "hub.db")
	cmd := exec.Command("sleep", "0.02")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn a child process: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	writeStatusFixture(t, dir, strings.Repeat("2", 32), pid, time.Minute, nil)
	envelope, raw, err := runStatusForTest(t, statusArgs(dir, dbPath)...)
	if err != errDegraded {
		t.Fatalf("a stale killed instance must exit three: %v (%s)", err, raw)
	}
	if envelope.Error != nil {
		t.Errorf("degraded output stays parseable with no command error: %+v", envelope.Error)
	}
	if !hasFindingCode(envelope.Data.Findings, "instance_terminated", sevWarn) {
		t.Errorf("findings must report the terminated instance: %+v", envelope.Data.Findings)
	}
	if envelope.Data.Instances[0].Fresh {
		t.Error("a minute-old snapshot must be stale")
	}
}

func TestStatusStaleRunningPIDReportsUnknown(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "hub.db")
	writeStatusFixture(t, dir, strings.Repeat("3", 32), os.Getpid(), time.Minute, nil)
	envelope, raw, err := runStatusForTest(t, statusArgs(dir, dbPath)...)
	if err != errDegraded {
		t.Fatalf("a stale snapshot must degrade: %v (%s)", err, raw)
	}
	instance := envelope.Data.Instances[0]
	if instance.Process != "unknown" {
		t.Errorf("process = %s, want unknown", instance.Process)
	}
	if !strings.Contains(instance.ProcessDetail, "pid reuse") {
		t.Errorf("process detail must name pid reuse: %s", instance.ProcessDetail)
	}
}

func TestStatusCorruptAndUnsupportedSnapshots(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "hub.db")
	writeStatusFixture(t, dir, strings.Repeat("4", 32), os.Getpid(), time.Second, func(s *observe.Snapshot) {
		s.FormatVersion = 99
	})
	corruptDir := filepath.Join(dir, "observe", strings.Repeat("5", 32))
	if err := os.MkdirAll(corruptDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corruptDir, "snapshot.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	envelope, raw, err := runStatusForTest(t, statusArgs(dir, dbPath)...)
	if err != errDegraded {
		t.Fatalf("broken snapshots must degrade: %v (%s)", err, raw)
	}
	if !hasFindingCode(envelope.Data.Findings, "instance_unsupported_version", sevError) {
		t.Errorf("an unsupported snapshot version is its own finding: %+v", envelope.Data.Findings)
	}
	if !hasFindingCode(envelope.Data.Findings, "instance_unreadable", sevWarn) {
		t.Errorf("corrupt JSON is its own finding: %+v", envelope.Data.Findings)
	}
	codes := map[string]bool{}
	for _, instance := range envelope.Data.Instances {
		codes[instance.DecodeError] = true
	}
	if len(codes) != 2 {
		t.Errorf("both broken instances must appear with their decode errors: %v", envelope.Data.Instances)
	}
}

func TestStatusDuplicateRoleListsInstancesSeparately(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "hub.db")
	writeStatusFixture(t, dir, strings.Repeat("6", 32), os.Getpid(), time.Second, func(s *observe.Snapshot) {
		s.Counters["ingest_events{event_result=inserted}"] = 1
	})
	writeStatusFixture(t, dir, strings.Repeat("7", 32), os.Getpid(), 2*time.Second, func(s *observe.Snapshot) {
		s.Counters["ingest_events{event_result=inserted}"] = 100
	})
	envelope, raw, err := runStatusForTest(t, statusArgs(dir, dbPath)...)
	if err != errDegraded {
		t.Fatalf("two live instances of one role must degrade: %v (%s)", err, raw)
	}
	if !hasFindingCode(envelope.Data.Findings, "multiple_instances", sevWarn) {
		t.Errorf("duplicate role must be called out: %+v", envelope.Data.Findings)
	}
	if len(envelope.Data.Instances) != 2 {
		t.Fatalf("instances = %d, want 2", len(envelope.Data.Instances))
	}
	counters := map[uint64]bool{}
	for _, instance := range envelope.Data.Instances {
		counters[instance.Counters["ingest_events{event_result=inserted}"]] = true
	}
	if len(counters) != 2 {
		t.Error("instance counters must never be merged")
	}
}

func TestStatusNoInstancesMentionsDisabledCollection(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "hub.db")
	envelope, raw, err := runStatusForTest(t, statusArgs(dir, dbPath)...)
	if err != errDegraded {
		t.Fatalf("no instances must degrade: %v (%s)", err, raw)
	}
	finding := findFinding(envelope.Data.Findings, "no_instances")
	if finding == nil || !strings.Contains(finding.Hint, "--observe off") {
		t.Fatalf("the no-instances hint must mention disabled observation: %+v", finding)
	}
	if !containsString(envelope.Data.Coverage.Missing, "instances") {
		t.Errorf("coverage must list missing instances: %+v", envelope.Data.Coverage)
	}
}

func TestStatusRemoteReportsCapabilityGap(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "hub.db")
	writeStatusFixture(t, dir, strings.Repeat("8", 32), os.Getpid(), time.Second, nil)
	envelope, raw, err := runStatusForTest(t, statusArgs(dir, dbPath, "--remote")...)
	if err != errDegraded {
		t.Fatalf("the remote capability gap must degrade: %v (%s)", err, raw)
	}
	if !hasFindingCode(envelope.Data.Findings, "remote_status_unsupported", sevWarn) {
		t.Errorf("remote gap must be explicit: %+v", envelope.Data.Findings)
	}
	if !containsString(envelope.Data.Coverage.Unsupported, "remote_hub_aggregates") {
		t.Errorf("coverage must name the unsupported probe: %+v", envelope.Data.Coverage)
	}
}

func TestStatusRemoteTopologyResolvesJoinState(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "setup"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "setup", "connection.json"), []byte(`{"mode":"remote"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hub.db"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	joinState := filepath.Join(dir, "join")
	writeStatusFixture(t, joinState, strings.Repeat("9", 32), os.Getpid(), time.Second, func(s *observe.Snapshot) {
		s.Instance.Roles = []string{"join"}
	})
	data, err := buildStatus(context.Background(), statusOptions{
		dbPath:     filepath.Join(dir, "hub.db"),
		stateDir:   joinState,
		setupState: dir,
		home:       dir,
		nowMS:      func() int64 { return time.Now().UnixMilli() },
		probePID:   func(int) string { return "running" },
		loadedRun:  func(context.Context, []string) error { return nil },
		checkRun:   func(context.Context, []string) error { return nil },
	})
	if err != nil {
		t.Fatalf("buildStatus: %v", err)
	}
	if data.Topology.Mode != "remote" {
		t.Errorf("mode = %s, want remote", data.Topology.Mode)
	}
	if data.Storage.Location != "remote-hub" {
		t.Errorf("a joined client must not report its old local database as the hub: %+v", data.Storage)
	}
	if data.Instances[0].Roles[0] != "join" {
		t.Errorf("the join instance must be the primary report: %+v", data.Instances)
	}
	if !containsString(data.Coverage.Notes, "joined device: hub-side aggregates are outside this build's local visibility") {
		t.Errorf("coverage must state the remote visibility limit: %+v", data.Coverage)
	}
}

func TestStatusCommandResolvesRemoteJoinState(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "setup"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "setup", "connection.json"), []byte(`{"mode":"remote"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	joinState := filepath.Join(dir, "join")
	writeStatusFixture(t, joinState, strings.Repeat("e", 32), os.Getpid(), time.Second, func(s *observe.Snapshot) {
		s.Instance.Roles = []string{"join"}
	})
	var stdout bytes.Buffer
	err := runStatusCommand([]string{"--db", filepath.Join(dir, "ignored.db"), "--json", "--remote"}, &stdout, &stdout)
	if err != nil && err != errDegraded {
		t.Fatalf("status on a remote-setup state must not fail hard: %v", err)
	}
	var envelope statusEnvelope
	if jsonErr := json.Unmarshal(stdout.Bytes(), &envelope); jsonErr != nil {
		t.Fatalf("decode: %v: %s", jsonErr, stdout.String())
	}
	if envelope.Data.Topology.Mode != "remote" {
		t.Errorf("mode = %s, want remote", envelope.Data.Topology.Mode)
	}
	if hasFindingCode(envelope.Data.Findings, "no_instances", sevWarn) {
		t.Errorf("the join state must be discovered: %+v", envelope.Data.Findings)
	}
}

func TestStatusGoldensSeparateUnavailableFromZero(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "hub.db")
	writeStatusFixture(t, dir, strings.Repeat("a", 32), os.Getpid(), time.Second, nil)
	_, raw, err := runStatusForTest(t, statusArgs(dir, dbPath)...)
	if err != nil {
		t.Fatalf("healthy status must exit zero: %v", err)
	}
	if !strings.Contains(raw, `"value":null`) {
		t.Error("an unsupported resource must serialize as a null value")
	}
	if !strings.Contains(raw, `"goroutines":{"value":0`) {
		t.Error("a supported zero must serialize as a present zero")
	}
	if !strings.Contains(raw, `"watch_sources{state=stopped}":0`) {
		t.Error("a zero gauge must serialize as zero, not null")
	}
}

func TestStatusRoleFilterAndTextOutput(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "hub.db")
	writeStatusFixture(t, dir, strings.Repeat("b", 32), os.Getpid(), time.Second, nil)
	writeStatusFixture(t, dir, strings.Repeat("c", 32), os.Getpid(), time.Second, func(s *observe.Snapshot) {
		s.Instance.Roles = []string{"mcp"}
	})
	var stdout bytes.Buffer
	err := runStatusCommand([]string{"--state", dir, "--db", dbPath, "--role", "mcp"}, &stdout, &stdout)
	if err != nil {
		t.Fatalf("role filter must not degrade a fresh report: %v", err)
	}
	text := stdout.String()
	if !strings.Contains(text, "roles mcp") || strings.Contains(text, "roles ingest") {
		t.Errorf("text output must respect the role filter: %s", text)
	}
	if !strings.Contains(text, "observation_current") {
		t.Errorf("text output must print findings: %s", text)
	}
}

func TestStatusRejectsBadFlags(t *testing.T) {
	var stdout bytes.Buffer
	if err := runStatusCommand([]string{"--role", "scheduler", "--state", t.TempDir()}, &stdout, &stdout); err == nil {
		t.Error("an unknown role must be a usage error")
	}
	if err := runStatusCommand([]string{"--instance", "nope", "--state", t.TempDir()}, &stdout, &stdout); err == nil {
		t.Error("an invalid instance id must be a usage error")
	}
	if err := runStatusCommand([]string{"extra", "--state", t.TempDir()}, &stdout, &stdout); err == nil {
		t.Error("unexpected arguments must be a usage error")
	}
}

func hasFindingCode(findings []statusFinding, code, severity string) bool {
	for _, f := range findings {
		if f.Code == code && f.Severity == severity {
			return true
		}
	}
	return false
}

func findFinding(findings []statusFinding, code string) *statusFinding {
	for i := range findings {
		if findings[i].Code == code {
			return &findings[i]
		}
	}
	return nil
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func TestStatusBacklogFindingCarriesBytes(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "hub.db")
	writeStatusFixture(t, dir, strings.Repeat("d", 32), os.Getpid(), time.Second, func(s *observe.Snapshot) {
		s.Gauges["watch_pending_complete_bytes"] = 4096
		s.Gauges["watch_partial_tail_bytes"] = 128
		s.Gauges["upload_backlog_bytes{agent=codex}"] = 1048576
	})
	envelope, raw, err := runStatusForTest(t, statusArgs(dir, dbPath)...)
	if err != errDegraded {
		t.Fatalf("a pending backlog must degrade: %v (%s)", err, raw)
	}
	pending := findFinding(envelope.Data.Findings, "pending_backlog")
	if pending == nil || !strings.Contains(pending.Evidence, "4.0 KiB") {
		t.Errorf("the backlog finding must show bytes: %+v", pending)
	}
	upload := findFinding(envelope.Data.Findings, "upload_backlog")
	if upload == nil || !strings.Contains(upload.Evidence, "codex") {
		t.Errorf("per-agent upload backlog must be reported: %+v", upload)
	}
	backlog := envelope.Data.Instances[0].Backlog
	if backlog == nil || backlog.PartialTailBytes == nil || *backlog.PartialTailBytes != 128 {
		t.Errorf("partial tail bytes must be surfaced separately: %+v", backlog)
	}
	if fmt.Sprintf("%.0f", backlog.UploadBytes["codex"]) != "1048576" {
		t.Errorf("upload bytes per agent must be surfaced: %+v", backlog.UploadBytes)
	}
}
