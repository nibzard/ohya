package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	claudeSessionID = "1e2e1111-1111-4111-8111-1111111111e1"
	codexThreadID   = "2e2e2222-2222-4222-8222-2222222222e2"

	backoffMarker = "backoff cap is 30 seconds"
	staleMarker   = "bounded at 4 MiB"
	batchMarker   = "batch byte bound is 8 MiB"

	releaseCodename = "Marlow-Feather"
)

var seedMarkers = []string{backoffMarker, staleMarker, batchMarker}

var gateCases = []caseDef{
	{
		key: "explicit-decision",
		prompt: "You are verifying ohya, a shared memory for coding agents, exposed as the sql tool " +
			"of the ohya MCP server; the database holds agent transcripts and full-text search over " +
			"events works well. Using that tool, find the decision recorded in an earlier session " +
			"about the ohya device-sync retry backoff cap, and report the cap value and whether " +
			"jitter is part of the decision. End your reply with one line of exactly the form " +
			"'evidence: <event-id>' where <event-id> is the numeric id (events.id) of the event " +
			"that contains the decision.",
		answerContains: []string{"30 seconds", "jitter"},
		evidenceMarker: backoffMarker,
		requireSQL:     true,
	},
	{
		key: "later-correction",
		prompt: "You are verifying ohya, a shared memory for coding agents, exposed as the sql tool " +
			"of the ohya MCP server. A decision about the ingestion batch byte bound was recorded " +
			"in an earlier session and later corrected. Using the sql tool, report the current " +
			"bound after the correction; state the current value only. End your reply with one " +
			"line of exactly the form 'evidence: <event-id>' where <event-id> is the numeric id " +
			"(events.id) of the event that carries the correction.",
		answerContains: []string{"8 MiB"},
		evidenceMarker: batchMarker,
		requireSQL:     true,
	},
	{
		key: "current-context",
		prompt: "Answer from this conversation only; do not call any tools and do not query the " +
			"ohya memory. The release codename for this verification run is " + releaseCodename + ". " +
			"Question: what is the release codename for this verification run? Reply with the " +
			"codename.",
		answerContains: []string{releaseCodename},
		forbidSQL:      true,
	},
}

type caseDef struct {
	key            string
	prompt         string
	answerContains []string
	evidenceMarker string
	requireSQL     bool
	forbidSQL      bool
}

func claudeFixture() string {
	var b strings.Builder
	p := func(uuid, parent, format string, args ...any) {
		fmt.Fprintf(&b, `{"uuid":"%s","parentUuid":%s,"isSidechain":false,"userType":"external","cwd":"/home/dev/ohya-e2e","sessionId":"%s","version":"2.1.280","gitBranch":"main",`, uuid, parent, claudeSessionID)
		fmt.Fprintf(&b, format, args...)
		b.WriteString("\n")
	}
	p("3e2e0001-0000-4000-8000-0000000000e1", "null",
		`"type":"user","message":{"role":"user","content":"Let's record the device-sync decisions for ohya."},"timestamp":"2026-11-12T09:00:01.000Z"}`)
	p("3e2e0002-0000-4000-8000-0000000000e2", `"3e2e0001-0000-4000-8000-0000000000e1"`,
		`"type":"assistant","apiBlockIndex":0,"message":{"id":"msg_e2e_0001","type":"message","role":"assistant","model":"fixture-model-v1","content":[{"type":"text","text":"Decision recorded: the ohya device sync retry backoff cap is 30 seconds, with jitter, applied per source."}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":100,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":20,"service_tier":"standard"}},"timestamp":"2026-11-12T09:00:06.000Z"}`)
	p("3e2e0003-0000-4000-8000-0000000000e3", `"3e2e0002-0000-4000-8000-0000000000e2"`,
		`"type":"user","message":{"role":"user","content":"One more thing: how does search rank?"},"timestamp":"2026-11-12T09:01:00.000Z"}`)
	p("3e2e0004-0000-4000-8000-0000000000e4", `"3e2e0003-0000-4000-8000-0000000000e3"`,
		`"type":"assistant","apiBlockIndex":0,"message":{"id":"msg_e2e_0002","type":"message","role":"assistant","model":"fixture-model-v1","content":[{"type":"text","text":"Also recorded: full-text search uses SQLite FTS5 with BM25 ranking."}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":90,"cache_creation_input_tokens":0,"cache_read_input_tokens":150,"output_tokens":15,"service_tier":"standard"}},"timestamp":"2026-11-12T09:01:05.000Z"}`)
	return b.String()
}

