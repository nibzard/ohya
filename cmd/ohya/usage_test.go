package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nibzard/ohya/internal/codex"
	"github.com/nibzard/ohya/internal/device"
	"github.com/nibzard/ohya/internal/store"
	"github.com/nibzard/ohya/internal/watch"
)

const (
	usageSessionCalling = "21111111-1111-4111-8111-111111111111"
	usageSessionQuiet   = "21111111-1111-4111-8111-111111111112"
	usageCodexThread    = "21111111-1111-7111-8111-1111111111c1"
	usageUntil          = "2026-11-16T12:00:00Z"
)

const usageSecret = "SECRETRAVEN42"

const usageOKResult = `{"columns":["id"],"row_count":1,"rows":[[42]],"truncated":false}`
const usageTimeoutResult = "sql tool: hubsync: rejected: execution deadline exceeded (5s per call): context deadline exceeded"

func usageCallingSession() string {
	records := []string{
		`{"type":"user","uuid":"u1","parentUuid":null,"isSidechain":false,"userType":"external","cwd":"/home/dev/usage","sessionId":"` + usageSessionCalling + `","version":"2.1.280","gitBranch":"main","message":{"role":"user","content":"query memory"},"timestamp":"2026-11-15T22:30:00.000Z"}`,
		`{"type":"attachment","uuid":"u2","parentUuid":"u1","isSidechain":false,"userType":"external","cwd":"/home/dev/usage","sessionId":"` + usageSessionCalling + `","version":"2.1.280","attachment":{"type":"prompt_snapshot","snapshot":{"tools":[{"name":"mcp__ohya__sql"},{"name":"mcp__ohya__annotate"}]}},"timestamp":"2026-11-15T22:30:00.000Z"}`,
		`{"type":"assistant","uuid":"u3","parentUuid":"u2","isSidechain":false,"userType":"external","cwd":"/home/dev/usage","sessionId":"` + usageSessionCalling + `","version":"2.1.280","apiBlockIndex":0,"message":{"id":"msg_usage_0001","type":"message","role":"assistant","model":"fixture-model-v1","content":[{"type":"tool_use","id":"call_usage_ok","name":"mcp__ohya__sql","input":{"query":"SELECT id FROM events WHERE text LIKE '%` + usageSecret + `%'"}}],"usage":{"input_tokens":10,"output_tokens":2}},"timestamp":"2026-11-15T22:32:00.000Z"}`,
		fmt.Sprintf(`{"type":"user","uuid":"u4","parentUuid":"u3","isSidechain":false,"userType":"external","cwd":"/home/dev/usage","sessionId":"%s","version":"2.1.280","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_usage_ok","content":%s}]},"timestamp":"2026-11-15T22:32:02.000Z"}`, usageSessionCalling, jsonString(usageOKResult)),
		`{"type":"assistant","uuid":"u5","parentUuid":"u4","isSidechain":false,"userType":"external","cwd":"/home/dev/usage","sessionId":"` + usageSessionCalling + `","version":"2.1.280","apiBlockIndex":0,"message":{"id":"msg_usage_0002","type":"message","role":"assistant","model":"fixture-model-v1","content":[{"type":"tool_use","id":"call_usage_slow","name":"mcp__ohya__sql","input":{"query":"SELECT raw_json FROM events LIMIT 5"}}],"usage":{"input_tokens":10,"output_tokens":2}},"timestamp":"2026-11-15T22:33:00.000Z"}`,
		fmt.Sprintf(`{"type":"user","uuid":"u6","parentUuid":"u5","isSidechain":false,"userType":"external","cwd":"/home/dev/usage","sessionId":"%s","version":"2.1.280","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_usage_slow","is_error":true,"content":%s}]},"timestamp":"2026-11-15T22:33:05.027Z"}`, usageSessionCalling, jsonString(usageTimeoutResult)),
		`{"type":"assistant","uuid":"u7","parentUuid":"u6","isSidechain":false,"userType":"external","cwd":"/home/dev/usage","sessionId":"` + usageSessionCalling + `","version":"2.1.280","apiBlockIndex":0,"message":{"id":"msg_usage_0003","type":"message","role":"assistant","model":"fixture-model-v1","content":[{"type":"tool_use","id":"call_usage_tail","name":"mcp__ohya__sql","input":{"query":"SELECT count(*) FROM sessions"}}],"usage":{"input_tokens":10,"output_tokens":2}},"timestamp":"2026-11-15T22:34:00.000Z"}`,
	}
	return strings.Join(records, "\n") + "\n"
}

