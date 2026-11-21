package observe

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testPublisherConfig(t *testing.T, dir string) PublisherConfig {
	t.Helper()
	clock := NewSystemClock()
	instance, err := NewInstance(dir, "d63898e", false, clock, RoleIngest)
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	recorder, err := NewRecorder(instance, clock)
	if err != nil {
		t.Fatalf("NewRecorder: %v", err)
	}
	return PublisherConfig{Dir: dir, Recorder: recorder, Clock: clock}
}

func readSnapshotFile(t *testing.T, dir string) Snapshot {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, snapshotFileName))
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	var snapshot Snapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("validate snapshot: %v", err)
	}
	return snapshot
}

func TestPublisherWritesStartupPeriodicAndFinalSnapshots(t *testing.T) {
	dir := t.TempDir()
	cfg := testPublisherConfig(t, dir)
	cfg.Interval = 5 * time.Millisecond
	publisher, err := NewPublisher(cfg)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	instanceDir := publisher.Dir()
	first := readSnapshotFile(t, instanceDir)
	if first.Counters["discovery_scans"] != 0 {
		t.Fatalf("startup snapshot must decode: %+v", first)
	}
	if first.Stopped {
		t.Fatal("startup snapshot must not be marked stopped")
	}
	publisher.cfg.Recorder.Add("discovery_scans", 2)
	var second Snapshot
	deadline := time.Now().Add(2 * time.Second)
	for second.Counters["discovery_scans"] != 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
		second = readSnapshotFile(t, instanceDir)
	}
	if second.Counters["discovery_scans"] != 2 {
		t.Fatalf("periodic snapshot must carry new counters: %+v", second.Counters)
	}
	if second.Stopped {
		t.Fatal("periodic snapshot must not be marked stopped")
	}
	publisher.cfg.Recorder.Add("discovery_scans", 3)
	if err := publisher.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := publisher.Close(); err != nil {
		t.Fatalf("second Close must be idempotent: %v", err)
	}
	final := readSnapshotFile(t, instanceDir)
	if final.Counters["discovery_scans"] != 5 {
		t.Fatalf("final snapshot must carry counters added after the last tick: %+v", final.Counters)
	}
	if !final.Stopped {
		t.Fatal("final snapshot written by Close must set stopped=true")
	}
}

func TestPublisherFilePermissionsArePrivate(t *testing.T) {
	dir := t.TempDir()
	cfg := testPublisherConfig(t, dir)
	publisher, err := NewPublisher(cfg)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	defer publisher.Close()
	info, err := os.Stat(publisher.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("instance dir mode = %o, want 0700", info.Mode().Perm())
	}
	snapshot, err := os.Stat(filepath.Join(publisher.Dir(), snapshotFileName))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Mode().Perm() != 0o600 {
		t.Errorf("snapshot mode = %o, want 0600", snapshot.Mode().Perm())
	}
}

func TestPublisherSnapshotBoundTruncatesDetailsAndKeepsCounters(t *testing.T) {
	dir := t.TempDir()
	cfg := testPublisherConfig(t, dir)
	cfg.MaxSnapshotBytes = 480
	cfg.Recorder.Add("ingest_events", 7, "inserted")
	cfg.Recorder.Set("watch_sources", 3, "active")
	cfg.Recorder.Set("watch_sources", 1, "missing")
	cfg.Recorder.Observe("parse_duration", time.Millisecond, "codex")
	cfg.Recorder.Observe("parse_duration", time.Millisecond, "pi")
	cfg.Recorder.Resource(ResourceGoroutines, Supported(9))
	cfg.Recorder.Resource(ResourceHeapAllocBytes, Supported(1<<20))
	publisher, err := NewPublisher(cfg)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	defer publisher.Close()
	raw, err := os.ReadFile(filepath.Join(publisher.Dir(), snapshotFileName))
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(raw)) > cfg.MaxSnapshotBytes {
		t.Fatalf("snapshot of %d bytes exceeds the bound", len(raw))
	}
	var snapshot Snapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	if !snapshot.Truncated {
		t.Error("oversized snapshot must set the truncation flag")
	}
	if snapshot.Counters["ingest_events{event_result=inserted}"] != 7 {
		t.Errorf("counters must survive truncation: %+v", snapshot.Counters)
	}
	if len(snapshot.Durations) != 0 || len(snapshot.Resources) != 0 || len(snapshot.Gauges) != 0 {
		t.Errorf("details must be dropped from a truncated snapshot: %+v", snapshot)
	}
}