func codexFixture() string {
	var b strings.Builder
	line := func(format string, args ...any) {
		fmt.Fprintf(&b, format, args...)
		b.WriteString("\n")
	}
	line(`{"timestamp":"2026-11-12T10:00:00.000Z","ordinal":0,"type":"session_meta","payload":{"id":"%s","timestamp":"2026-11-12T10:00:00.000Z","cwd":"/home/dev/ohya-e2e","originator":"codex-tui","cli_version":"0.169.0","source":"cli","model_provider":"openai","git":{"commit_hash":"0000000000000000000000000000000000000000","branch":"main","repository_url":"https://example.com/dev/ohya-e2e.git"}}}`, codexThreadID)
	line(`{"timestamp":"2026-11-12T10:00:02.000Z","ordinal":1,"type":"response_item","payload":{"type":"message","id":"msg_e2e_codex_1","role":"user","content":[{"type":"input_text","text":"We need to bound ingestion batch memory on this project."}]}}`)
	line(`{"timestamp":"2026-11-12T10:00:08.000Z","ordinal":2,"type":"response_item","payload":{"type":"message","id":"msg_e2e_codex_2","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"Decision: ingestion batches are bounded at 4 MiB per batch."}]}}`)
	line(`{"timestamp":"2026-11-12T10:05:00.000Z","ordinal":3,"type":"response_item","payload":{"type":"message","id":"msg_e2e_codex_3","role":"user","content":[{"type":"input_text","text":"Large records still spike memory past the 4 MiB batch bound."}]}}`)
	line(`{"timestamp":"2026-11-12T10:05:10.000Z","ordinal":4,"type":"response_item","payload":{"type":"message","id":"msg_e2e_codex_4","role":"assistant","phase":"final","content":[{"type":"output_text","text":"Correction: the ingestion batch byte bound is 8 MiB; a batch never splits a single record."}]}}`)
	line(`{"timestamp":"2026-11-12T10:05:11.000Z","ordinal":5,"type":"token_usage_record","payload":{"thread_id":"%s","turn_id":"t_e2e_0001","response_id":"resp_e2e_codex_1","usage":{"input_tokens":900,"cached_input_tokens":0,"cache_write_input_tokens":0,"output_tokens":60,"reasoning_output_tokens":0,"total_tokens":960}}}`, codexThreadID)
	return b.String()
}

type seedFixture struct {
	agent, nativeID, path, content string
}

func seedFixtures(claudeRoot, codexRoot string) []seedFixture {
	return []seedFixture{
		{
			agent: "claude-code", nativeID: claudeSessionID, content: claudeFixture(),
			path: filepath.Join(claudeRoot, "-home-dev-ohya-e2e", claudeSessionID+".jsonl"),
		},
		{
			agent: "codex", nativeID: codexThreadID, content: codexFixture(),
			path: filepath.Join(codexRoot, "2026", "11", "12", "rollout-2026-11-12T10-00-00-"+codexThreadID+".jsonl"),
		},
	}
}

