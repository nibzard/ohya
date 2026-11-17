package observe

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newUsageWriter(t *testing.T, dir string, mutate func(*UsageWriterConfig)) *UsageWriter {
	t.Helper()
	cfg := UsageWriterConfig{
		Dir:      dir,
		Instance: validUsageInstance(),
		Clock:    NewFakeClock(1794840423123),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	w, err := NewUsageWriter(cfg)
	if err != nil {
		t.Fatalf("NewUsageWriter: %v", err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

func validUsageInstance() Instance {
	return Instance{
		InstanceID:  "3f9b2c1d8e4a7f60b5d3c2a19e8f7b6d",
		PID:         4242,
		Roles:       []string{"mcp"},
		StartWallMS: 1794840423123,
	}
}

func usageTestRecord(rows int64) UsageRecord {
	return UsageRecord{
		SchemaVersion: UsageFormatVersion,
		Tool:          UsageToolSQL,
		Client:        UsageClientClaudeCode,
		Path:          UsagePathLocal,
		Outcome:       UsageOutcomeOK,
		Rows:          rows,
		PhaseMS:       UsagePhases{TotalMS: 1.5},
	}
}

func usageFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, validUsageInstance().InstanceID))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func usageLines(t *testing.T, path string) []UsageRecord {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	defer file.Close()
	var records []UsageRecord
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var record UsageRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatalf("decode usage line %q: %v", scanner.Text(), err)
		}
		records = append(records, record)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return records
}

func TestUsageWriterWritesStampedRecords(t *testing.T) {
	dir := t.TempDir()
	w := newUsageWriter(t, dir, nil)
	if !w.Record(usageTestRecord(3)) {
		t.Fatal("first record dropped")
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	path := filepath.Join(dir, validUsageInstance().InstanceID, "usage.jsonl")
	records := usageLines(t, path)
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	record := records[0]
	if record.TimeMS != 1794840423123 {
		t.Errorf("time_ms = %d, want the writer clock stamp", record.TimeMS)
	}
	if record.TimeBucket != "2026-11-16T14:00Z" {
		t.Errorf("time_bucket = %q", record.TimeBucket)
	}
	if record.InstanceID != validUsageInstance().InstanceID {
		t.Errorf("instance_id = %q", record.InstanceID)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("usage file mode = %o, want 0600", info.Mode().Perm())
	}
	dirInfo, err := os.Stat(filepath.Join(dir, validUsageInstance().InstanceID))
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode().Perm() != 0o700 {
		t.Errorf("usage dir mode = %o, want 0700", dirInfo.Mode().Perm())
	}
}

func TestUsageWriterDropsInvalidRecords(t *testing.T) {
	dir := t.TempDir()
	w := newUsageWriter(t, dir, nil)
	record := usageTestRecord(1)
	record.Outcome = "crashed"
	if w.Record(record) {
		t.Fatal("invalid record accepted")
	}
	if w.Dropped() != 1 {
		t.Fatalf("dropped = %d, want 1", w.Dropped())
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if files := usageFiles(t, dir); len(files) != 0 {
		t.Fatalf("files = %v, want none", files)
	}
}

func TestUsageWriterRejectsRecordAfterClose(t *testing.T) {
	dir := t.TempDir()
	w := newUsageWriter(t, dir, nil)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if w.Record(usageTestRecord(1)) {
		t.Fatal("record accepted after close")
	}
	if w.Dropped() != 1 {
		t.Fatalf("dropped = %d, want 1", w.Dropped())
	}
}

func TestUsageWriterRotatesAndBoundsFiles(t *testing.T) {
	measureDir := t.TempDir()
	w := newUsageWriter(t, measureDir, nil)
	if !w.Record(usageTestRecord(1)) {
		t.Fatal("seed record dropped")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(measureDir, validUsageInstance().InstanceID, "usage.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	line := info.Size()

	dir := t.TempDir()
	w2 := newUsageWriter(t, dir, func(cfg *UsageWriterConfig) {
		cfg.MaxFileBytes = line + line/2
	})
	for rows := int64(2); rows <= 5; rows++ {
		if !w2.Record(usageTestRecord(rows)) {
			t.Fatalf("record %d dropped", rows)
		}
	}
	if err := w2.Close(); err != nil {
		t.Fatal(err)
	}
	instanceDir := filepath.Join(dir, validUsageInstance().InstanceID)
	files := usageFiles(t, dir)
	if len(files) != 3 {
		t.Fatalf("files = %v, want usage.jsonl, .1, and .2", files)
	}
	want := map[string]int64{
		"usage.jsonl":   5,
		"usage.jsonl.1": 4,
		"usage.jsonl.2": 3,
	}
	for name, rows := range want {
		records := usageLines(t, filepath.Join(instanceDir, name))
		if len(records) != 1 || records[0].Rows != rows {
			t.Fatalf("%s = %v, want exactly record rows=%d", name, records, rows)
		}
	}
}

func TestUsageWriterCountsWriteFailures(t *testing.T) {
	dir := t.TempDir()
	instanceDir := filepath.Join(dir, validUsageInstance().InstanceID)
	if err := os.MkdirAll(filepath.Join(instanceDir, "usage.jsonl"), 0o700); err != nil {
		t.Fatal(err)
	}
	w := newUsageWriter(t, dir, nil)
	if !w.Record(usageTestRecord(1)) {
		t.Fatal("record dropped at enqueue; the failure belongs to the writer loop")
	}
	if err := w.Close(); err == nil {
		t.Fatal("Close must report the write failure")
	}
	if w.Failed() == 0 {
		t.Error("write failures not counted")
	}
}

func TestUsageWriterRejectsSymlinkedInstanceDir(t *testing.T) {
	dir := t.TempDir()
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(dir, validUsageInstance().InstanceID)); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := NewUsageWriter(UsageWriterConfig{
		Dir:      dir,
		Instance: validUsageInstance(),
		Clock:    NewFakeClock(1),
	}); err == nil {
		t.Fatal("symlinked instance directory accepted")
	}
}

func TestUsageWriterRejectsSymlinkedFile(t *testing.T) {
	dir := t.TempDir()
	instanceDir := filepath.Join(dir, validUsageInstance().InstanceID)
	if err := os.MkdirAll(instanceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "outside.jsonl")
	if err := os.Symlink(target, filepath.Join(instanceDir, "usage.jsonl")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	w := newUsageWriter(t, dir, nil)
	if !w.Record(usageTestRecord(1)) {
		t.Fatal("record dropped at enqueue")
	}
	if err := w.Close(); err == nil {
		t.Fatal("symlinked usage file accepted")
	}
	if w.Failed() == 0 {
		t.Error("write failures not counted")
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Error("writer wrote through a symlink")
	}
}

func TestUsageWriterConfigValidation(t *testing.T) {
	clock := NewFakeClock(1)
	cases := []struct {
		name string
		cfg  UsageWriterConfig
		want string
	}{
		{"no directory", UsageWriterConfig{Instance: validUsageInstance(), Clock: clock}, "directory"},
		{"no instance", UsageWriterConfig{Dir: t.TempDir(), Clock: clock}, "instance"},
		{"no clock", UsageWriterConfig{Dir: t.TempDir(), Instance: validUsageInstance()}, "clock"},
	}
	for _, tc := range cases {
		if _, err := NewUsageWriter(tc.cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}
}
