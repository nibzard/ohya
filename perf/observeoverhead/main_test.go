package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestPercentile(t *testing.T) {
	sorted := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}
	if got := percentile(sorted, 95); got != 19 {
		t.Fatalf("percentile(95) of 1..20 = %v, want 19", got)
	}
	if got := percentile(sorted, 50); got != 10 {
		t.Fatalf("percentile(50) of 1..20 = %v, want 10", got)
	}
	if got := percentile(nil, 95); got != 0 {
		t.Fatalf("percentile of empty = %v, want 0", got)
	}
}

func TestWriteStopFixtures(t *testing.T) {
	dir := t.TempDir()
	records, err := writeStopFixtures(dir)
	if err != nil {
		t.Fatal(err)
	}
	if records != 3 {
		t.Fatalf("stop fixtures report %d ingestible records, want 3", records)
	}
	malformed, err := os.ReadFile(filepath.Join(dir, "malformed.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(malformed), "\n"); got != 4 {
		t.Fatalf("malformed fixture holds %d lines, want 4", got)
	}
	third := strings.SplitN(string(malformed), "\n", 4)[2]
	if !strings.Contains(third, "NOT JSON") {
		t.Fatalf("third line of malformed fixture is not the malformed record: %s", third)
	}
	oversize, err := os.ReadFile(filepath.Join(dir, "oversize.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(oversize), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("oversize fixture holds %d lines, want 3", len(lines))
	}
	if len(lines[1]) <= maxRecordBytesLimit {
		t.Fatalf("oversize line is %d bytes, want more than the %d byte limit", len(lines[1]), maxRecordBytesLimit)
	}
}

func TestProcReadingsOnSelf(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("procRSSKiB reads /proc/<pid>/status")
	}
	if rss := procRSSKiB(os.Getpid()); rss <= 0 {
		t.Fatalf("procRSSKiB(self) = %d, want positive", rss)
	}
	sample := procSnapshot(os.Getpid())
	if sample.writeBytes < 0 || sample.cpuSec < 0 {
		t.Fatalf("procSnapshot(self) = %+v, want non-negative fields", sample)
	}
}

func TestTreeBytesAndFiles(t *testing.T) {
	dir := t.TempDir()
	if got := treeBytes(dir); got != 0 {
		t.Fatalf("treeBytes(empty) = %d, want 0", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "snapshot.json"), []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "inst")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "events.jsonl"), []byte("ab\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := treeBytes(dir); got != 8 {
		t.Fatalf("treeBytes = %d, want 8", got)
	}
	if got := treeFiles(dir); got != 2 {
		t.Fatalf("treeFiles = %d, want 2", got)
	}
}

func TestCountStopEvents(t *testing.T) {
	dir := t.TempDir()
	observe := filepath.Join(dir, "observe", "inst")
	if err := os.MkdirAll(observe, 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"event_code":"startup"}` + "\n" +
		`{"event_code":"source_stopped","outcome":"invalid"}` + "\n" +
		`{"event_code":"source_stopped","outcome":"oversize"}` + "\n"
	if err := os.WriteFile(filepath.Join(observe, "events.jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := countStopEvents(filepath.Join(dir, "observe")); got != 2 {
		t.Fatalf("countStopEvents = %d, want 2", got)
	}
}

func TestRegisterCleanupWritesIdempotentLine(t *testing.T) {
	tmp := t.TempDir()
	list := filepath.Join(tmp, "cleanup.sh")
	t.Setenv("LOOPER_RESOURCE_CLEANUP_FILE", list)
	root := filepath.Join(tmp, "owned-root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	registerCleanup(root)
	registerCleanup(root)
	data, err := os.ReadFile(list)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("cleanup file holds %d lines after two registrations, want 2", len(lines))
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "test -e ") || !strings.Contains(line, "rm -rf -- ") {
			t.Fatalf("cleanup line %q is not a guarded rm -rf", line)
		}
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote(`/tmp/a b`); got != `'/tmp/a b'` {
		t.Fatalf("shellQuote = %s", got)
	}
	if got := shellQuote("it's"); got != `'it'\''s'` {
		t.Fatalf("shellQuote with quote = %s", got)
	}
}

func TestRenderReportVerdicts(t *testing.T) {
	opts := options{corpusMiB: 24, reps: 2, idle: time.Second, interval: time.Second}
	results := []runResult{
		{rep: 1, side: "off", importMS: 5000, steadyKiB: 20 * 1024, events: 10},
		{rep: 1, side: "on", importMS: 5100, steadyKiB: 24 * 1024, obsBytes: 40960, obsFiles: 3, statusP95: 40, versionP95: 20, events: 10, stopLogs: 2},
		{rep: 2, side: "on", importMS: 5150, steadyKiB: 25 * 1024, obsBytes: 40960, obsFiles: 3, statusP95: 45, versionP95: 21, events: 10, stopLogs: 2},
		{rep: 2, side: "off", importMS: 5050, steadyKiB: 21 * 1024, events: 10},
	}
	report, err := renderReport(opts, "/bin/ohya", 10, results)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"median end-to-end import wall overhead",
		"budget <= 3%: PASS",
		"budget <= 16 MiB: PASS",
		"budget <= 100 ms",
		"ceiling 24 MiB logs + 16 MiB history + 256 KiB snapshot: PASS",
	} {
		if !strings.Contains(report, want) {
			t.Fatalf("report missing %q:\n%s", want, report)
		}
	}
	failing := []runResult{
		{rep: 1, side: "off", importMS: 1000, steadyKiB: 10 * 1024, events: 10},
		{rep: 1, side: "on", importMS: 5000, steadyKiB: 90 * 1024, statusP95: 400, events: 10},
	}
	report, err = renderReport(opts, "/bin/ohya", 10, failing)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report, "FAIL") || !strings.Contains(report, "A budget was exceeded") {
		t.Fatalf("report does not surface failing budgets:\n%s", report)
	}
}
