package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSectionFromRecordsFramesWireLayout(t *testing.T) {
	records := [][]byte{[]byte(`{"a":1}`), []byte(`{"b":2}`)}
	section := sectionFromRecords(records)
	want := 2*12 + len(records[0]) + len(records[1])
	if len(section) != want {
		t.Fatalf("section length = %d, want %d", len(section), want)
	}
	off := binary.BigEndian.Uint64(section[:8])
	n := binary.BigEndian.Uint32(section[8:12])
	if off != 0 || int(n) != len(records[0]) || !bytes.Equal(section[12:12+n], records[0]) {
		t.Fatalf("first record framing wrong: offset %d len %d", off, n)
	}
	off2 := binary.BigEndian.Uint64(section[12+len(records[0]) : 12+len(records[0])+8])
	if off2 != uint64(len(records[0])+1) {
		t.Fatalf("second offset = %d, want %d", off2, len(records[0])+1)
	}
}

func TestFileSectionMatchesRecordSection(t *testing.T) {
	data := []byte("{\"x\":1}\n{\"y\":2}\n{\"z\":3}\n")
	byFile := fileSection(data)
	var records [][]byte
	for _, line := range bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n")) {
		records = append(records, line)
	}
	if !bytes.Equal(byFile, sectionFromRecords(records)) {
		t.Fatalf("file section and record section differ")
	}
	incomplete := []byte("{\"x\":1}\n{\"y\":2}\n{\"tail\"")
	if got, want := len(fileSection(incomplete)), 2*(12+len("{\"x\":1}")); got != want {
		t.Fatalf("incomplete tail changed the section: %d bytes, want %d", got, want)
	}
}

func TestSplitFilesHalvesWithoutOverlap(t *testing.T) {
	files := []dictFile{
		{path: "a", section: make([]byte, 10)},
		{path: "b", section: make([]byte, 20)},
		{path: "c", section: make([]byte, 30)},
		{path: "d", section: make([]byte, 40)},
	}
	train, measure := splitFiles(files)
	var total int64
	for _, f := range append(append([]dictFile{}, train...), measure...) {
		total += int64(len(f.section))
	}
	if total != 100 {
		t.Fatalf("split lost bytes: train %d + measure %d = %d, want 100", len(train), len(measure), total)
	}
	if len(train) == 0 || len(measure) == 0 {
		t.Fatalf("split produced empty halves: %d train, %d measure", len(train), len(measure))
	}
	seen := map[string]int{}
	for _, f := range train {
		seen[f.path]++
	}
	for _, f := range measure {
		seen[f.path]++
	}
	for _, f := range files {
		if seen[f.path] != 1 {
			t.Fatalf("file %s appears %d times across halves", f.path, seen[f.path])
		}
	}
}

func TestBucketPacesWrites(t *testing.T) {
	b := newBucket(4 << 20)
	defer b.close()
	start := time.Now()
	b.take(128 << 10)
	elapsed := time.Since(start)
	if elapsed < 20*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("taking 128 KiB at 4 MiB/s took %s; want roughly 30 ms", elapsed)
	}
}

func TestWalkCorpusRequiresFixture(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := walkCorpus(dir); err == nil {
		t.Fatalf("empty corpus root must fail, not return files")
	}
	agentDir := filepath.Join(dir, "claude-code")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "s.jsonl"), []byte("{\"a\":1}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	files, total, err := walkCorpus(dir)
	if err != nil || len(files) != 1 || total != 8 {
		t.Fatalf("walkCorpus = %d files, %d bytes, err %v; want 1 file, 8 bytes", len(files), total, err)
	}
	if files[0].agent != "claude-code" {
		t.Fatalf("agent = %q, want claude-code", files[0].agent)
	}
}
