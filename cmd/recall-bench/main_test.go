package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseClaudeStreamCountsUsageToolsAndBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stream.jsonl")
	lines := []string{
		`{"type":"system","subtype":"init","model":"test-model-1"}`,
		`{"type":"assistant","message":{"id":"msg_1","model":"test-model-1","content":[{"type":"text","text":"looking"}],"usage":{"input_tokens":100,"cache_creation_input_tokens":10,"cache_read_input_tokens":900,"output_tokens":5}}}`,
		`{"type":"assistant","message":{"id":"msg_1","model":"test-model-1","content":[{"type":"tool_use","name":"Grep","input":{}}],"usage":{"input_tokens":100,"cache_creation_input_tokens":10,"cache_read_input_tokens":900,"output_tokens":5}}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"found 3 matches, one two three"}]}}`,
		`{"type":"assistant","message":{"id":"msg_2","model":"test-model-1","content":[{"type":"tool_use","name":"mcp__ohya__sql","input":{}}],"usage":{"input_tokens":400,"cache_read_input_tokens":1000,"output_tokens":7}}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","is_error":true,"content":"query denied"}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":""}]}}`,
		`{"type":"result","subtype":"success","result":"The value is 40 milliseconds.\nevidence: 421337","duration_ms":5321,"num_turns":3,"model":"test-model-1","total_cost_usd":0.05,"usage":{"input_tokens":700,"cache_creation_input_tokens":0,"cache_read_input_tokens":2100,"output_tokens":20}}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := parseClaudeStream(path)
	if m.Answer != "The value is 40 milliseconds.\nevidence: 421337" || !m.OK {
		t.Fatalf("answer/ok: %q %v", m.Answer, m.OK)
	}
	if m.Model != "test-model-1" {
		t.Fatalf("model: %q", m.Model)
	}
	if m.InputTokens != 700 || m.OutputTokens != 20 || m.CacheReadTokens != 2100 {
		t.Fatalf("cumulative result usage must win: in=%d out=%d cache=%d", m.InputTokens, m.OutputTokens, m.CacheReadTokens)
	}
	if m.CostUSD != 0.05 {
		t.Fatalf("cost: %v", m.CostUSD)
	}
	if m.CacheReadTokens != 2100 || m.CacheNewTokens != 0 {
		t.Fatalf("cache usage after override: read=%d new=%d", m.CacheReadTokens, m.CacheNewTokens)
	}
	if len(m.ToolCalls) != 2 || m.ToolCalls[0] != "Grep" || m.ToolCalls[1] != "mcp__ohya__sql" {
		t.Fatalf("tool calls: %v", m.ToolCalls)
	}
	if m.ErrorResults != 1 || m.EmptyResults != 1 {
		t.Fatalf("error=%d empty=%d", m.ErrorResults, m.EmptyResults)
	}
	if m.ToolResultBytes != int64(len("found 3 matches, one two three")+len("query denied")) {
		t.Fatalf("tool result bytes: %d", m.ToolResultBytes)
	}
	if m.DurationMS != 5321 || m.NumTurns != 3 {
		t.Fatalf("duration/turns: %d %d", m.DurationMS, m.NumTurns)
	}
	if len(m.Problems) != 0 {
		t.Fatalf("problems: %v", m.Problems)
	}
}

func TestParseClaudeStreamDropsDeniedCalls(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stream.jsonl")
	lines := []string{
		`{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","id":"call_a","name":"Bash"},{"type":"tool_use","id":"call_b","name":"mcp__ohya__sql"}]}}`,
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","is_error":true,"content":"blocked"}]}}`,
		`{"type":"result","subtype":"success","result":"done","duration_ms":10,"num_turns":1,"permission_denials":[{"tool_name":"Bash","tool_use_id":"call_a"}]}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := parseClaudeStream(path)
	if len(m.ToolCalls) != 1 || m.ToolCalls[0] != "mcp__ohya__sql" {
		t.Fatalf("denied call must be dropped: %v", m.ToolCalls)
	}
	if m.DeniedCalls != 1 {
		t.Fatalf("denied count: %d", m.DeniedCalls)
	}
}

func TestParseClaudeStreamArrayToolResultContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stream.jsonl")
	lines := []string{
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":[{"type":"text","text":"alpha"},{"type":"text","text":"beta"}]}]}}`,
		`{"type":"user","message":{"role":"user","content":"plain typed prompt"}`,
		`{"type":"result","subtype":"success","result":"done","duration_ms":10,"num_turns":1}`,
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := parseClaudeStream(path)
	if m.ToolResultBytes != int64(len("alpha")+len("beta")+1) {
		t.Fatalf("block-array result bytes: %d", m.ToolResultBytes)
	}
	if m.EmptyResults != 0 {
		t.Fatalf("empty results: %d", m.EmptyResults)
	}
}

func TestValidateQuestionRules(t *testing.T) {
	cases := []struct {
		name string
		q    Question
		want string
	}{
		{"negative-with-answer", Question{Key: "x", Tier: "negative", Negative: true, Prompt: "p", AnswerContains: []string{"a"}}, "answer_contains"},
		{"negative-nonzero", Question{Key: "x", Tier: "negative", Negative: true, Prompt: "p", ExpectedSessions: 1}, "zero"},
		{"aggregate-no-count", Question{Key: "x", Tier: "aggregate", Prompt: "p"}, "count_value"},
		{"plain-no-marker", Question{Key: "x", Tier: "needle", Prompt: "p", AnswerContains: []string{"a"}, ExpectedSessions: 1}, "marker"},
		{"plain-no-sessions", Question{Key: "x", Tier: "needle", Prompt: "p", AnswerContains: []string{"a"}, EvidenceMarker: "m m m"}, "expected_sessions"},
		{"local-no-files", Question{Key: "x", Tier: "needle", Prompt: "p", AnswerContains: []string{"a"}, EvidenceMarker: "m m m", ExpectedSessions: 1}, "expected_files"},
		{"bad-tier", Question{Key: "x", Tier: "wild", Prompt: "p"}, "tier"},
		{"marker-quote", Question{Key: "x", Tier: "needle", Prompt: "p", AnswerContains: []string{"a"}, EvidenceMarker: "has \"quotes\"", ExpectedSessions: 1, ExpectedFiles: 1}, "marker"},
		{"ok", Question{Key: "x", Tier: "needle", Prompt: "p", AnswerContains: []string{"a"}, EvidenceMarker: "m m m", ExpectedSessions: 1, ExpectedFiles: 1}, ""},
		{"ok-remote", Question{Key: "x", Tier: "crossdevice", Scope: "remote-only", Prompt: "p", AnswerContains: []string{"a"}, EvidenceMarker: "m m m", ExpectedSessions: 1}, ""},
	}
	for _, tc := range cases {
		err := validateQuestion(tc.q)
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: unexpected error %v", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want error containing %q, got %v", tc.name, tc.want, err)
		}
	}
}

