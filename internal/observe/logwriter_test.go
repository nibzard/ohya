package observe

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testLogWriter(t *testing.T, dir string, mutate func(*LogWriterConfig)) (*LogWriter, *Recorder, *SystemClock) {
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
	cfg := LogWriterConfig{Dir: dir, Instance: instance, Clock: clock, Recorder: recorder}
	if mutate != nil {
		mutate(&cfg)
	}
	w, err := NewLogWriter(cfg)
	if err != nil {
		t.Fatalf("NewLogWriter: %v", err)
	}
	return w, recorder, clock
}

func readLogLines(t *testing.T, path string) []LogEntry {
	t.Helper()
	entries := readLogLinesIfExists(t, path)
	if entries == nil {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read log file: %v", err)
		}
		t.Fatalf("log file %s holds no valid entries: %q", path, raw)
	}
	return entries
}

func readLogLinesIfExists(t *testing.T, path string) []LogEntry {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	if !bytes.HasSuffix(raw, []byte("\n")) {
		if idx := bytes.LastIndexByte(raw, '\n'); idx >= 0 {
			raw = raw[:idx+1]
		} else {
			raw = nil
		}
	}
	var entries []LogEntry
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		if line == "" {
			continue
		}
		var entry LogEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Errorf("decode log line %q: %v", line, err)
			continue
		}
		if err := entry.Validate(); err != nil {
			t.Errorf("validate log line: %v", err)
			continue
		}
		entries = append(entries, entry)
	}
	return entries
}