func jsonString(s string) string {
	encoded, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

func usageQuietSession() string {
	records := []string{
		`{"type":"user","uuid":"q1","parentUuid":null,"isSidechain":false,"userType":"external","cwd":"/home/dev/usage","sessionId":"` + usageSessionQuiet + `","version":"2.1.280","gitBranch":"main","message":{"role":"user","content":"the mcp__ohya__sql tool exists but never call it"},"timestamp":"2026-11-15T23:00:00.000Z"}`,
		`{"type":"attachment","uuid":"q2","parentUuid":"q1","isSidechain":false,"userType":"external","cwd":"/home/dev/usage","sessionId":"` + usageSessionQuiet + `","version":"2.1.280","attachment":{"type":"prompt_snapshot","snapshot":{"tools":[{"name":"mcp__ohya__sql"},{"name":"mcp__ohya__annotate"}]}},"timestamp":"2026-11-15T23:00:00.000Z"}`,
	}
	return strings.Join(records, "\n") + "\n"
}

func usageCodexRollout() string {
	itemCompleted := func(item map[string]any) map[string]any {
		return map[string]any{
			"type": "event_msg", "payload": map[string]any{
				"type": "item_completed", "thread_id": usageCodexThread, "item": item,
			},
		}
	}
	mcpCallEnd := func(invocation map[string]any, durationSecs, durationNanos int64, result map[string]any) map[string]any {
		return map[string]any{
			"type": "event_msg", "payload": map[string]any{
				"type": "mcp_tool_call_end", "call_id": "call_ann",
				"invocation": invocation,
				"duration":   map[string]any{"secs": durationSecs, "nanos": durationNanos},
				"result":     result,
			},
		}
	}
	records := []map[string]any{
		{"type": "session_meta", "payload": map[string]any{"id": usageCodexThread, "cwd": "/home/dev/usage"}},
		{"type": "response_item", "payload": map[string]any{
			"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": "check memory"}},
		}},
		itemCompleted(map[string]any{
			"type": "McpToolCall", "id": "exec-1", "server": "ohya", "tool": "sql",
			"arguments": map[string]any{"query": "SELECT agent, count(*) FROM sessions GROUP BY agent"},
			"status":    "completed",
			"result": map[string]any{"content": []any{map[string]any{
				"type": "text", "text": `{"columns":["agent"],"row_count":1,"rows":[["claude-code"]]}`,
			}}},
			"duration": map[string]any{"secs": 4, "nanos": 160371084},
		}),
	}
	records[len(records)-1]["payload"].(map[string]any)["started_at_ms"] = 1794782340117
	records[len(records)-1]["payload"].(map[string]any)["completed_at_ms"] = 1794782344277
	records = append(records,
		mcpCallEnd(map[string]any{"server": "ohya", "tool": "annotate", "arguments": map[string]any{
			"operations": []any{map[string]any{
				"op": "upsert", "event_id": 1, "namespace": "tags", "key": "reviewed",
				"value": map[string]any{"ok": true},
			}},
		}}, 0, 900000000, map[string]any{
			"Ok": map[string]any{"content": []any{map[string]any{"type": "text", "text": "applied 1 unchanged 0"}}},
		}),
		itemCompleted(map[string]any{
			"type": "McpToolCall", "id": "exec-2", "server": "cassandra", "tool": "cql_query",
			"arguments": map[string]any{}, "status": "completed",
			"result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "other server"}}},
		}),
		itemCompleted(map[string]any{
			"type": "McpToolCall", "id": "exec-3", "server": "ohya", "tool": "sql",
			"arguments": map[string]any{"query": "SELECT count(*) FROM events_settled"},
			"status":    "failed",
			"result": map[string]any{"content": []any{map[string]any{
				"type": "text", "text": usageTimeoutResult,
			}}},
			"duration": map[string]any{"secs": 5, "nanos": 334000000},
		}),
	)
	timestamps := []string{
		"2026-11-15T22:39:00.000Z", "2026-11-15T22:39:01.000Z", "2026-11-15T22:39:04.278Z",
		"2026-11-15T22:39:05.000Z", "2026-11-15T22:39:06.000Z", "2026-11-15T22:39:12.000Z",
	}
	var lines []string
	for i, record := range records {
		if i >= 1 {
			record["ordinal"] = i
		}
		record["timestamp"] = timestamps[i]
		encoded, err := json.Marshal(record)
		if err != nil {
			panic(err)
		}
		lines = append(lines, string(encoded))
	}
	return strings.Join(lines, "\n") + "\n"
}

