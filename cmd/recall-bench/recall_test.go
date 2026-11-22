package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRunTree(t *testing.T, dir string, questions []Question, records []runRecord) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "runs"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(questionsSnapshotPath(dir), questions); err != nil {
		t.Fatal(err)
	}
	for _, r := range records {
		leaf := filepath.Join(dir, "runs", r.Arm, r.Question, "r1")
		if err := os.MkdirAll(leaf, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeJSON(filepath.Join(leaf, "meta.json"), r); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(leaf, "stream.jsonl"), []byte(""), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCompareRunsRendersTiersDeltasAndCoverage(t *testing.T) {
	q1 := Question{Key: "q1", Tier: "needle", Prompt: "p", AnswerContains: []string{"a"}, EvidenceMarker: "m m m", ExpectedSessions: 1, ExpectedFiles: 1}
	q2 := Question{Key: "q2", Tier: "timeboxed", Prompt: "p", AnswerContains: []string{"b"}, EvidenceMarker: "n n n", ExpectedSessions: 1, ExpectedFiles: 1}
	dirA := t.TempDir()
	writeRunTree(t, dirA, []Question{q1}, []runRecord{
		{Question: "q1", Arm: armRaw, Verdict: "correct", WallSeconds: 10, InputTokens: 100, CostUSD: 0.10},
		{Question: "q1", Arm: armOhya, Verdict: "correct", WallSeconds: 5, InputTokens: 80, CostUSD: 0.05},
	})
	dirB := t.TempDir()
	writeRunTree(t, dirB, []Question{q1, q2}, []runRecord{
		{Question: "q1", Arm: armRaw, Verdict: "wrong-answer", WallSeconds: 8, InputTokens: 90, CostUSD: 0.20},
		{Question: "q1", Arm: armOhya, Verdict: "correct", WallSeconds: 4, InputTokens: 70, CostUSD: 0.04},
		{Question: "q2", Arm: armRaw, Verdict: "correct", WallSeconds: 3, InputTokens: 50, CostUSD: 0.02},
		{Question: "q2", Arm: armOhya, Verdict: "timeout", WallSeconds: 180, InputTokens: 0, CostUSD: 0.30},
	})
	out, err := compareRuns(dirA, dirB, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"| needle | raw | 1/1 | 0/1 | 10.0 | 8.0 | -20% |",
		"| needle | ohya | 1/1 | 1/1 | 5.0 | 4.0 | -20% |",
		"| timeboxed | raw | - | 1/1 |",
		"| timeboxed | ohya | - | 0/1 |",
		"only in B: q2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q", want)
		}
	}
	if !strings.Contains(out, "| raw | $0.100 | $0.220 |") {
		t.Errorf("cost per correct raw row wrong:\n%s", out)
	}
	if !strings.Contains(out, "| ohya | $0.050 | $0.340 |") {
		t.Errorf("cost per correct ohya row wrong:\n%s", out)
	}
	if !strings.Contains(out, "no manifest") {
		t.Errorf("synthetic dirs have no manifest; the header must say so")
	}
}

type fakeCounter struct {
	counts map[string]int
}

func (f fakeCounter) countSessionsWithText(_ context.Context, marker string, settled bool) (int, error) {
	if !settled {
		return f.counts[marker], nil
	}
	return f.counts[marker+"/settled"], nil
}

