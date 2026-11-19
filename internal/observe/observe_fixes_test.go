package observe

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLogWriterCoalescesNilCountsIntoEmittedEntry(t *testing.T) {
	dir := t.TempDir()
	inst := Instance{
		InstanceID:  "0123456789abcdef0123456789abcdef",
		Roles:       []string{string(RoleIngest)},
		PID:         1,
		StartWallMS: 1000,
	}
	block := make(chan struct{})
	firstStarted := make(chan struct{})
	var written []LogEntry
	var mu sync.Mutex
	w, err := NewLogWriter(LogWriterConfig{
		Dir:             dir,
		Instance:        inst,
		Clock:           NewSystemClock(),
		SummaryInterval: time.Hour,
		writeLine: func(lw *LogWriter, entry LogEntry, line []byte) error {
			mu.Lock()
			idx := len(written)
			written = append(written, entry)
			mu.Unlock()
			if idx == 0 {
				close(firstStarted)
				<-block
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	w.Event("ingest", LevelInfo, EventSourceReturned, "", nil)
	<-firstStarted
	w.Event("ingest", LevelWarn, EventCommitFailed, "register", nil)
	w.Event("ingest", LevelWarn, EventCommitFailed, "register", nil)
	w.Event("ingest", LevelWarn, EventCommitFailed, "register", nil)
	close(block)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(written) != 2 {
		t.Fatalf("written entries = %d, want 2: %+v", len(written), written)
	}
	if got := written[1].Counts["coalesced"]; got != 2 {
		t.Fatalf("coalesced count on emitted entry = %d, want 2 (counts=%+v)", got, written[1].Counts)
	}
}

func TestUsageWriterConcurrentCloseHasNoRace(t *testing.T) {
	dir := t.TempDir()
	inst := Instance{
		InstanceID:  "0123456789abcdef0123456789abcdef",
		Roles:       []string{string(RoleMCP)},
		PID:         1,
		StartWallMS: 1000,
	}
	w, err := NewUsageWriter(UsageWriterConfig{
		Dir:      dir,
		Instance: inst,
		Clock:    NewSystemClock(),
	})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = w.Close()
		}()
	}
	wg.Wait()
}

func TestParseProcStatHandlesSpacesAndParensInComm(t *testing.T) {
	raw := "12345 (ohya (agent) v2) S 1 12345 12345 0 -1 4194304 100 0 0 0 250 150 0 0 20 0 4 0 1234 500000 42 18446744073709551615"
	cpu, pages, ok := parseProcStat(raw)
	if !ok {
		t.Fatal("parseProcStat returned ok=false")
	}
	if cpu != 4.0 {
		t.Fatalf("cpu = %v, want 4.0", cpu)
	}
	if pages != 42 {
		t.Fatalf("pages = %v, want 42", pages)
	}
}

func TestPublisherPruneHistoryThrottlesUnderByteLimit(t *testing.T) {
	dir := t.TempDir()
	clk := NewSystemClock()
	rec, err := NewRecorder(Instance{
		InstanceID:  "0123456789abcdef0123456789abcdef",
		Roles:       []string{string(RoleIngest)},
		PID:         1,
		StartWallMS: 1000,
	}, clk)
	if err != nil {
		t.Fatal(err)
	}
	p := &Publisher{
		cfg: PublisherConfig{
			Dir:             dir,
			Recorder:        rec,
			Clock:           clk,
			MaxHistoryBytes: 1 << 20,
			MaxHistoryAge:   24 * time.Hour,
		},
		dir:       dir,
		writeErrs: map[string]uint64{},
	}
	path := filepath.Join(dir, historyFileName)
	var lines []string
	for _, ts := range []int64{1000, 100_000_000} {
		b, _ := json.Marshal(struct {
			SampledWallMS int64 `json:"sampled_wall_ms"`
		}{SampledWallMS: ts})
		lines = append(lines, string(b))
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	now := int64(100_000_000)
	p.pruneHistory(now)
	firstInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p.pruneHistory(now + 60_000)
	secondInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if secondInfo.Size() <= firstInfo.Size() {
		t.Fatalf("expected pruneHistory to skip rewrite within throttle interval (first=%d second=%d)", firstInfo.Size(), secondInfo.Size())
	}
}