func ingestUsageRoot(t *testing.T, dbPath, stateDir, agent string, files map[string]string) {
	t.Helper()
	root := filepath.Join(filepath.Dir(dbPath), "roots", agent+"-"+filepath.Base(stateDir))
	for name, content := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		writeFile(t, path, content)
	}
	hub, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	deviceID, err := device.LoadOrCreate(stateDir)
	if err != nil {
		hub.Close()
		t.Fatal(err)
	}
	w, err := watch.New(stateDir, agent, watch.Options{})
	if err != nil {
		hub.Close()
		t.Fatal(err)
	}
	r := newIngestor(hub, w, agent, deviceID, 0, nil, io.Discard, nil, nil)
	stats, err := r.poll(context.Background(), []agentDir{{dir: root}}, true)
	hub.Close()
	if err != nil {
		t.Fatalf("ingest %s: %v", agent, err)
	}
	if stats.failed > 0 || stats.stopped > 0 {
		t.Fatalf("ingest %s degraded: %+v", agent, stats)
	}
}

func buildUsageHub(t *testing.T) (dbPath, stateDir string) {
	t.Helper()
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "hub", "hub.db")
	stateDir = filepath.Join(dir, "state")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	secondState := filepath.Join(dir, "state-two")
	ingestUsageRoot(t, dbPath, stateDir, "claude-code", map[string]string{
		"calling-session.jsonl": usageCallingSession(),
		"quiet-session.jsonl":   usageQuietSession(),
	})
	ingestUsageRoot(t, dbPath, secondState, codex.AgentName, map[string]string{
		"rollout-2026-11-15T22-39-00-" + usageCodexThread + ".jsonl": usageCodexRollout(),
	})
	return dbPath, stateDir
}

func runUsage(t *testing.T, dbPath, stateDir string, extra ...string) (string, string, int) {
	t.Helper()
	args := append([]string{"--db", dbPath, "--state", stateDir, "--until", usageUntil}, extra...)
	var stdout, stderr bytes.Buffer
	code := cliMain(append([]string{"usage"}, args...), &stdout, &stderr)
	return stdout.String(), stderr.String(), code
}

