package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCitedEventID(t *testing.T) {
	for _, tc := range []struct {
		answer string
		want   int64
		found  bool
	}{
		{"The cap is 30 seconds.\nevidence: 42", 42, true},
		{"evidence: 7 ... later corrected, evidence: 9", 9, true},
		{"Evidence: #12", 12, true},
		{"the decision is final", 0, false},
		{"no digits here evidence: none", 0, false},
	} {
		id, found := citedEventID(tc.answer)
		if found != tc.found || id != tc.want {
			t.Errorf("citedEventID(%q) = (%d, %v), want (%d, %v)", tc.answer, id, found, tc.want, tc.found)
		}
	}
}

func TestParseClaudeTrace(t *testing.T) {
	path := writeTemp(t, "stream.jsonl", strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"s1"}`,
		`{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","id":"t1","name":"mcp__ohya__sql","input":{"query":"SELECT 1"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1"}]}}`,
		`{"type":"assistant","message":{"id":"m2","content":[{"type":"text","text":"The answer."}]}}`,
		`{"type":"result","subtype":"success","result":"The answer. evidence: 5","is_error":false}`,
	}, "\n")+"\n")
	answer, ok, calls, problems := parseClaudeTrace(path)
	if !ok || answer != "The answer. evidence: 5" {
		t.Fatalf("ok=%v answer=%q", ok, answer)
	}
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	if len(calls) != 1 || calls[0] != "mcp__ohya__sql" {
		t.Fatalf("calls: %v", calls)
	}
}

func TestParseClaudeTraceMissingResult(t *testing.T) {
	path := writeTemp(t, "stream.jsonl",
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"mcp__ohya__sql"}]}}`+"\n")
	_, ok, calls, problems := parseClaudeTrace(path)
	if ok {
		t.Fatal("ok without a result event")
	}
	if len(problems) == 0 {
		t.Fatal("expected a problem for the missing result event")
	}
	if len(calls) != 1 {
		t.Fatalf("calls: %v", calls)
	}
}

func TestParseClaudeTraceErrorResult(t *testing.T) {
	path := writeTemp(t, "stream.jsonl",
		`{"type":"result","subtype":"error_during_execution","result":"boom"}`+"\n")
	_, ok, _, problems := parseClaudeTrace(path)
	if ok {
		t.Fatal("ok on error subtype")
	}
	if len(problems) == 0 {
		t.Fatal("expected a problem naming the error subtype")
	}
}

func TestParseCodexTrace(t *testing.T) {
	path := writeTemp(t, "events.jsonl", strings.Join([]string{
		`{"type":"thread.started","thread_id":"t1"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.completed","item":{"type":"mcp_tool_call","server":"ohya","tool":"sql","status":"completed"}}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"Working on it."}}`,
		`{"type":"item.completed","item":{"type":"error","message":"Falling back from WebSockets to HTTPS transport."}}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"The bound is 8 MiB. evidence: 12"}}`,
		`{"type":"turn.completed"}`,
	}, "\n")+"\n")
	msg, turnDone, calls, problems := parseCodexTrace(path)
	if !turnDone || msg != "The bound is 8 MiB. evidence: 12" {
		t.Fatalf("turnDone=%v msg=%q", turnDone, msg)
	}
	if len(calls) != 1 || calls[0] != "ohya.sql" {
		t.Fatalf("calls: %v", calls)
	}
	// The websocket-fallback error item is recorded, not fatal.
	if len(problems) != 1 || !strings.Contains(problems[0], "WebSockets") {
		t.Fatalf("problems: %v", problems)
	}
}

func TestParseCodexTraceNoTurn(t *testing.T) {
	path := writeTemp(t, "events.jsonl",
		`{"type":"item.completed","item":{"type":"agent_message","text":"partial"}}`+"\n")
	msg, turnDone, _, _ := parseCodexTrace(path)
	if turnDone {
		t.Fatal("turnDone without a turn.completed event")
	}
	if msg != "partial" {
		t.Fatalf("msg=%q", msg)
	}
}

// fakeResolve returns canned event text for the checkCase tests.
func fakeResolve(text string, found bool) resolveFunc {
	return func(context.Context, int64) (string, bool, error) { return text, found, nil }
}