func TestRecalibrateRewritesDriftAndKeepsStable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "curated.json")
	original := `[{"key":"drift","tier":"needle","prompt":"p","answer_contains":["a"],"evidence_marker":"m m m","expected_sessions":1,"expected_files":1},
{"key":"stable","tier":"needle","prompt":"p","answer_contains":["b"],"evidence_marker":"n n n","expected_sessions":2,"expected_settled":1,"expected_files":2}]`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	oldFiles := filesWithText
	defer func() { filesWithText = oldFiles }()
	filesWithText = func(marker string, roots []string) (int, error) {
		if marker == "m m m" {
			return 2, nil
		}
		return 2, nil
	}
	hub := fakeCounter{counts: map[string]int{
		"m m m": 3, "m m m/settled": 3,
		"n n n": 2, "n n n/settled": 1,
	}}
	if err := recalibrateWith(context.Background(), hub, []string{path}, nil); err != nil {
		t.Fatal(err)
	}
	var qs []Question
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &qs); err != nil {
		t.Fatal(err)
	}
	byKey := map[string]Question{}
	for _, q := range qs {
		byKey[q.Key] = q
	}
	d := byKey["drift"]
	if d.ExpectedSessions != 3 || d.ExpectedSettled != 3 || d.ExpectedFiles != 2 {
		t.Errorf("drift not recalibrated: %+v", d)
	}
	s := byKey["stable"]
	if s.ExpectedSessions != 2 || s.ExpectedSettled != 1 || s.ExpectedFiles != 2 {
		t.Errorf("stable question changed: %+v", s)
	}
}

func TestRecalibrateRefusesCollidedNegative(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "curated.json")
	original := `[{"key":"neg","tier":"negative","negative":true,"prompt":"p","evidence_marker":"z z z"}]`
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	oldFiles := filesWithText
	defer func() { filesWithText = oldFiles }()
	filesWithText = func(string, []string) (int, error) { return 0, nil }
	hub := fakeCounter{counts: map[string]int{"z z z": 1, "z z z/settled": 1}}
	err := recalibrateWith(context.Background(), hub, []string{path}, nil)
	if err == nil || !strings.Contains(err.Error(), "regenerate") {
		t.Fatalf("want collision refusal, got %v", err)
	}
}

func TestPreflightRowClassification(t *testing.T) {
	cases := []struct {
		name             string
		row              preflightRow
		ok, waitableWant bool
	}{
		{"exact match", preflightRow{total: 2, settled: 1, files: 2, wantSessions: 2, wantSettled: 1, wantFiles: 2}, true, false},
		{"settle gate pending", preflightRow{total: 2, settled: 0, files: 2, wantSessions: 2, wantSettled: 1, wantFiles: 2}, false, true},
		{"ingest lag", preflightRow{total: 1, settled: 1, files: 2, wantSessions: 2, wantSettled: 2, wantFiles: 2}, false, true},
		{"collision over count", preflightRow{total: 3, settled: 3, files: 3, wantSessions: 2, wantSettled: 2, wantFiles: 3}, false, false},
		{"local file missing", preflightRow{total: 2, settled: 2, files: 1, wantSessions: 2, wantSettled: 2, wantFiles: 2}, false, false},
	}
	for _, tc := range cases {
		if got := tc.row.ok(); got != tc.ok {
			t.Errorf("%s: ok=%v want %v", tc.name, got, tc.ok)
		}
		if got := tc.row.waitable(); got != tc.waitableWant {
			t.Errorf("%s: waitable=%v want %v", tc.name, got, tc.waitableWant)
		}
	}
}

func TestManifestHelpers(t *testing.T) {
	dir := t.TempDir()
	if err := writeJSON(filepath.Join(dir, "canary-facts.json"), map[string]any{"seed": 42}); err != nil {
		t.Fatal(err)
	}
	if got := canarySeedOf(dir); got != 42 {
		t.Fatalf("canarySeedOf: %d", got)
	}
	if got := canarySeedOf(t.TempDir()); got != 0 {
		t.Fatalf("canarySeedOf without facts: %d", got)
	}
	p := filepath.Join(dir, "q.json")
	if err := os.WriteFile(p, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	sum, err := fileSHA256(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum) != 64 {
		t.Fatalf("sha256 hex length: %d", len(sum))
	}
}

func TestWantSettledOf(t *testing.T) {
	if got := wantSettledOf(Question{ExpectedSessions: 3}); got != 3 {
		t.Fatalf("default: %d", got)
	}
	if got := wantSettledOf(Question{ExpectedSessions: 3, ExpectedSettled: 2}); got != 2 {
		t.Fatalf("explicit: %d", got)
	}
}