func decodeUsageJSON(t *testing.T, output string) usageReport {
	t.Helper()
	var envelope struct {
		SchemaVersion int              `json:"schema_version"`
		Command       string           `json:"command"`
		Data          usageReport      `json:"data"`
		Error         *json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal([]byte(output), &envelope); err != nil {
		t.Fatalf("decode envelope: %v\n%s", err, output)
	}
	if envelope.Error != nil {
		t.Fatalf("envelope error: %s", *envelope.Error)
	}
	if envelope.Command != "ohya usage" {
		t.Fatalf("command = %q", envelope.Command)
	}
	return envelope.Data
}

func agentToolRow(t *testing.T, report usageReport, agent, tool string) usageAgentTool {
	t.Helper()
	for _, row := range report.Transcript.ByAgentTool {
		if row.Agent == agent && row.Tool == tool {
			return row
		}
	}
	t.Fatalf("no by_agent_tool row for %s/%s in %+v", agent, tool, report.Transcript.ByAgentTool)
	return usageAgentTool{}
}

func outcomeCalls(t *testing.T, report usageReport, tool, outcome string) int64 {
	t.Helper()
	for _, row := range report.Transcript.Outcomes {
		if row.Tool == tool && row.Key == outcome {
			return row.Calls
		}
	}
	return 0
}

func TestUsageReportTranscriptEvidence(t *testing.T) {
	dbPath, stateDir := buildUsageHub(t)

	stdout, _, code := runUsage(t, dbPath, stateDir, "--json")
	if code != 0 {
		t.Fatalf("exit %d, stdout:\n%s", code, stdout)
	}
	report := decodeUsageJSON(t, stdout)

	if report.Partial {
		t.Fatalf("unexpected partial report: %+v", report.Notes)
	}
	if report.Transcript.Calls != 6 {
		t.Fatalf("calls = %d, want 3 claude-code sql + 2 codex sql + 1 codex annotate", report.Transcript.Calls)
	}
	claude := agentToolRow(t, report, "claude-code", "sql")
	if claude.Calls != 3 || claude.Sessions != 1 || claude.Devices != 1 {
		t.Fatalf("claude-code/sql row = %+v", claude)
	}
	codexSQL := agentToolRow(t, report, "codex", "sql")
	if codexSQL.Calls != 2 || codexSQL.Sessions != 1 {
		t.Fatalf("codex/sql row = %+v", codexSQL)
	}
	codexAnnotate := agentToolRow(t, report, "codex", "annotate")
	if codexAnnotate.Calls != 1 {
		t.Fatalf("codex/annotate row = %+v", codexAnnotate)
	}

	if got := outcomeCalls(t, report, "sql", "ok"); got != 2 {
		t.Fatalf("sql ok outcomes = %d, want 2", got)
	}
	if got := outcomeCalls(t, report, "sql", "timeout"); got != 2 {
		t.Fatalf("sql timeout outcomes = %d, want 2 (claude-code is_error and codex failed)", got)
	}
	if got := outcomeCalls(t, report, "sql", "no_result"); got != 1 {
		t.Fatalf("sql no_result outcomes = %d, want 1", got)
	}
	if got := outcomeCalls(t, report, "annotate", "ok"); got != 1 {
		t.Fatalf("annotate ok outcomes = %d, want 1", got)
	}

	if len(report.Transcript.LatencyMS) != 2 {
		t.Fatalf("latency rows = %+v", report.Transcript.LatencyMS)
	}
	for _, stat := range report.Transcript.LatencyMS {
		switch stat.Tool {
		case "sql":
			if stat.N != 4 {
				t.Fatalf("sql latency n = %d, want 4 (2000, 4160, 5027, 5334)", stat.N)
			}
			if stat.P50 != 4160 || stat.P95 != 5334 || stat.Max != 5334 {
				t.Fatalf("sql latency = %+v, want p50 4160 p95 5334 max 5334", stat)
			}
		case "annotate":
			if stat.N != 1 || stat.Max != 900 {
				t.Fatalf("annotate latency = %+v, want n 1 max 900", stat)
			}
		}
	}

	for _, stat := range report.Transcript.ResultBytes {
		if stat.Tool == "sql" {
			wantMax := int64(len(usageTimeoutResult))
			if stat.N != 4 || stat.MaxBytes != wantMax {
				t.Fatalf("sql result bytes = %+v, want n 4 max %d", stat, wantMax)
			}
		}
	}

	if report.Transcript.RawJSONCalls != 1 {
		t.Fatalf("raw_json calls = %d, want 1 (the slow query names raw_json)", report.Transcript.RawJSONCalls)
	}
	availability := report.Transcript.ToolsAvailable
	if availability.Agent != "claude-code" ||
		availability.AvailableSessions != 2 ||
		availability.AvailableUnusedSessions != 1 {
		t.Fatalf("availability = %+v, want 2 available sessions and 1 unused", availability)
	}
	if report.Transcript.SessionsWithCalls != 2 {
		t.Fatalf("sessions with calls = %d, want the calling claude session and the codex thread", report.Transcript.SessionsWithCalls)
	}

	week := isoWeek(1794784320000)
	var weekRows int
	for _, row := range report.Transcript.ByAgentDeviceWeek {
		if row.Week == "" {
			t.Fatalf("week missing on row %+v", row)
		}
		weekRows += int(row.Calls)
	}
	if weekRows != 6 {
		t.Fatalf("by_agent_device_week calls sum = %d, want 6", weekRows)
	}
	if week == "" {
		t.Fatal("isoWeek helper returned an empty label")
	}
}

func TestUsageCodexMCPFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "codex", "mcp-tool-call.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "hub", "hub.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ingestUsageRoot(t, dbPath, filepath.Join(dir, "device-state"), codex.AgentName, map[string]string{
		"rollout-2026-11-16T10-00-01-00000000-0000-7000-8000-0000000000e1.jsonl": string(raw),
	})

	stdout, _, code := runUsage(t, dbPath, stateDir, "--json", "--local-detail")
	if code != 0 {
		t.Fatalf("exit %d, stdout:\n%s", code, stdout)
	}
	report := decodeUsageJSON(t, stdout)
	if report.Transcript.Calls != 1 {
		t.Fatalf("calls = %d, want the one fixture McpToolCall", report.Transcript.Calls)
	}
	row := agentToolRow(t, report, "codex", "sql")
	if row.Calls != 1 || row.Sessions != 1 || row.Devices != 1 {
		t.Fatalf("codex/sql row = %+v", row)
	}
	if got := outcomeCalls(t, report, "sql", "ok"); got != 1 {
		t.Fatalf("sql ok outcomes = %d", got)
	}
	resultText := `{"columns":["n"],"row_count":1,"rows":[[3]],"truncated":false}`
	if len(report.Transcript.Detail) != 1 {
		t.Fatalf("detail = %+v", report.Transcript.Detail)
	}
	detail := report.Transcript.Detail[0]
	if detail.Agent != "codex" || detail.Tool != "sql" {
		t.Fatalf("detail = %+v", detail)
	}
	if detail.LatencyMS == nil || *detail.LatencyMS != 28 {
		t.Fatalf("latency = %v, want completed_at_ms - started_at_ms = 28", detail.LatencyMS)
	}
	if detail.ResultBytes == nil || *detail.ResultBytes != int64(len(resultText)) {
		t.Fatalf("result bytes = %v, want %d", detail.ResultBytes, len(resultText))
	}
	if detail.QueryText != "SELECT count(*) AS n FROM sessions" {
		t.Fatalf("query text = %q", detail.QueryText)
	}
}