func TestPublisherHistoryAppendsPerIntervalAndPrunesByBytes(t *testing.T) {
	dir := t.TempDir()
	cfg := testPublisherConfig(t, dir)
	cfg.Interval = 4 * time.Millisecond
	cfg.HistoryInterval = time.Millisecond
	lineBound := int64(2400)
	cfg.MaxHistoryBytes = lineBound
	publisher, err := NewPublisher(cfg)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	path := filepath.Join(publisher.Dir(), historyFileName)
	var lines []string
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		lines = strings.Split(strings.TrimSpace(string(raw)), "\n")
		if len(lines) >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	publisher.Close()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) < 2 {
		t.Fatalf("history must append one sample per interval, got %d lines", len(lines))
	}
	if int64(len(raw)) > lineBound+1200 {
		t.Fatalf("history of %d bytes exceeds the retention bound", len(raw))
	}
	var last SampledLine
	for _, line := range lines {
		var head SampledLine
		if err := json.Unmarshal([]byte(line), &head); err != nil {
			t.Fatalf("history line must decode: %v", err)
		}
		if head.SampledWallMS < last.SampledWallMS {
			t.Fatalf("history must stay ordered: %v", lines)
		}
		last = head
	}
	var snapshot Snapshot
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &snapshot); err != nil {
		t.Fatalf("newest history line must be a snapshot: %v", err)
	}
}

type SampledLine struct {
	SampledWallMS int64 `json:"sampled_wall_ms"`
}