func TestLogWriterWritesEntriesWithPrivatePermissions(t *testing.T) {
	dir := t.TempDir()
	w, _, _ := testLogWriter(t, dir, nil)
	if !w.Event("ingest", LevelInfo, EventStartup, "", map[string]int64{"roles": 1}) {
		t.Fatal("first event was dropped")
	}
	if !w.Event("ingest", LevelWarn, EventCheckpointFailed, "flush", nil) {
		t.Fatal("second event was dropped")
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	instanceDir := filepath.Join(dir, w.cfg.Instance.InstanceID)
	info, err := os.Stat(instanceDir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("instance dir mode = %o, want 0700", info.Mode().Perm())
	}
	path := filepath.Join(instanceDir, logsFileName)
	logInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if logInfo.Mode().Perm() != 0o600 {
		t.Fatalf("log file mode = %o, want 0600", logInfo.Mode().Perm())
	}
	entries := readLogLines(t, path)
	if len(entries) != 3 {
		t.Fatalf("entries = %d, want 2 emitted plus the final summary", len(entries))
	}
	if entries[0].EventCode != string(EventStartup) || entries[0].Component != "ingest" || entries[0].Counts["roles"] != 1 {
		t.Fatalf("startup entry = %+v", entries[0])
	}
	if entries[1].Level != LevelWarn || entries[1].Outcome != "flush" {
		t.Fatalf("warn entry = %+v", entries[1])
	}
	if entries[2].EventCode != string(EventSummary) || entries[2].Outcome != "final" {
		t.Fatalf("final entry = %+v", entries[2])
	}
}

func TestLogWriterCoalescesBurstWarningsIntoQueuedEntry(t *testing.T) {
	dir := t.TempDir()
	release := make(chan struct{})
	w, recorder, _ := testLogWriter(t, dir, func(cfg *LogWriterConfig) {
		cfg.writeLine = func(w *LogWriter, entry LogEntry, line []byte) error {
			<-release
			return nil
		}
	})
	if !w.Event("uploader", LevelWarn, EventTransportOffline, "connect", nil) {
		t.Fatal("first event dropped")
	}
	time.Sleep(20 * time.Millisecond)
	for i := 0; i < 30; i++ {
		if !w.Event("uploader", LevelWarn, EventTransportOffline, "connect", nil) {
			t.Fatalf("event %d dropped", i)
		}
	}
	close(release)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := w.Coalesced(); got < 20 {
		t.Fatalf("coalesced = %d, want at least 20 of the 31 identical warnings", got)
	}
	if got := recorder.Snapshot().Counters["instrumentation_log_coalesced"]; got < 20 {
		t.Fatalf("recorder coalesced = %d, want at least 20", got)
	}
}

func TestLogWriterRotatesAtBoundAndKeepsBoundedFiles(t *testing.T) {
	dir := t.TempDir()
	w, _, _ := testLogWriter(t, dir, func(cfg *LogWriterConfig) {
		cfg.MaxFileBytes = 400
	})
	for i := 0; i < 40; i++ {
		if !w.Event("ingest", LevelInfo, EventStartup, "", map[string]int64{"n": int64(i)}) {
			t.Fatalf("event %d dropped", i)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	instanceDir := filepath.Join(dir, w.cfg.Instance.InstanceID)
	for _, name := range []string{logsFileName, logsFileName + ".1", logsFileName + ".2"} {
		info, err := os.Stat(filepath.Join(instanceDir, name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if info.Size() > 400+LogLineMaxBytes {
			t.Fatalf("%s grew to %d bytes past the bound", name, info.Size())
		}
	}
	if _, err := os.Stat(filepath.Join(instanceDir, logsFileName+".3")); err == nil {
		t.Fatal("a third rotated file exists past the retention bound")
	}
	newest := int64(-1)
	for _, name := range []string{logsFileName, logsFileName + ".1", logsFileName + ".2"} {
		for _, entry := range readLogLines(t, filepath.Join(instanceDir, name)) {
			if n, ok := entry.Counts["n"]; ok {
				newest = max(newest, n)
			}
		}
	}
	if newest != 39 {
		t.Fatalf("newest count across the bounded files = %d, want 39", newest)
	}
}

func TestLogWriterRotationUnderConcurrentReaders(t *testing.T) {
	dir := t.TempDir()
	w, _, _ := testLogWriter(t, dir, func(cfg *LogWriterConfig) {
		cfg.MaxFileBytes = 512
	})
	instanceDir := w.Dir()
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for r := 0; r < 3; r++ {
		wg.Add(1)
		go func(reader int) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, name := range []string{logsFileName, logsFileName + ".1"} {
					raw, err := os.ReadFile(filepath.Join(instanceDir, name))
					if err != nil {
						continue
					}
					if !bytes.HasSuffix(raw, []byte("\n")) {
						if idx := bytes.LastIndexByte(raw, '\n'); idx >= 0 {
							raw = raw[:idx+1]
						} else {
							raw = nil
						}
					}
					for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
						if line == "" {
							continue
						}
						var entry LogEntry
						if err := json.Unmarshal([]byte(line), &entry); err != nil {
							t.Errorf("reader %d saw a torn line in %s: %v", reader, name, err)
							return
						}
					}
				}
			}
		}(r)
	}
	for i := 0; i < 200; i++ {
		if !w.Event("ingest", LevelWarn, EventCommitFailed, "ingest", map[string]int64{"n": int64(i)}) {
			t.Fatalf("event %d dropped", i)
		}
	}
	close(stop)
	wg.Wait()
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestLogWriterDropsWhenQueueIsFull(t *testing.T) {
	dir := t.TempDir()
	release := make(chan struct{})
	w, recorder, _ := testLogWriter(t, dir, func(cfg *LogWriterConfig) {
		cfg.QueueDepth = 2
		cfg.writeLine = func(w *LogWriter, entry LogEntry, line []byte) error {
			<-release
			return nil
		}
	})
	for i := 0; i < 8; i++ {
		w.Event("ingest", LevelInfo, EventStartup, "", map[string]int64{"i": int64(i)})
	}
	if w.Dropped() == 0 {
		t.Fatal("a full queue must drop entries without blocking producers")
	}
	close(release)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if recorder.Snapshot().Counters["instrumentation_log_dropped"] == 0 {
		t.Fatal("queue drops were not counted")
	}
}

func TestLogWriterRejectsInvalidCodesAndOversizeLines(t *testing.T) {
	dir := t.TempDir()
	w, recorder, _ := testLogWriter(t, dir, nil)
	if w.Event("ingest", LevelInfo, EventCode("not_a_code"), "", nil) {
		t.Fatal("an unknown event code was accepted")
	}
	counts := map[string]int64{}
	for i := 0; i < 400; i++ {
		counts[fmt.Sprintf("counter_with_a_rather_long_name_%d", i)] = int64(i)
	}
	if !w.Event("ingest", LevelInfo, EventSummary, "", counts) {
		t.Fatal("valid summary dropped")
	}
	_ = recorder
	_ = w.Close()
	if w.Dropped() != 2 {
		t.Fatalf("dropped = %d, want 2 (invalid code, oversize line)", w.Dropped())
	}
	entries := readLogLines(t, filepath.Join(dir, w.cfg.Instance.InstanceID, logsFileName))
	if len(entries) != 1 || entries[0].EventCode != string(EventSummary) || entries[0].Outcome != "final" {
		t.Fatalf("entries = %+v", entries)
	}
}

func TestLogWriterStderrFallbackIsBoundedAndRateLimited(t *testing.T) {
	dir := t.TempDir()
	var stderr bytes.Buffer
	w, recorder, _ := testLogWriter(t, dir, func(cfg *LogWriterConfig) {
		cfg.Stderr = &stderr
	})
	instanceDir := w.Dir()
	if err := os.Mkdir(filepath.Join(instanceDir, logsFileName), 0o700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		w.Event("ingest", LevelInfo, EventStartup, "", nil)
		time.Sleep(2 * time.Millisecond)
	}
	notices := strings.Count(stderr.String(), "application log write failed")
	if notices == 0 {
		t.Fatal("the first write failure never reached stderr")
	}
	if notices > 2 {
		t.Fatalf("stderr notices = %d, want at most one per minute", notices)
	}
	if recorder.Snapshot().Counters["instrumentation_write_errors{write_target=log}"] == 0 {
		t.Fatal("write errors were not counted")
	}
	if strings.Contains(stderr.String(), instanceDir) {
		t.Fatal("the stderr notice leaked the instance path")
	}
	if err := w.Close(); err == nil {
		t.Fatal("Close must report the first write failure")
	}
}

func TestLogWriterEmitsSummaryWhenActiveAndHeartbeatWhenIdle(t *testing.T) {
	dir := t.TempDir()
	w, recorder, _ := testLogWriter(t, dir, func(cfg *LogWriterConfig) {
		cfg.SummaryInterval = 30 * time.Millisecond
		cfg.HeartbeatInterval = 90 * time.Millisecond
	})
	recorder.Add("ingest_events", 1, "inserted")
	path := filepath.Join(w.Dir(), logsFileName)
	deadline := time.Now().Add(3 * time.Second)
	var summary, heartbeat int
	for time.Now().Before(deadline) {
		summary, heartbeat = 0, 0
		for _, entry := range readLogLinesIfExists(t, path) {
			switch entry.EventCode {
			case string(EventSummary):
				if entry.Outcome != "final" {
					summary++
				}
			case string(EventHeartbeat):
				heartbeat++
			}
		}
		if summary >= 1 && heartbeat >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if summary == 0 {
		t.Fatal("an active recorder never produced a summary entry")
	}
	if heartbeat == 0 {
		t.Fatal("an idle recorder never produced a heartbeat entry")
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