func TestUsageReportDeterministicText(t *testing.T) {
	dbPath, stateDir := buildUsageHub(t)

	first, _, code := runUsage(t, dbPath, stateDir)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	second, _, code := runUsage(t, dbPath, stateDir)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if first != second {
		t.Fatalf("two runs differ:\n%s\n---\n%s", first, second)
	}
	if !strings.Contains(first, "no sql or annotate calls found") &&
		!strings.Contains(first, "claude-code") {
		t.Fatalf("text report misses the agent summary:\n%s", first)
	}
}

func TestUsageReportHidesTranscriptContentByDefault(t *testing.T) {
	dbPath, stateDir := buildUsageHub(t)

	text, _, code := runUsage(t, dbPath, stateDir)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.Contains(text, usageSecret) {
		t.Fatalf("default text output leaks the seeded secret:\n%s", text)
	}
	asJSON, _, code := runUsage(t, dbPath, stateDir, "--json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.Contains(asJSON, usageSecret) {
		t.Fatal("default JSON output leaks the seeded secret")
	}

	detail, _, code := runUsage(t, dbPath, stateDir, "--local-detail")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(detail, usageSecret) {
		t.Fatalf("--local-detail omits the local query text:\n%s", detail)
	}
	if strings.Contains(detail, usageOKResult) {
		t.Fatalf("--local-detail must never print result text:\n%s", detail)
	}
}