func TestLoadQuestionsRejectsDuplicateKeys(t *testing.T) {
	dir := t.TempDir()
	q := `[{"key":"k","tier":"needle","prompt":"p","answer_contains":["a"],"evidence_marker":"m m m","expected_sessions":1,"expected_files":1}]`
	p := filepath.Join(dir, "q.json")
	if err := os.WriteFile(p, []byte(q), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadQuestions([]string{p, p}); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("want duplicate key error, got %v", err)
	}
}

func TestArmPromptShapes(t *testing.T) {
	q := Question{Key: "k", Tier: "needle", Prompt: "Find the Zed-11 retry ceiling.", AnswerContains: []string{"40 milliseconds"}, EvidenceMarker: "Zed-11 retry ceiling"}
	roots := []string{"/r/claude", "/r/codex", "/r/pi"}
	raw := armPrompt(q, armRaw, roots)
	ohya := armPrompt(q, armOhya, roots)
	for _, want := range []string{"/r/claude", "/r/codex", "/r/pi", "absolute path", q.Prompt} {
		if !strings.Contains(raw, want) {
			t.Errorf("raw prompt missing %q", want)
		}
	}
	if !strings.Contains(raw, "evidence: <path>") && !strings.Contains(raw, "<path>") {
		t.Errorf("raw prompt lacks path evidence instruction")
	}
	for _, want := range []string{"mcp__ohya__sql", "events.id", q.Prompt} {
		if !strings.Contains(ohya, want) {
			t.Errorf("ohya prompt missing %q", want)
		}
	}
	for _, banned := range []string{"/r/claude", ".claude/projects"} {
		if strings.Contains(ohya, banned) {
			t.Errorf("ohya prompt must not name transcript roots, found %q", banned)
		}
	}
	neg := Question{Key: "n", Tier: "negative", Negative: true, Prompt: "Was there a Zed-12 plan?"}
	if !strings.Contains(armPrompt(neg, armOhya, roots), "evidence: none") {
		t.Errorf("negative prompt lacks the none instruction")
	}
	agg := Question{Key: "a", Tier: "aggregate", Prompt: "How many sessions mention Zed-11?", CountValue: 2}
	if !strings.Contains(armPrompt(agg, armRaw, roots), "count: N") {
		t.Errorf("aggregate prompt lacks count instruction")
	}
}