func writeSeedFixtures(fixtures []seedFixture) error {
	for _, fixture := range fixtures {
		if err := os.MkdirAll(filepath.Dir(fixture.path), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(fixture.path, []byte(fixture.content), 0o600); err != nil {
			return err
		}
	}
	return nil
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

type resolver struct {
	session *mcp.ClientSession
}

type resolveFunc func(ctx context.Context, id int64) (text string, found bool, err error)

func openResolver(ctx context.Context, ohya, dbPath, stateDir string) (*resolver, error) {
	cmd := exec.CommandContext(ctx, ohya, "mcp", "--db", dbPath, "--state", stateDir)
	client := mcp.NewClient(&mcp.Implementation{Name: "memory-e2e-gate", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return nil, fmt.Errorf("connect the gate's ohya mcp session: %w", err)
	}
	return &resolver{session: session}, nil
}

func (r *resolver) close() error { return r.session.Close() }

func (r *resolver) query(ctx context.Context, query string) ([][]any, error) {
	result, err := r.session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "sql",
		Arguments: map[string]any{"query": query},
	})
	if err != nil {
		return nil, err
	}
	if result.IsError {
		return nil, fmt.Errorf("sql tool error: %s", textOf(result))
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Rows [][]any `json:"rows"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	return payload.Rows, nil
}

func textOf(result *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range result.Content {
		if text, ok := c.(*mcp.TextContent); ok && text.Text != "" {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

func (r *resolver) verifySeedInventory(ctx context.Context, fixtures []seedFixture) error {
	eventCount := 0
	for _, fixture := range fixtures {
		eventCount += strings.Count(fixture.content, "\n")
	}
	rows, err := r.query(ctx, `SELECT (SELECT count(*) FROM sources),
		(SELECT count(*) FROM sessions), (SELECT count(*) FROM events)`)
	if err != nil {
		return fmt.Errorf("count seeded sources, sessions, and events: %w", err)
	}
	if len(rows) != 1 || len(rows[0]) != 3 ||
		rows[0][0] != float64(len(fixtures)) || rows[0][1] != float64(len(fixtures)) ||
		rows[0][2] != float64(eventCount) {
		return fmt.Errorf("unexpected seed inventory: got %v, want %d sources, %d sessions, and %d events",
			rows, len(fixtures), len(fixtures), eventCount)
	}

	for _, fixture := range fixtures {
		path, err := filepath.EvalSymlinks(fixture.path)
		if err != nil {
			return fmt.Errorf("resolve %s fixture path: %w", fixture.agent, err)
		}
		records := strings.Split(strings.TrimSuffix(fixture.content, "\n"), "\n")
		for i, record := range records {
			records[i] = sqlString(record)
		}
		query := fmt.Sprintf(`SELECT count(DISTINCT e.raw_json)
			FROM events e JOIN sources src ON src.id = e.source_id
			JOIN sessions s ON s.id = e.session_id
			WHERE src.path = %s AND src.agent = %s AND s.agent = src.agent AND s.native_id = %s
			AND e.raw_json IN (%s)`, sqlString(path), sqlString(fixture.agent),
			sqlString(fixture.nativeID), strings.Join(records, ", "))
		rows, err := r.query(ctx, query)
		if err != nil {
			return fmt.Errorf("verify %s fixture records: %w", fixture.agent, err)
		}
		if len(rows) != 1 || len(rows[0]) != 1 || rows[0][0] != float64(len(records)) {
			return fmt.Errorf("%s seed records do not match the fixture", fixture.agent)
		}
	}
	return nil
}

func sqlString(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func (r *resolver) countEventsWithText(ctx context.Context, marker string) (int, error) {
	rows, err := r.query(ctx, fmt.Sprintf(
		"SELECT count(*) FROM events WHERE text LIKE '%%%s%%'", marker))
	if err != nil {
		return 0, err
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		return 0, fmt.Errorf("count query returned %v", rows)
	}
	n, ok := rows[0][0].(float64)
	if !ok {
		return 0, fmt.Errorf("count returned %T", rows[0][0])
	}
	return int(n), nil
}

func (r *resolver) eventText(ctx context.Context, id int64) (string, bool, error) {
	rows, err := r.query(ctx, fmt.Sprintf(
		"SELECT id, text FROM events WHERE id = %d", id))
	if err != nil {
		return "", false, err
	}
	if len(rows) == 0 {
		return "", false, nil
	}
	if len(rows) > 1 || len(rows[0]) != 2 {
		return "", false, fmt.Errorf("id lookup returned %v", rows)
	}
	text, _ := rows[0][1].(string)
	return text, true, nil
}