func TestUsageReportServerRecords(t *testing.T) {
	dbPath, stateDir := buildUsageHub(t)
	instanceDir := filepath.Join(stateDir, "observe", "0123456789abcdef0123456789abcdef")
	if err := os.MkdirAll(instanceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	lines := []string{
		usageServerLine(t, 1794786000000, "sql", "ok", 100, 2048, []string{"events.id", "events.raw_json"}, []string{"count"}, false, nil),
		usageServerLine(t, 1794786100000, "sql", "timeout", 0, 0, []string{"events.text"}, nil, true, nil),
	}
	if err := os.WriteFile(filepath.Join(instanceDir, "usage.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	outOfWindow := usageServerLine(t, 1500000000000, "sql", "ok", 1, 2, nil, nil, false, nil)
	if err := os.WriteFile(filepath.Join(instanceDir, "usage.jsonl.1"), []byte(outOfWindow+"\nnot json at all\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, _, code := runUsage(t, dbPath, stateDir, "--json")
	if code != 0 {
		t.Fatalf("exit %d, stdout:\n%s", code, stdout)
	}
	report := decodeUsageJSON(t, stdout)
	server := report.Server
	if !server.Found || server.Records != 2 || server.Instances != 1 {
		t.Fatalf("server section = %+v, want 2 records from 1 instance", server)
	}
	if server.MalformedRecords != 1 {
		t.Fatalf("malformed records = %d, want 1", server.MalformedRecords)
	}
	if server.Rows != 100 || server.ResultBytesMax != 2048 || server.FTSCalls != 1 {
		t.Fatalf("server aggregates = rows %d bytes %d fts %d", server.Rows, server.ResultBytesMax, server.FTSCalls)
	}
	if server.RawJSONCalls != 1 {
		t.Fatalf("server raw_json calls = %d, want 1", server.RawJSONCalls)
	}
	var readOK bool
	for _, read := range server.Reads {
		if read.Key == "events.raw_json" && read.Calls == 1 {
			readOK = true
		}
	}
	if !readOK {
		t.Fatalf("reads = %+v, want events.raw_json 1", server.Reads)
	}
	found := map[string]int64{}
	for _, row := range server.ByToolClientWeek {
		key := row.Tool + "/" + row.Client + "/" + row.Path + "/" + row.Week
		found[key] += row.Calls
	}
	if found["sql/claude-code/local/"+isoWeek(1794786000000)] != 2 {
		t.Fatalf("by_tool_client_week = %+v", server.ByToolClientWeek)
	}
}

func TestUsageReportEmptyHub(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "hub.db")
	hub, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	hub.Close()

	stdout, _, code := runUsage(t, dbPath, dir, "--json")
	if code != 0 {
		t.Fatalf("exit %d, stdout:\n%s", code, stdout)
	}
	report := decodeUsageJSON(t, stdout)
	if report.Transcript.Calls != 0 || len(report.Transcript.ByAgentTool) != 0 {
		t.Fatalf("transcript section = %+v, want no calls", report.Transcript)
	}
	if report.Server.Found {
		t.Fatalf("server section = %+v, want none found", report.Server)
	}
}

func TestUsageWindowAndFlagErrors(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "hub.db")
	hub, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	hub.Close()

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--db", dbPath, "--state", dir, "--until", usageUntil, "--since", "abc"}, "--since"},
		{[]string{"--db", dbPath, "--state", dir, "--until", "yesterday"}, "--until"},
		{[]string{"--db", dbPath, "--state", dir, "--deadline", "0s"}, "--deadline"},
	} {
		var stdout, stderr bytes.Buffer
		code := cliMain(append([]string{"usage"}, tc.args...), &stdout, &stderr)
		if code != 2 {
			t.Fatalf("args %v: exit %d, stderr %q", tc.args, code, stderr.String())
		}
		if !strings.Contains(stderr.String(), tc.want) || !strings.Contains(stderr.String(), "ohya usage --help") {
			t.Fatalf("args %v: stderr %q, want %q with help pointer", tc.args, stderr.String(), tc.want)
		}
	}
}

func TestUsageMissingDatabaseFails(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := cliMain([]string{"usage", "--db", filepath.Join(dir, "absent.db"), "--state", dir, "--until", usageUntil}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want no report", stdout.String())
	}
}

func TestParseUsageSince(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"30d", 30 * 24 * time.Hour, true},
		{"7d", 7 * 24 * time.Hour, true},
		{"48h", 48 * time.Hour, true},
		{"90m", 90 * time.Minute, true},
		{"0d", 0, false},
		{"-5d", 0, false},
		{"dd", 0, false},
		{"", 0, false},
		{"-1h", 0, false},
	} {
		got, err := parseUsageSince(tc.in)
		if tc.ok {
			if err != nil || got != tc.want {
				t.Fatalf("parseUsageSince(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
			}
		} else if err == nil {
			t.Fatalf("parseUsageSince(%q) = %v; want an error", tc.in, got)
		}
	}
}

func usageServerLine(t *testing.T, ms int64, tool, outcome string, rows, resultBytes int64, reads, functions []string, fts bool, annotateOps map[string]int64) string {
	t.Helper()
	record := map[string]any{
		"schema_version": 1,
		"time_bucket":    fmt.Sprintf("%s", time.UnixMilli(ms).UTC().Truncate(time.Hour).Format("2006-01-02T15:00Z")),
		"time_ms":        ms,
		"instance_id":    "0123456789abcdef0123456789abcdef",
		"tool":           tool,
		"client":         "claude-code",
		"client_version": "2.1",
		"path":           "local",
		"device_id":      "dev",
		"session_hint":   "",
		"outcome":        outcome,
		"sqlite_code":    0,
		"phase_ms":       map[string]any{"admission_wait_ms": 0.1, "reader_wait_ms": 0.1, "first_row_ms": 3.2, "iteration_ms": 1.0, "total_ms": 4.4},
		"rows":           rows,
		"result_bytes":   resultBytes,
		"reads":          reads,
		"functions":      functions,
		"fts":            fts,
	}
	if annotateOps != nil {
		record["annotate_ops"] = annotateOps
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