func TestNewCanaryPlanDeterministicAndDistinct(t *testing.T) {
	a := newCanaryPlan(42)
	b := newCanaryPlan(42)
	if a.code1 != b.code1 || a.v1 != b.v1 || a.v5New != b.v5New {
		t.Fatal("same seed produced different plans")
	}
	if a.v5Old == a.v5New {
		t.Fatalf("correction values must differ: %d", a.v5Old)
	}
	seen := map[string]bool{}
	for _, c := range []string{a.code1, a.code3, a.code5, a.code6, a.code8} {
		if seen[c] {
			t.Fatalf("duplicate codeword %q", c)
		}
		seen[c] = true
	}
	qs := a.questions()
	if len(qs) != 8 {
		t.Fatalf("want 8 canary questions, got %d", len(qs))
	}
	for _, q := range qs {
		if err := validateQuestion(q); err != nil {
			t.Errorf("%s: %v", q.Key, err)
		}
	}
	facts := a.sessionList()
	if len(facts) != 8 {
		t.Fatalf("want 8 planting sessions, got %d", len(facts))
	}
	var agents []string
	for _, s := range facts {
		switch s.Agent {
		case "claude", "codex", "pi":
		default:
			t.Fatalf("unknown planting agent %q", s.Agent)
		}
		agents = append(agents, s.Agent)
	}
	// The c5 correction session must come after the original decision.
	correctionAt, originalAt := -1, -1
	for i, s := range facts {
		if strings.Contains(s.Fact, "Correction to an earlier decision") {
			correctionAt = i
		}
		if strings.Contains(s.Fact, a.code5) && !strings.Contains(s.Fact, "Correction") && originalAt < 0 {
			originalAt = i
		}
	}
	if originalAt < 0 || correctionAt < originalAt {
		t.Fatalf("correction must follow the original: original=%d correction=%d", originalAt, correctionAt)
	}
}

func TestCanaryMarkersConfineToTheirSessions(t *testing.T) {
	a := newCanaryPlan(7)
	qs := a.questions()
	byKey := map[string]string{}
	for _, q := range qs {
		if q.EvidenceMarker != "" {
			byKey[q.Key] = q.EvidenceMarker
		}
	}
	facts := a.sessionList()
	allFacts := strings.Join(mapFacts(facts), "\n")
	for key, marker := range byKey {
		if !strings.Contains(allFacts, marker) {
			t.Errorf("%s marker %q not present in any planted fact", key, marker)
		}
	}
	// Each evidence marker must appear in exactly one planted fact: the
	// aggregate question rides the c1 codename, which appears in two.
	c1 := a.code1
	hits := 0
	for _, f := range mapFacts(facts) {
		if strings.Contains(f, c1) {
			hits++
		}
	}
	if hits != 2 {
		t.Errorf("c1 codename must appear in exactly 2 planted facts for the aggregate ground truth, got %d", hits)
	}
	c5 := byKey["c5-correction-claude"]
	hits = 0
	for _, f := range mapFacts(facts) {
		if strings.Contains(f, c5) {
			hits++
		}
	}
	if hits != 1 {
		t.Errorf("c5 correction marker must appear in exactly 1 planted fact, got %d", hits)
	}
}

