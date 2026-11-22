package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var (
	fileEvidenceRe = regexp.MustCompile(`(?i)evidence"?:?\s*(/\S+)`)
	evidenceRe     = regexp.MustCompile(`(?i)evidence"?:?\s*#?\s*(\d+)`)
	countLineRe    = regexp.MustCompile(`(?im)^\s*count"?:?\s*(\d+)\s*$`)
)

var negativePhrases = []string{
	"no evidence", "no record", "not find", "no sign", "no indication",
	"nothing found", "no mention", "could not locate", "no decision",
}

// metaTools carry no transcript data: ToolSearch resolves deferred tool
// schemas and appears when the client has not seen the ohya tools yet.
var metaTools = map[string]bool{"ToolSearch": true}

// gradeRun scores one finished run. Verdicts: correct, wrong-answer,
// bad-evidence, no-citation, negative-correct, negative-fabricated, invalid.
func gradeRun(ctx context.Context, res *resolver, q Question, arm string, m traceMetrics) string {
	for _, call := range m.ToolCalls {
		if metaTools[call] {
			continue
		}
		if arm == armOhya && !strings.HasPrefix(call, "mcp__ohya__") {
			return "invalid"
		}
		if arm == armRaw && strings.HasPrefix(call, "mcp__") {
			return "invalid"
		}
	}
	answer := m.Answer
	switch {
	case q.Negative:
		lower := strings.ToLower(answer)
		for _, p := range negativePhrases {
			if strings.Contains(lower, p) {
				return "negative-correct"
			}
		}
		return "negative-fabricated"
	case q.Tier == "aggregate":
		matches := countLineRe.FindAllStringSubmatch(answer, -1)
		if len(matches) == 0 {
			return "no-citation"
		}
		n, err := strconv.Atoi(matches[len(matches)-1][1])
		if err != nil {
			return "no-citation"
		}
		if n == q.CountValue {
			return "correct"
		}
		return "wrong-answer"
	}
	for _, want := range q.AnswerContains {
		if !strings.Contains(answer, want) {
			return "wrong-answer"
		}
	}
	if q.EvidenceMarker == "" {
		return "correct"
	}
	if arm == armRaw {
		matches := fileEvidenceRe.FindAllStringSubmatch(answer, -1)
		if len(matches) == 0 {
			return "no-citation"
		}
		path := matches[len(matches)-1][1]
		ok, err := fileContains(path, q.EvidenceMarker)
		if err != nil {
			return "bad-evidence"
		}
		if !ok {
			return "bad-evidence"
		}
		return "correct"
	}
	id, ok := citedEventID(answer)
	if !ok {
		return "no-citation"
	}
	text, sessionID, found, err := res.eventText(ctx, id)
	if err != nil || !found {
		return "bad-evidence"
	}
	if strings.Contains(text, q.EvidenceMarker) {
		return "correct"
	}
	// The citation may name any record of the deciding session, mirroring
	// the file-level contract the raw arm is graded against.
	inSession, err := res.sessionHasText(ctx, sessionID, q.EvidenceMarker)
	if err != nil {
		return "bad-evidence"
	}
	if inSession {
		return "correct"
	}
	return "bad-evidence"
}

func citedEventID(answer string) (int64, bool) {
	matches := evidenceRe.FindAllStringSubmatch(answer, -1)
	if len(matches) == 0 {
		return 0, false
	}
	id, err := strconv.ParseInt(matches[len(matches)-1][1], 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

// resolver is a client session against the ohya mcp server over the hub
// database, used for preflight counts and event-id grading.
type resolver struct {
	session *mcp.ClientSession
}

func openResolver(ctx context.Context, ohya, dbPath, stateDir string) (*resolver, error) {
	cmd := exec.CommandContext(ctx, ohya, "mcp", "--db", dbPath, "--state", stateDir)
	client := mcp.NewClient(&mcp.Implementation{Name: "recall-bench", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return nil, fmt.Errorf("connect the bench ohya mcp session: %w", err)
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
		return nil, fmt.Errorf("sql tool error: %s", resultText(result))
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

func resultText(result *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range result.Content {
		if text, ok := c.(*mcp.TextContent); ok && text.Text != "" {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

func (r *resolver) countSessionsWithText(ctx context.Context, marker string, settled bool) (int, error) {
	table := "events"
	if settled {
		table = "events_settled"
	}
	rows, err := r.query(ctx, fmt.Sprintf(
		"SELECT count(DISTINCT session_id) FROM %s WHERE id IN "+
			"(SELECT rowid FROM events_fts WHERE events_fts MATCH '\"%s\"')", table, marker))
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

func (r *resolver) eventText(ctx context.Context, id int64) (text string, sessionID string, found bool, err error) {
	rows, err := r.query(ctx, fmt.Sprintf("SELECT id, session_id, text FROM events WHERE id = %d", id))
	if err != nil {
		return "", "", false, err
	}
	if len(rows) == 0 {
		return "", "", false, nil
	}
	if len(rows) > 1 || len(rows[0]) != 3 {
		return "", "", false, fmt.Errorf("id lookup returned %v", rows)
	}
	sessionID, _ = rows[0][1].(string)
	text, _ = rows[0][2].(string)
	return text, sessionID, true, nil
}

func (r *resolver) sessionHasText(ctx context.Context, sessionID, marker string) (bool, error) {
	rows, err := r.query(ctx, fmt.Sprintf(
		"SELECT 1 FROM events WHERE session_id = '%s' AND id IN "+
			"(SELECT rowid FROM events_fts WHERE events_fts MATCH '\"%s\"') LIMIT 1", sessionID, marker))
	if err != nil {
		return false, err
	}
	return len(rows) > 0, nil
}