func TestPublisherRecordsOwnGaugesAndSurvivesWriteFailures(t *testing.T) {
	dir := t.TempDir()
	cfg := testPublisherConfig(t, dir)
	cfg.Interval = 5 * time.Millisecond
	publisher, err := NewPublisher(cfg)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snapshot := cfg.Recorder.Snapshot()
		if _, ok := snapshot.Durations["instrumentation_publish_duration"]; ok {
			if _, ok := snapshot.Gauges["instrumentation_last_publish_ms"]; ok {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	snapshot := cfg.Recorder.Snapshot()
	if _, ok := snapshot.Durations["instrumentation_publish_duration"]; !ok {
		t.Error("publisher must observe its own publication duration")
	}
	if _, ok := snapshot.Gauges["instrumentation_last_publish_ms"]; !ok {
		t.Error("publisher must set the last publication gauge")
	}
	publisher.Close()

	failing := testPublisherConfig(t, dir)
	failing.Interval = 5 * time.Millisecond
	failing.snapshotWriter = func(path string, data []byte) error {
		return os.ErrPermission
	}
	failedPublisher, err := NewPublisher(failing)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	snapshot = failing.Recorder.Snapshot()
	if snapshot.Counters["instrumentation_write_errors{write_target=snapshot}"] == 0 {
		t.Error("failed writes must count in instrumentation_write_errors")
	}
	if err := failedPublisher.Close(); err == nil {
		t.Error("Close must surface the accumulated write failure")
	}
	if failing.Recorder.Add("discovery_scans", 1); failing.Recorder.Snapshot().Counters["discovery_scans"] != 1 {
		t.Error("recorder must keep working after publish failures")
	}
}

func TestWriteInstanceFileCleansTemporaryFilesAfterFailures(t *testing.T) {
	instanceDir := t.TempDir()
	blocked := filepath.Join(instanceDir, snapshotFileName)
	if err := os.Mkdir(blocked, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeInstanceFile(blocked, []byte("{}")); err == nil {
		t.Fatal("replacing a directory must fail")
	}
	entries, err := os.ReadDir(instanceDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".snapshot-") {
			t.Errorf("temporary file %s survived a failed write", entry.Name())
		}
	}
}

func TestCleanStaleInstancesRetentionRules(t *testing.T) {
	dir := t.TempDir()
	observeDir := filepath.Join(dir, "observe")
	if err := os.MkdirAll(observeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	deadPID := exitedChildPID(t)
	writeInstance := func(id string, pid int, wallMS int64) string {
		t.Helper()
		instanceDir := filepath.Join(observeDir, id)
		if err := os.MkdirAll(instanceDir, 0o700); err != nil {
			t.Fatal(err)
		}
		snapshot := Snapshot{
			FormatVersion: SnapshotFormatVersion,
			Instance:      Instance{InstanceID: id, PID: pid, Roles: []string{"ingest"}, StartWallMS: wallMS},
			UptimeMS:      1,
			SampledWallMS: wallMS,
			Counters:      map[string]uint64{},
		}
		raw, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(instanceDir, snapshotFileName), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		return instanceDir
	}
	live := writeInstance(strings.Repeat("a", 32), os.Getpid(), time.Now().UnixMilli())
	dead := make([]string, 10)
	for i := range dead {
		dead[i] = writeInstance(fmt.Sprintf("%032d", i+1), deadPID, time.Now().UnixMilli())
	}
	oldLive := writeInstance(strings.Repeat("b", 32), os.Getpid(), time.Now().UnixMilli())
	oldTime := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(oldLive, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	oldDead := writeInstance(strings.Repeat("c", 32), deadPID, time.Now().UnixMilli())
	if err := os.Chtimes(oldDead, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(observeDir, "stranger")
	if err := os.MkdirAll(foreign, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(observeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := CleanStaleInstances(observeDir, NewSystemClock()); err != nil {
		t.Fatalf("CleanStaleInstances: %v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Error("a live instance must never be deleted")
	}
	if _, err := os.Stat(oldLive); err != nil {
		t.Error("an old but live instance must never be deleted")
	}
	if _, err := os.Stat(oldDead); err == nil {
		t.Error("an instance older than the retention age must be deleted")
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Error("a directory without a valid instance name must be left alone")
	}
	remaining := 0
	for _, path := range dead {
		if _, err := os.Stat(path); err == nil {
			remaining++
		}
	}
	if remaining != RetainedInstances {
		t.Errorf("terminated instances kept = %d, want %d", remaining, RetainedInstances)
	}
}

func TestCleanStaleInstancesKeepsSnapshotlessDirsUntilOld(t *testing.T) {
	observeDir := filepath.Join(t.TempDir(), "observe")
	if err := os.MkdirAll(filepath.Join(observeDir, strings.Repeat("d", 32)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := CleanStaleInstances(observeDir, NewSystemClock()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(observeDir, strings.Repeat("d", 32))); err != nil {
		t.Error("a recent snapshotless instance must be kept")
	}
	old := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(observeDir, strings.Repeat("d", 32)), old, old); err != nil {
		t.Fatal(err)
	}
	if err := CleanStaleInstances(observeDir, NewSystemClock()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(observeDir, strings.Repeat("d", 32))); err == nil {
		t.Error("an old snapshotless instance must be deleted by the age rule")
	}
}

func TestEnsureInstanceDirRejectsSymlinks(t *testing.T) {
	dir := t.TempDir()
	observeDir := filepath.Join(dir, "observe")
	if err := os.MkdirAll(observeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	id := strings.Repeat("e", 32)
	if err := os.Symlink(target, filepath.Join(observeDir, id)); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureInstanceDir(observeDir, id); err == nil {
		t.Error("a symlinked instance path must be rejected")
	}
	if _, err := EnsureInstanceDir(observeDir, "short"); err == nil {
		t.Error("a non-instance directory name must be rejected")
	}
}

func exitedChildPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "0.02")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn a child process: %v", err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait for the child: %v", err)
	}
	return pid
}

func TestConcurrentPublishersAndRecordersStayConsistent(t *testing.T) {
	dir := t.TempDir()
	cfg := testPublisherConfig(t, dir)
	cfg.Interval = 2 * time.Millisecond
	publisher, err := NewPublisher(cfg)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				cfg.Recorder.Add("parse_records", 1, "normalized")
				cfg.Recorder.Set("watch_sources", 3, "active")
			}
		}()
	}
	wg.Wait()
	publisher.Close()
	snapshot := readSnapshotFile(t, publisher.Dir())
	if snapshot.Counters["parse_records{parse_result=normalized}"] != 800 {
		t.Errorf("counter = %d, want 800", snapshot.Counters["parse_records{parse_result=normalized}"])
	}
}

func TestCleanStaleInstancesTreatsStoppedSnapshotAsTerminatedEvenWithLivePID(t *testing.T) {
	observeDir := filepath.Join(t.TempDir(), "observe")
	if err := os.MkdirAll(observeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	paths := make([]string, RetainedInstances+2)
	for i := range paths {
		id := fmt.Sprintf("a%031d", i+1)
		instanceDir := filepath.Join(observeDir, id)
		if err := os.MkdirAll(instanceDir, 0o700); err != nil {
			t.Fatal(err)
		}
		snapshot := Snapshot{
			FormatVersion: SnapshotFormatVersion,
			Instance:      Instance{InstanceID: id, PID: os.Getpid(), Roles: []string{"mcp"}, StartWallMS: time.Now().UnixMilli()},
			UptimeMS:      1,
			SampledWallMS: time.Now().UnixMilli(),
			Counters:      map[string]uint64{},
			Stopped:       true,
		}
		raw, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(instanceDir, snapshotFileName), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		paths[i] = instanceDir
	}
	if err := CleanStaleInstances(observeDir, NewSystemClock()); err != nil {
		t.Fatalf("CleanStaleInstances: %v", err)
	}
	remaining := 0
	for _, path := range paths {
		if _, err := os.Stat(path); err == nil {
			remaining++
		}
	}
	if remaining != RetainedInstances {
		t.Errorf("stopped instances kept = %d, want %d", remaining, RetainedInstances)
	}
}

func TestPublisherPeriodicallyCleansStaleInstances(t *testing.T) {
	dir := t.TempDir()
	cfg := testPublisherConfig(t, dir)
	cfg.Interval = 5 * time.Millisecond
	cfg.CleanInterval = 10 * time.Millisecond
	publisher, err := NewPublisher(cfg)
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	defer publisher.Close()

	deadPID := exitedChildPID(t)
	created := make([]string, RetainedInstances+3)
	for i := range created {
		id := fmt.Sprintf("b%031d", i+1)
		instanceDir := filepath.Join(dir, id)
		if err := os.MkdirAll(instanceDir, 0o700); err != nil {
			t.Fatal(err)
		}
		snapshot := Snapshot{
			FormatVersion: SnapshotFormatVersion,
			Instance:      Instance{InstanceID: id, PID: deadPID, Roles: []string{"mcp"}, StartWallMS: time.Now().UnixMilli()},
			UptimeMS:      1,
			SampledWallMS: time.Now().UnixMilli(),
			Counters:      map[string]uint64{},
		}
		raw, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(instanceDir, snapshotFileName), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		created[i] = instanceDir
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		remaining := 0
		for _, path := range created {
			if _, err := os.Stat(path); err == nil {
				remaining++
			}
		}
		if remaining == RetainedInstances {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("publisher did not periodically clean stale terminated instances")
}