func mapFacts(sessions []plantedSession) []string {
	out := make([]string, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, s.Fact)
	}
	return out
}

func TestGradeRunAuditRules(t *testing.T) {
	base := Question{
		Key: "k", Tier: "needle", Prompt: "p",
		AnswerContains: []string{"val"},
		EvidenceMarker: "marker words here", ExpectedSessions: 1, ExpectedFiles: 1,
	}
	needle := traceMetrics{Answer: "val\nmicrostate: none"}
	cases := []struct {
		name string
		arm  string
		m    traceMetrics
		want string
	}{
		{"meta-tool allowed on ohya arm", armOhya, traceMetrics{Answer: needle.Answer, ToolCalls: []string{"ToolSearch", "mcp__ohya__sql"}}, "no-citation"},
		{"read on ohya arm is invalid", armOhya, traceMetrics{Answer: needle.Answer, ToolCalls: []string{"Read"}}, "invalid"},
		{"bash on ohya arm is invalid", armOhya, traceMetrics{Answer: needle.Answer, ToolCalls: []string{"mcp__ohya__sql", "Bash"}}, "invalid"},
		{"mcp on raw arm is invalid", armRaw, traceMetrics{Answer: needle.Answer, ToolCalls: []string{"mcp__other__sql"}}, "invalid"},
		{"grep on raw arm is fine", armRaw, traceMetrics{Answer: needle.Answer, ToolCalls: []string{"Grep", "Bash"}}, "no-citation"},
	}
	for _, tc := range cases {
		got := gradeRun(context.Background(), nil, base, tc.arm, tc.m)
		if got != tc.want {
			t.Errorf("%s: want %q, got %q", tc.name, tc.want, got)
		}
	}
}

func TestGradeRunNegativePhrases(t *testing.T) {
	q := Question{Key: "n", Tier: "negative", Negative: true, Prompt: "p"}
	for _, answer := range []string{
		"I found no evidence of this decision.",
		"There is no record of that anywhere I searched.",
		"Nothing found matching the question.",
	} {
		if got := gradeRun(context.Background(), nil, q, armOhya, traceMetrics{Answer: answer}); got != "negative-correct" {
			t.Errorf("answer %q: want negative-correct, got %q", answer, got)
		}
	}
	if got := gradeRun(context.Background(), nil, q, armOhya, traceMetrics{Answer: "The value is 42."}); got != "negative-fabricated" {
		t.Errorf("fabricated negative: want negative-fabricated, got %q", got)
	}
}

func TestGradeRunAggregateCountLine(t *testing.T) {
	q := Question{Key: "a", Tier: "aggregate", Prompt: "p", CountValue: 2}
	if got := gradeRun(context.Background(), nil, q, armOhya, traceMetrics{Answer: "Two sessions.\ncount: 2"}); got != "correct" {
		t.Errorf("want correct, got %q", got)
	}
	if got := gradeRun(context.Background(), nil, q, armOhya, traceMetrics{Answer: "Three sessions.\ncount: 3"}); got != "wrong-answer" {
		t.Errorf("want wrong-answer, got %q", got)
	}
	if got := gradeRun(context.Background(), nil, q, armOhya, traceMetrics{Answer: "I found two."}); got != "no-citation" {
		t.Errorf("want no-citation, got %q", got)
	}
}

func TestMedianHelpers(t *testing.T) {
	if got := medianFloat([]float64{5, 1, 9}); got != 5 {
		t.Fatalf("medianFloat: %v", got)
	}
	if got := medianInt([]int64{10, 2}); got != "10" {
		t.Fatalf("medianInt even-length picks upper element: %v", got)
	}
	if got := medianInt(nil); got != "-" {
		t.Fatalf("medianInt empty: %v", got)
	}
}