func TestCheckCase(t *testing.T) {
	decision := caseDef{
		key:            "explicit-decision",
		answerContains: []string{"30 seconds"},
		evidenceMarker: backoffMarker,
		requireSQL:     true,
	}
	goodRun := &runResult{
		ok:        true,
		answer:    "The cap is 30 seconds, with jitter.\nevidence: 2",
		toolCalls: []string{"mcp__ohya__sql"},
	}
	if problems := checkCase(decision, goodRun, fakeResolve("Decision recorded: the ohya device sync retry "+backoffMarker+", with jitter.", true)); len(problems) != 0 {
		t.Errorf("good run problems: %v", problems)
	}

	noQuery := &runResult{ok: true, answer: "The cap is 30 seconds.\nevidence: 2"}
	if problems := checkCase(decision, noQuery, fakeResolve("the ohya device sync retry "+backoffMarker, true)); len(problems) != 1 || !strings.Contains(problems[0], "never queried memory") {
		t.Errorf("no-query problems: %v", problems)
	}

	staleEvidence := &runResult{
		ok:        true,
		answer:    "The cap is 30 seconds.\nevidence: 3",
		toolCalls: []string{"mcp__ohya__sql"},
	}
	if problems := checkCase(decision, staleEvidence, fakeResolve("an unrelated event", true)); len(problems) != 1 || !strings.Contains(problems[0], "does not carry the expected evidence") {
		t.Errorf("stale-evidence problems: %v", problems)
	}

	unresolvable := &runResult{
		ok:        true,
		answer:    "The cap is 30 seconds.\nevidence: 99",
		toolCalls: []string{"mcp__ohya__sql"},
	}
	if problems := checkCase(decision, unresolvable, fakeResolve("", false)); len(problems) != 1 || !strings.Contains(problems[0], "does not resolve") {
		t.Errorf("unresolvable problems: %v", problems)
	}

	currentContext := gateCases[2]
	leak := &runResult{ok: true, answer: "The codename is " + releaseCodename + ".", toolCalls: []string{"ohya.sql"}}
	if problems := checkCase(currentContext, leak, fakeResolve("", false)); len(problems) != 1 || !strings.Contains(problems[0], "queried memory despite") {
		t.Errorf("leak problems: %v", problems)
	}
	clean := &runResult{ok: true, answer: "The codename is " + releaseCodename + "."}
	if problems := checkCase(currentContext, clean, fakeResolve("", false)); len(problems) != 0 {
		t.Errorf("clean current-context problems: %v", problems)
	}
}

func TestSeedFixtures(t *testing.T) {
	for _, fixture := range []struct {
		name, text string
		markers    []string
	}{
		{"claude", claudeFixture(), []string{backoffMarker, "retry backoff cap"}},
		{"codex", codexFixture(), []string{staleMarker, batchMarker}},
	} {
		lines := strings.Split(strings.TrimRight(fixture.text, "\n"), "\n")
		if len(lines) < 4 {
			t.Fatalf("%s fixture has %d lines", fixture.name, len(lines))
		}
		var lastOrdinal = -1
		for i, line := range lines {
			var v map[string]any
			if err := json.Unmarshal([]byte(line), &v); err != nil {
				t.Fatalf("%s fixture line %d is not valid JSON: %v", fixture.name, i+1, err)
			}
			if ord, ok := v["ordinal"].(float64); ok {
				if int(ord) <= lastOrdinal {
					t.Fatalf("%s fixture ordinals not increasing at line %d", fixture.name, i+1)
				}
				lastOrdinal = int(ord)
			}
		}
		for _, marker := range fixture.markers {
			if !strings.Contains(fixture.text, marker) {
				t.Errorf("%s fixture lacks marker %q", fixture.name, marker)
			}
		}
	}

	// The correction marker must not appear in the stale decision text and
	// the stale marker must not appear in the correction text, so the
	// evidence check can tell them apart.
	codex := codexFixture()
	lines := strings.Split(strings.TrimRight(codex, "\n"), "\n")
	var stale, correction string
	for _, line := range lines {
		if strings.Contains(line, staleMarker) {
			stale = line
		}
		if strings.Contains(line, batchMarker) {
			correction = line
		}
	}
	if strings.Contains(stale, batchMarker) || strings.Contains(correction, staleMarker) {
		t.Fatal("stale decision and correction markers are not disjoint")
	}

	// The release codename must appear nowhere in the seeded history; the
	// current-context case must be answerable only from the prompt.
	for _, fixture := range []string{claudeFixture(), codexFixture()} {
		if strings.Contains(fixture, releaseCodename) {
			t.Fatal("release codename leaked into a seeded fixture")
		}
	}
}

func TestWriteSeedFixtures(t *testing.T) {
	root := t.TempDir()
	claudeRoot, codexRoot := filepath.Join(root, "claude"), filepath.Join(root, "codex")
	if err := writeSeedFixtures(claudeRoot, codexRoot); err != nil {
		t.Fatal(err)
	}
	claudePath := filepath.Join(claudeRoot, "-home-dev-ohya-e2e", claudeSessionID+".jsonl")
	if _, err := os.Stat(claudePath); err != nil {
		t.Fatalf("claude fixture: %v", err)
	}
	codexPath := filepath.Join(codexRoot, "2026", "11", "12",
		"rollout-2026-11-12T10-00-00-"+codexThreadID+".jsonl")
	if _, err := os.Stat(codexPath); err != nil {
		t.Fatalf("codex fixture: %v", err)
	}
}
