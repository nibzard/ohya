package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nibzard/ohya/internal/observe"
	"github.com/nibzard/ohya/internal/store"
)

const (
	usageDefaultSince  = "30d"
	usageDefaultDeadln = 120 * time.Second
	usageMaxSessions   = 5000
	usageMaxCalls      = 20000
	usageClaudeAgent   = "claude-code"
	usageCodexAgent    = "codex"
	usageOhyaserver    = "ohya"
	usageFTSMatch      = `"mcp ohya sql" OR "mcp ohya annotate"`
	usageTimeoutMarker = "execution deadline exceeded"
)

var usageSpec = commandSpec{
	name:      "ohya usage",
	synopsis:  "report how agents use the sql and annotate tools",
	usageLine: "ohya usage [flags]",
	description: "Combines two local evidence sources over a time window: server usage records under the state directory's observe/ tree (phases, outcomes, column reads, sizes) and transcript evidence in the hub database (Claude Code mcp__ohya__ tool_use and tool_result pairs, Codex McpToolCall items), which also covers calls made before instrumentation existed and sessions where the tools were available but unused. " +
		"Deterministic and read-only: no network, no LLM, no SQL text or transcript excerpts in the default output; --local-detail prints query text from transcripts, which are already local data, for the maintainer's own analysis. " +
		"The whole report runs under a deadline and row bounds; on expiry it prints the partial result, and exits 1 when expiry left nothing to report.",
	argsTitle: "Arguments",
	args:      "(none)",
	examples:  []string{"ohya usage", "ohya usage --since 7d --json", "ohya usage --until 2026-11-16T11:22:00Z"},
}

type usageWindow struct {
	SinceMS int64
	UntilMS int64
}

func parseUsageWindow(since, until string, nowMS int64) (usageWindow, error) {
	sinceSpan, err := parseUsageSince(since)
	if err != nil {
		return usageWindow{}, fmt.Errorf("--since: %w", err)
	}
	untilMS := nowMS
	if until != "" {
		t, err := time.Parse(time.RFC3339, until)
		if err != nil {
			return usageWindow{}, fmt.Errorf("--until: %w", err)
		}
		untilMS = t.UnixMilli()
	}
	return usageWindow{SinceMS: untilMS - sinceSpan.Milliseconds(), UntilMS: untilMS}, nil
}

func parseUsageSince(s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	if n := len(s); s[n-1] == 'd' {
		days, err := strconv.Atoi(s[:n-1])
		if err != nil || days <= 0 {
			return 0, fmt.Errorf("invalid day count %q", s)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	return d, nil
}

type usageLimits struct {
	MaxSessions int
	MaxCalls    int
}

type usageCall struct {
	Agent       string
	DeviceID    string
	SessionID   string
	Week        string
	Tool        string
	TimeMS      int64
	Outcome     string
	LatencyMS   float64
	HasLatency  bool
	ResultBytes int64
	HasResult   bool
	QueryText   string
}

type usageReport struct {
	SchemaVersion int             `json:"schema_version"`
	SinceMS       int64           `json:"since_ms"`
	UntilMS       int64           `json:"until_ms"`
	Partial       bool            `json:"partial"`
	Notes         []string        `json:"notes,omitempty"`
	Transcript    usageTranscript `json:"transcript"`
	Server        usageServer     `json:"server"`
}

type usageTranscript struct {
	Calls             int64             `json:"calls"`
	ByAgentTool       []usageAgentTool  `json:"by_agent_tool"`
	ByAgentDeviceWeek []usageAgentWeek  `json:"by_agent_device_week"`
	Outcomes          []usageToolCount  `json:"outcomes"`
	LatencyMS         []usageLatency    `json:"latency_ms"`
	ResultBytes       []usageResultSize `json:"result_bytes"`
	RawJSONCalls      int64             `json:"raw_json_calls"`
	SessionsWithCalls int64             `json:"sessions_with_calls"`
	ToolsAvailable    usageAvailability `json:"tools_available"`
	Detail            []usageCallDetail `json:"detail,omitempty"`
}

type usageAgentTool struct {
	Agent    string `json:"agent"`
	Tool     string `json:"tool"`
	Calls    int64  `json:"calls"`
	Sessions int64  `json:"sessions"`
	Devices  int64  `json:"devices"`
}

type usageAgentWeek struct {
	Agent    string `json:"agent"`
	DeviceID string `json:"device_id"`
	Week     string `json:"week"`
	Tool     string `json:"tool"`
	Calls    int64  `json:"calls"`
}

type usageToolCount struct {
	Tool  string `json:"tool"`
	Key   string `json:"key"`
	Calls int64  `json:"calls"`
}

type usageLatency struct {
	Tool string  `json:"tool"`
	N    int     `json:"n"`
	P50  float64 `json:"p50_ms"`
	P95  float64 `json:"p95_ms"`
	Max  float64 `json:"max_ms"`
}

type usageResultSize struct {
	Tool     string `json:"tool"`
	N        int    `json:"n"`
	MaxBytes int64  `json:"max_bytes"`
}

type usageAvailability struct {
	Agent                   string `json:"agent"`
	AvailableSessions       int64  `json:"available_sessions"`
	AvailableUnusedSessions int64  `json:"available_unused_sessions"`
	Note                    string `json:"note,omitempty"`
}

type usageCallDetail struct {
	Time        string   `json:"time"`
	Agent       string   `json:"agent"`
	DeviceID    string   `json:"device_id"`
	SessionID   string   `json:"session_id"`
	Tool        string   `json:"tool"`
	Outcome     string   `json:"outcome"`
	LatencyMS   *float64 `json:"latency_ms,omitempty"`
	ResultBytes *int64   `json:"result_bytes,omitempty"`
	QueryText   string   `json:"query_text,omitempty"`
}

type usageServer struct {
	Found            bool              `json:"found"`
	Instances        int64             `json:"instances"`
	Records          int64             `json:"records"`
	MalformedRecords int64             `json:"malformed_records"`
	ByToolClientWeek []usageServerWeek `json:"by_tool_client_week"`
	Outcomes         []usageToolCount  `json:"outcomes"`
	LatencyMS        []usageLatency    `json:"latency_ms"`
	Rows             int64             `json:"rows"`
	ResultBytesMax   int64             `json:"result_bytes_max"`
	Reads            []usageKeyCount   `json:"reads"`
	RawJSONCalls     int64             `json:"raw_json_calls"`
	Functions        []usageKeyCount   `json:"functions"`
	FTSCalls         int64             `json:"fts_calls"`
	AnnotateOps      *usageAnnotateOps `json:"annotate_ops,omitempty"`
}

type usageServerWeek struct {
	Tool   string `json:"tool"`
	Client string `json:"client"`
	Path   string `json:"path"`
	Week   string `json:"week"`
	Calls  int64  `json:"calls"`
}

type usageKeyCount struct {
	Key   string `json:"key"`
	Calls int64  `json:"calls"`
}

type usageAnnotateOps struct {
	Upsert int64 `json:"upsert"`
	Delete int64 `json:"delete"`
}

func runUsageCommand(args []string, stdout, stderr io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	c := newCommandFlags(usageSpec)
	db := c.fs.String("db", filepath.Join(home, ".ohya", "hub.db"),
		"hub database `path`")
	state := c.fs.String("state", "",
		"state `directory` whose observe/ tree holds server usage records; defaults to the database directory")
	since := c.fs.String("since", usageDefaultSince,
		"window `length` before --until, as a Go duration or Nd for days")
	until := c.fs.String("until", "",
		"window end as an RFC3339 `timestamp`; defaults to now")
	deadline := c.fs.Duration("deadline", usageDefaultDeadln,
		"bound the whole report; a partial report prints when it expires")
	asJSON := c.fs.Bool("json", false,
		"print one JSON object instead of the text report (docs/adr/0005)")
	localDetail := c.fs.Bool("local-detail", false,
		"also print per-call lines with query text from local transcripts")
	if err := c.parse(args, stdout); err != nil {
		return err
	}
	if c.fs.NArg() > 0 {
		return c.usageErrorf("unexpected argument %q", c.fs.Arg(0))
	}
	if *deadline <= 0 {
		return c.usageErrorf("--deadline must be positive")
	}
	window, err := parseUsageWindow(*since, *until, time.Now().UnixMilli())
	if err != nil {
		return c.usageErrorf("%s", err.Error())
	}
	stateDir := *state
	if stateDir == "" {
		stateDir = filepath.Dir(*db)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *deadline)
	defer cancel()
	report, calls, collectErr := collectUsageReport(ctx, *db, stateDir, window, usageLimits{
		MaxSessions: usageMaxSessions,
		MaxCalls:    usageMaxCalls,
	})
	if collectErr != nil {
		return jsonFailurePrint(*asJSON, stdout, usageSpec.name, collectErr)
	}
	if *localDetail {
		report.Transcript.Detail = usageDetail(calls)
	}
	if *asJSON {
		return printJSON(stdout, usageSpec.name, report)
	}
	renderUsageText(stdout, report, stateDir)
	return nil
}

func usageDetail(calls []usageCall) []usageCallDetail {
	details := make([]usageCallDetail, 0, len(calls))
	for _, call := range calls {
		d := usageCallDetail{
			Time:      time.UnixMilli(call.TimeMS).UTC().Format(time.RFC3339),
			Agent:     call.Agent,
			DeviceID:  call.DeviceID,
			SessionID: call.SessionID,
			Tool:      call.Tool,
			Outcome:   call.Outcome,
			QueryText: call.QueryText,
		}
		if call.HasLatency {
			latency := call.LatencyMS
			d.LatencyMS = &latency
		}
		if call.HasResult {
			resultBytes := call.ResultBytes
			d.ResultBytes = &resultBytes
		}
		details = append(details, d)
	}
	sort.Slice(details, func(i, j int) bool {
		a, b := details[i], details[j]
		if a.Time != b.Time {
			return a.Time < b.Time
		}
		if a.Agent != b.Agent {
			return a.Agent < b.Agent
		}
		return a.SessionID < b.SessionID
	})
	return details
}

func collectUsageReport(ctx context.Context, dbPath, stateDir string, window usageWindow, limits usageLimits) (*usageReport, []usageCall, error) {
	hub, err := store.OpenReadOnly(dbPath)
	if err != nil {
		return nil, nil, err
	}
	defer hub.Close()
	reader, err := hub.Reader()
	if err != nil {
		return nil, nil, err
	}
	defer reader.Close()

	report := &usageReport{SchemaVersion: 1, SinceMS: window.SinceMS, UntilMS: window.UntilMS}
	calls, transcriptTruncated, transcriptErr := collectTranscriptCalls(ctx, reader, window, limits)
	availability, availabilityErr := collectAvailability(ctx, reader, window)

	hardErr := firstHardError(transcriptErr, availabilityErr)
	if hardErr != nil {
		return nil, nil, fmt.Errorf("read transcript evidence: %w", hardErr)
	}
	deadlineHit := errors.Is(transcriptErr, context.DeadlineExceeded) ||
		errors.Is(availabilityErr, context.DeadlineExceeded)
	if deadlineHit {
		report.Partial = true
		report.Notes = append(report.Notes,
			"deadline expired before the transcript scan finished; the counts above are a lower bound")
	}
	if transcriptTruncated {
		report.Partial = true
		report.Notes = append(report.Notes,
			fmt.Sprintf("transcript evidence truncated at %d calls; narrow the window", limits.MaxCalls))
	}
	if deadlineHit && len(calls) == 0 && availability.AvailableSessions == 0 {
		return nil, nil, fmt.Errorf("deadline expired before any evidence was collected")
	}
	report.Transcript = transcriptSection(calls, availability)

	server, serverErr := collectServerUsage(stateDir, window)
	if serverErr != nil {
		report.Notes = append(report.Notes, "server usage records unreadable: "+serverErr.Error())
	} else {
		report.Server = *server
	}
	return report, calls, nil
}

func firstHardError(errs ...error) error {
	for _, err := range errs {
		if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			return err
		}
	}
	return nil
}

func collectTranscriptCalls(ctx context.Context, reader *sql.DB, window usageWindow, limits usageLimits) ([]usageCall, bool, error) {
	claudeCalls, claudeTruncated, err := collectClaudeCodeCalls(ctx, reader, window, limits)
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return nil, false, err
	}
	codexCalls, codexTruncated, codexErr := collectCodexCalls(ctx, reader, window, limits)
	if codexErr != nil && !errors.Is(codexErr, context.DeadlineExceeded) {
		return nil, false, codexErr
	}
	calls := append(claudeCalls, codexCalls...)
	sort.Slice(calls, func(i, j int) bool {
		a, b := calls[i], calls[j]
		if a.TimeMS != b.TimeMS {
			return a.TimeMS < b.TimeMS
		}
		if a.Agent != b.Agent {
			return a.Agent < b.Agent
		}
		return a.SessionID < b.SessionID
	})
	return calls, claudeTruncated || codexTruncated, firstHardError(err, codexErr)
}

type claudeBlockMeta struct {
	Blocks []struct {
		Type      string         `json:"type"`
		Name      string         `json:"name"`
		ID        string         `json:"id"`
		Input     map[string]any `json:"input"`
		ToolUseID string         `json:"tool_use_id"`
		IsError   *bool          `json:"is_error"`
	} `json:"blocks"`
}

func collectClaudeCodeCalls(ctx context.Context, reader *sql.DB, window usageWindow, limits usageLimits) ([]usageCall, bool, error) {
	rows, err := reader.QueryContext(ctx, `
		SELECT e.session_id, s.device_id, e.time_ms, e.metadata
		FROM events_fts f
		JOIN events e ON e.id = f.rowid AND e.duplicate_of IS NULL
		JOIN sessions s ON s.id = e.session_id
		WHERE events_fts MATCH ?
		  AND s.agent = ?
		  AND e.time_ms >= ? AND e.time_ms <= ?
		ORDER BY e.id
		LIMIT ?`,
		usageFTSMatch, usageClaudeAgent, window.SinceMS, window.UntilMS, limits.MaxCalls+1)
	if err != nil {
		return nil, false, fmt.Errorf("claude-code candidate query: %w", err)
	}
	defer rows.Close()
	type candidate struct {
		sessionID string
		deviceID  string
		timeMS    int64
		metadata  string
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.sessionID, &c.deviceID, &c.timeMS, &c.metadata); err != nil {
			return nil, false, fmt.Errorf("claude-code candidate scan: %w", err)
		}
		candidates = append(candidates, c)
	}
	if err := rows.Err(); err != nil {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		return nil, false, fmt.Errorf("claude-code candidate rows: %w", err)
	}
	truncated := len(candidates) > limits.MaxCalls
	if truncated {
		candidates = candidates[:limits.MaxCalls]
	}
	var calls []usageCall
	for _, c := range candidates {
		if ctx.Err() != nil {
			return calls, truncated, ctx.Err()
		}
		var meta claudeBlockMeta
		if err := json.Unmarshal([]byte(c.metadata), &meta); err != nil {
			continue
		}
		for _, block := range meta.Blocks {
			if !strings.HasPrefix(block.Name, "mcp__ohya__") || block.ID == "" {
				continue
			}
			call := usageCall{
				Agent:     usageClaudeAgent,
				DeviceID:  c.deviceID,
				SessionID: c.sessionID,
				Week:      isoWeek(c.timeMS),
				Tool:      strings.TrimPrefix(block.Name, "mcp__ohya__"),
				TimeMS:    c.timeMS,
				Outcome:   "no_result",
			}
			if query, ok := block.Input["query"].(string); ok {
				call.QueryText = query
			}
			pairClaudeResult(ctx, reader, &call, block.ID)
			calls = append(calls, call)
			if len(calls) >= limits.MaxCalls {
				return calls, true, nil
			}
		}
	}
	return calls, truncated, nil
}

func pairClaudeResult(ctx context.Context, reader *sql.DB, call *usageCall, callID string) {
	rows, err := reader.QueryContext(ctx, `
		SELECT e.time_ms, e.metadata, length(coalesce(e.text, '')), substr(coalesce(e.text, ''), 1, 512)
		FROM events e
		WHERE e.session_id = ? AND e.duplicate_of IS NULL
		  AND e.time_ms >= ? AND e.metadata LIKE ?
		ORDER BY e.time_ms, e.id
		LIMIT 6`,
		call.SessionID, call.TimeMS, `%"`+callID+`"%`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var timeMS int64
		var metadata, textPrefix string
		var textLen int64
		if err := rows.Scan(&timeMS, &metadata, &textLen, &textPrefix); err != nil {
			return
		}
		var meta claudeBlockMeta
		if err := json.Unmarshal([]byte(metadata), &meta); err != nil {
			continue
		}
		for _, block := range meta.Blocks {
			if block.ToolUseID != callID {
				continue
			}
			latency := float64(timeMS - call.TimeMS)
			if latency < 0 {
				latency = 0
			}
			call.LatencyMS = latency
			call.HasLatency = true
			call.ResultBytes = textLen
			call.HasResult = true
			call.Outcome = "ok"
			if block.IsError != nil && *block.IsError {
				call.Outcome = "error"
				if strings.Contains(textPrefix, usageTimeoutMarker) {
					call.Outcome = "timeout"
				}
			}
			return
		}
	}
}

type codexMcpRecord struct {
	Payload struct {
		Type          string `json:"type"`
		StartedAtMS   int64  `json:"started_at_ms"`
		CompletedAtMS int64  `json:"completed_at_ms"`
		Item          *struct {
			Type      string         `json:"type"`
			Server    string         `json:"server"`
			Tool      string         `json:"tool"`
			Status    string         `json:"status"`
			Arguments map[string]any `json:"arguments"`
			Result    *struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
			Duration *struct {
				Secs  int64 `json:"secs"`
				Nanos int64 `json:"nanos"`
			} `json:"duration"`
		} `json:"item"`
		Invocation *struct {
			Server    string         `json:"server"`
			Tool      string         `json:"tool"`
			Arguments map[string]any `json:"arguments"`
		} `json:"invocation"`
		Duration *struct {
			Secs  int64 `json:"secs"`
			Nanos int64 `json:"nanos"`
		} `json:"duration"`
		Result *struct {
			Ok *struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"Ok"`
			Err json.RawMessage `json:"Err"`
		} `json:"result"`
	} `json:"payload"`
}

func collectCodexCalls(ctx context.Context, reader *sql.DB, window usageWindow, limits usageLimits) ([]usageCall, bool, error) {
	sessions, truncated, err := windowSessions(ctx, reader, usageCodexAgent, window, limits)
	if err != nil {
		return nil, false, err
	}
	var calls []usageCall
	for _, sessionID := range sessions {
		if ctx.Err() != nil {
			return calls, truncated, ctx.Err()
		}
		rows, err := reader.QueryContext(ctx, `
			SELECT e.session_id, s.device_id, e.time_ms, e.raw_json
			FROM events e JOIN sessions s ON s.id = e.session_id
			WHERE e.session_id = ? AND e.kind = 'event_msg' AND e.duplicate_of IS NULL
			  AND e.time_ms >= ? AND e.time_ms <= ?
			  AND (
			    (json_extract(e.raw_json, '$.payload.type') = 'item_completed'
			     AND json_extract(e.raw_json, '$.payload.item.type') = 'McpToolCall'
			     AND json_extract(e.raw_json, '$.payload.item.server') = ?
			    ) OR (
			     json_extract(e.raw_json, '$.payload.type') = 'mcp_tool_call_end'
			     AND json_extract(e.raw_json, '$.payload.invocation.server') = ?
			    ))
			ORDER BY e.id`,
			sessionID, window.SinceMS, window.UntilMS, usageOhyaserver, usageOhyaserver)
		if err != nil {
			return calls, truncated, fmt.Errorf("codex call query: %w", err)
		}
		for rows.Next() {
			var sessionID, deviceID, raw string
			var timeMS int64
			if err := rows.Scan(&sessionID, &deviceID, &timeMS, &raw); err != nil {
				rows.Close()
				return nil, false, fmt.Errorf("codex call scan: %w", err)
			}
			call, ok := codexCallFromRecord(raw, sessionID, deviceID, timeMS)
			if !ok {
				continue
			}
			calls = append(calls, call)
			if len(calls) >= limits.MaxCalls {
				rows.Close()
				return calls, true, nil
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			if ctx.Err() != nil {
				return calls, truncated, ctx.Err()
			}
			return nil, false, fmt.Errorf("codex call rows: %w", err)
		}
		rows.Close()
	}
	return calls, truncated, nil
}

func codexCallFromRecord(raw, sessionID, deviceID string, timeMS int64) (usageCall, bool) {
	var record codexMcpRecord
	if err := json.Unmarshal([]byte(raw), &record); err != nil {
		return usageCall{}, false
	}
	call := usageCall{
		Agent:     usageCodexAgent,
		DeviceID:  deviceID,
		SessionID: sessionID,
		Week:      isoWeek(timeMS),
		TimeMS:    timeMS,
		Outcome:   "ok",
	}
	p := record.Payload
	switch {
	case p.Item != nil && p.Item.Server == usageOhyaserver:
		call.Tool = p.Item.Tool
		if query, ok := p.Item.Arguments["query"].(string); ok {
			call.QueryText = query
		}
		if p.Item.Result != nil {
			call.HasResult = true
			for _, content := range p.Item.Result.Content {
				call.ResultBytes += int64(len(content.Text))
			}
		}
		if p.Item.Status == "failed" {
			call.Outcome = "error"
			if p.Item.Result != nil {
				for _, content := range p.Item.Result.Content {
					if strings.Contains(content.Text, usageTimeoutMarker) {
						call.Outcome = "timeout"
					}
				}
			}
		}
		switch {
		case p.CompletedAtMS > 0 && p.StartedAtMS > 0:
			call.LatencyMS = float64(p.CompletedAtMS - p.StartedAtMS)
			call.HasLatency = true
		case p.Item.Duration != nil:
			call.LatencyMS = durationMS(p.Item.Duration.Secs, p.Item.Duration.Nanos)
			call.HasLatency = true
		}
	case p.Invocation != nil && p.Invocation.Server == usageOhyaserver:
		call.Tool = p.Invocation.Tool
		if query, ok := p.Invocation.Arguments["query"].(string); ok {
			call.QueryText = query
		}
		if p.Result != nil && p.Result.Err != nil && string(p.Result.Err) != "null" {
			call.Outcome = "error"
			if strings.Contains(string(p.Result.Err), usageTimeoutMarker) {
				call.Outcome = "timeout"
			}
		}
		if p.Result != nil && p.Result.Ok != nil {
			call.HasResult = true
			for _, content := range p.Result.Ok.Content {
				call.ResultBytes += int64(len(content.Text))
			}
		}
		if p.Duration != nil {
			call.LatencyMS = durationMS(p.Duration.Secs, p.Duration.Nanos)
			call.HasLatency = true
		}
	default:
		return usageCall{}, false
	}
	if call.Tool == "" {
		return usageCall{}, false
	}
	return call, true
}

func durationMS(secs, nanos int64) float64 {
	if secs < 0 || nanos < 0 {
		return 0
	}
	return float64(secs)*1000 + float64(nanos)/1e6
}

func windowSessions(ctx context.Context, reader *sql.DB, agent string, window usageWindow, limits usageLimits) ([]string, bool, error) {
	rows, err := reader.QueryContext(ctx, `
		SELECT id FROM sessions
		WHERE agent = ? AND last_event_at_ms >= ?
		  AND (started_at_ms IS NULL OR started_at_ms <= ?)
		ORDER BY id
		LIMIT ?`,
		agent, window.SinceMS, window.UntilMS, limits.MaxSessions+1)
	if err != nil {
		return nil, false, fmt.Errorf("%s session query: %w", agent, err)
	}
	defer rows.Close()
	var sessions []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, false, fmt.Errorf("%s session scan: %w", agent, err)
		}
		sessions = append(sessions, id)
	}
	if err := rows.Err(); err != nil {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		return nil, false, fmt.Errorf("%s session rows: %w", agent, err)
	}
	truncated := len(sessions) > limits.MaxSessions
	if truncated {
		sessions = sessions[:limits.MaxSessions]
	}
	return sessions, truncated, nil
}

func collectAvailability(ctx context.Context, reader *sql.DB, window usageWindow) (usageAvailability, error) {
	availability := usageAvailability{
		Agent: usageClaudeAgent,
		Note:  "codex transcripts do not record tool availability; Claude Code prompt snapshots and deferred tool deltas list the tools",
	}
	err := reader.QueryRowContext(ctx, `
		SELECT count(*) FROM (
		SELECT s.id FROM sessions s
		WHERE s.agent = 'claude-code' AND s.last_event_at_ms >= ?
		  AND (s.started_at_ms IS NULL OR s.started_at_ms <= ?)
		  AND EXISTS (
		    SELECT 1 FROM events e
		    WHERE e.session_id = s.id AND e.kind = 'attachment'
		      AND e.time_ms <= ?
		      AND json_extract(e.raw_json, '$.attachment.type') IN ('prompt_snapshot', 'deferred_tools_delta')
		      AND (e.raw_json LIKE '%mcp__ohya__sql%' OR e.raw_json LIKE '%mcp__ohya__annotate%')
		    LIMIT 1))`,
		window.SinceMS, window.UntilMS, window.UntilMS).Scan(&availability.AvailableSessions)
	if err != nil {
		if ctx.Err() != nil {
			return availability, ctx.Err()
		}
		return availability, fmt.Errorf("availability query: %w", err)
	}
	return availability, nil
}

func transcriptSection(calls []usageCall, availability usageAvailability) usageTranscript {
	section := usageTranscript{Calls: int64(len(calls))}
	type agentToolKey struct{ agent, tool string }
	agentToolCalls := map[agentToolKey]int64{}
	agentToolSessions := map[agentToolKey]map[string]bool{}
	agentToolDevices := map[agentToolKey]map[string]bool{}
	agentWeekCalls := map[usageAgentWeek]int64{}
	outcomeCalls := map[usageToolCount]int64{}
	sessionsWithCalls := map[string]bool{}
	latencies := map[string][]float64{}
	resultSizes := map[string][]int64{}
	var rawJSONCalls int64
	for _, call := range calls {
		at := agentToolKey{call.Agent, call.Tool}
		agentToolCalls[at]++
		if agentToolSessions[at] == nil {
			agentToolSessions[at] = map[string]bool{}
		}
		agentToolSessions[at][call.SessionID] = true
		if agentToolDevices[at] == nil {
			agentToolDevices[at] = map[string]bool{}
		}
		agentToolDevices[at][call.DeviceID] = true
		agentWeekCalls[usageAgentWeek{
			Agent: call.Agent, DeviceID: call.DeviceID, Week: call.Week, Tool: call.Tool,
		}]++
		outcomeCalls[usageToolCount{Tool: call.Tool, Key: call.Outcome}]++
		sessionsWithCalls[call.SessionID] = true
		if call.HasLatency {
			latencies[call.Tool] = append(latencies[call.Tool], call.LatencyMS)
		}
		if call.HasResult {
			resultSizes[call.Tool] = append(resultSizes[call.Tool], call.ResultBytes)
		}
		if call.Tool == observe.UsageToolSQL && strings.Contains(strings.ToLower(call.QueryText), "raw_json") {
			rawJSONCalls++
		}
	}
	for key, n := range agentToolCalls {
		section.ByAgentTool = append(section.ByAgentTool, usageAgentTool{
			Agent: key.agent, Tool: key.tool, Calls: n,
			Sessions: int64(len(agentToolSessions[key])),
			Devices:  int64(len(agentToolDevices[key])),
		})
	}
	sort.Slice(section.ByAgentTool, func(i, j int) bool {
		a, b := section.ByAgentTool[i], section.ByAgentTool[j]
		if a.Agent != b.Agent {
			return a.Agent < b.Agent
		}
		return a.Tool < b.Tool
	})
	for row, n := range agentWeekCalls {
		row.Calls = n
		section.ByAgentDeviceWeek = append(section.ByAgentDeviceWeek, row)
	}
	sort.Slice(section.ByAgentDeviceWeek, func(i, j int) bool {
		a, b := section.ByAgentDeviceWeek[i], section.ByAgentDeviceWeek[j]
		if a.Agent != b.Agent {
			return a.Agent < b.Agent
		}
		if a.DeviceID != b.DeviceID {
			return a.DeviceID < b.DeviceID
		}
		if a.Week != b.Week {
			return a.Week < b.Week
		}
		return a.Tool < b.Tool
	})
	for key, n := range outcomeCalls {
		section.Outcomes = append(section.Outcomes, usageToolCount{Tool: key.Tool, Key: key.Key, Calls: n})
	}
	sort.Slice(section.Outcomes, func(i, j int) bool {
		a, b := section.Outcomes[i], section.Outcomes[j]
		if a.Tool != b.Tool {
			return a.Tool < b.Tool
		}
		return a.Key < b.Key
	})
	for tool, values := range latencies {
		section.LatencyMS = append(section.LatencyMS, latencyStat(tool, values))
	}
	sort.Slice(section.LatencyMS, func(i, j int) bool { return section.LatencyMS[i].Tool < section.LatencyMS[j].Tool })
	for tool, values := range resultSizes {
		sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
		section.ResultBytes = append(section.ResultBytes, usageResultSize{
			Tool: tool, N: len(values), MaxBytes: values[len(values)-1],
		})
	}
	sort.Slice(section.ResultBytes, func(i, j int) bool { return section.ResultBytes[i].Tool < section.ResultBytes[j].Tool })
	section.RawJSONCalls = rawJSONCalls
	section.SessionsWithCalls = int64(len(sessionsWithCalls))

	var claudeCalledSessions int64
	for sessionID := range sessionsWithCalls {
		if strings.HasPrefix(sessionID, usageClaudeAgent+":") {
			claudeCalledSessions++
		}
	}
	availability.AvailableUnusedSessions = availability.AvailableSessions - claudeCalledSessions
	if availability.AvailableUnusedSessions < 0 {
		availability.AvailableUnusedSessions = 0
	}
	section.ToolsAvailable = availability
	return section
}

func latencyStat(tool string, values []float64) usageLatency {
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	return usageLatency{
		Tool: tool,
		N:    len(sorted),
		P50:  percentile(sorted, 0.50),
		P95:  percentile(sorted, 0.95),
		Max:  sorted[len(sorted)-1],
	}
}

func percentile(sorted []float64, fraction float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	rank := int(math.Ceil(fraction * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

func isoWeek(ms int64) string {
	year, week := time.UnixMilli(ms).UTC().ISOWeek()
	return fmt.Sprintf("%04d-W%02d", year, week)
}

func collectServerUsage(stateDir string, window usageWindow) (*usageServer, error) {
	observeDir := filepath.Join(stateDir, "observe")
	matches, err := filepath.Glob(filepath.Join(observeDir, "*", "usage.jsonl*"))
	if err != nil {
		return nil, err
	}
	server := &usageServer{}
	instances := map[string]bool{}
	weekCalls := map[usageServerWeek]int64{}
	outcomeCalls := map[usageToolCount]int64{}
	readCalls := map[string]int64{}
	functionCalls := map[string]int64{}
	latencies := map[string][]float64{}
	for _, path := range matches {
		instance := filepath.Base(filepath.Dir(path))
		if !instances[instance] {
			instances[instance] = true
			server.Instances++
		}
		if err := readUsageFile(path, window, server, weekCalls, outcomeCalls, readCalls, functionCalls, latencies); err != nil {
			return nil, err
		}
	}
	server.Found = len(matches) > 0
	for row, n := range weekCalls {
		row.Calls = n
		server.ByToolClientWeek = append(server.ByToolClientWeek, row)
	}
	sort.Slice(server.ByToolClientWeek, func(i, j int) bool {
		a, b := server.ByToolClientWeek[i], server.ByToolClientWeek[j]
		if a.Tool != b.Tool {
			return a.Tool < b.Tool
		}
		if a.Client != b.Client {
			return a.Client < b.Client
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Week < b.Week
	})
	for key, n := range outcomeCalls {
		server.Outcomes = append(server.Outcomes, usageToolCount{Tool: key.Tool, Key: key.Key, Calls: n})
	}
	sort.Slice(server.Outcomes, func(i, j int) bool {
		a, b := server.Outcomes[i], server.Outcomes[j]
		if a.Tool != b.Tool {
			return a.Tool < b.Tool
		}
		return a.Key < b.Key
	})
	server.Reads = sortedKeyCounts(readCalls)
	server.Functions = sortedKeyCounts(functionCalls)
	for tool, values := range latencies {
		server.LatencyMS = append(server.LatencyMS, latencyStat(tool, values))
	}
	sort.Slice(server.LatencyMS, func(i, j int) bool { return server.LatencyMS[i].Tool < server.LatencyMS[j].Tool })
	return server, nil
}

func readUsageFile(
	path string,
	window usageWindow,
	server *usageServer,
	weekCalls map[usageServerWeek]int64,
	outcomeCalls map[usageToolCount]int64,
	readCalls map[string]int64,
	functionCalls map[string]int64,
	latencies map[string][]float64,
) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(io.LimitReader(file, 64<<20))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var record observe.UsageRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			server.MalformedRecords++
			continue
		}
		if record.TimeMS < window.SinceMS || record.TimeMS > window.UntilMS {
			continue
		}
		server.Records++
		server.Rows += record.Rows
		if record.ResultBytes > server.ResultBytesMax {
			server.ResultBytesMax = record.ResultBytes
		}
		if record.FTS {
			server.FTSCalls++
		}
		weekCalls[usageServerWeek{
			Tool:   record.Tool,
			Client: record.Client,
			Path:   record.Path,
			Week:   isoWeek(record.TimeMS),
		}]++
		outcomeCalls[usageToolCount{Tool: record.Tool, Key: record.Outcome}]++
		latencies[record.Tool] = append(latencies[record.Tool], record.PhaseMS.TotalMS)
		for _, read := range record.Reads {
			readCalls[read]++
			if read == "events.raw_json" || read == "events_settled.raw_json" {
				server.RawJSONCalls++
			}
		}
		for _, function := range record.Functions {
			functionCalls[function]++
		}
		if record.AnnotateOps != nil {
			if server.AnnotateOps == nil {
				server.AnnotateOps = &usageAnnotateOps{}
			}
			server.AnnotateOps.Upsert += record.AnnotateOps.Upsert
			server.AnnotateOps.Delete += record.AnnotateOps.Delete
		}
	}
	return scanner.Err()
}

func sortedKeyCounts(counts map[string]int64) []usageKeyCount {
	out := make([]usageKeyCount, 0, len(counts))
	for key, n := range counts {
		out = append(out, usageKeyCount{Key: key, Calls: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Calls != out[j].Calls {
			return out[i].Calls > out[j].Calls
		}
		return out[i].Key < out[j].Key
	})
	return out
}

func renderUsageText(w io.Writer, report *usageReport, stateDir string) {
	since := time.UnixMilli(report.SinceMS).UTC().Format(time.RFC3339)
	until := time.UnixMilli(report.UntilMS).UTC().Format(time.RFC3339)
	fmt.Fprintf(w, "ohya usage — %s .. %s\n\n", since, until)
	fmt.Fprintf(w, "Transcript evidence (hub events):\n")
	if len(report.Transcript.ByAgentTool) == 0 {
		fmt.Fprintf(w, "  no sql or annotate calls found\n")
	}
	for _, row := range report.Transcript.ByAgentTool {
		sessionWord := "sessions"
		if row.Sessions == 1 {
			sessionWord = "session"
		}
		deviceWord := "devices"
		if row.Devices == 1 {
			deviceWord = "device"
		}
		fmt.Fprintf(w, "  %-12s %-9s %3d calls  %d %s  %d %s\n",
			row.Agent, row.Tool, row.Calls, row.Sessions, sessionWord, row.Devices, deviceWord)
	}
	if len(report.Transcript.ByAgentDeviceWeek) > 0 {
		fmt.Fprintf(w, "  by agent, device, week:\n")
		for _, row := range report.Transcript.ByAgentDeviceWeek {
			fmt.Fprintf(w, "    %-12s %s  %s  %-9s %3d\n",
				row.Agent, row.DeviceID, row.Week, row.Tool, row.Calls)
		}
	}
	if len(report.Transcript.Outcomes) > 0 {
		fmt.Fprintf(w, "  outcomes:")
		byTool := map[string][]string{}
		for _, row := range report.Transcript.Outcomes {
			byTool[row.Tool] = append(byTool[row.Tool], fmt.Sprintf("%s %d", row.Key, row.Calls))
		}
		tools := make([]string, 0, len(byTool))
		for tool := range byTool {
			tools = append(tools, tool)
		}
		sort.Strings(tools)
		for _, tool := range tools {
			fmt.Fprintf(w, " %s: %s;", tool, strings.Join(byTool[tool], ", "))
		}
		fmt.Fprintln(w)
	}
	for _, stat := range report.Transcript.LatencyMS {
		fmt.Fprintf(w, "  latency ms (agent-observed) %s: n %d  p50 %.0f  p95 %.0f  max %.0f\n",
			stat.Tool, stat.N, stat.P50, stat.P95, stat.Max)
	}
	for _, stat := range report.Transcript.ResultBytes {
		fmt.Fprintf(w, "  result bytes %s: n %d  max %d\n", stat.Tool, stat.N, stat.MaxBytes)
	}
	var sqlCalls int64
	for _, row := range report.Transcript.ByAgentTool {
		if row.Tool == observe.UsageToolSQL {
			sqlCalls += row.Calls
		}
	}
	fmt.Fprintf(w, "  queries naming raw_json: %d of %d sql calls\n",
		report.Transcript.RawJSONCalls, sqlCalls)
	availability := report.Transcript.ToolsAvailable
	fmt.Fprintf(w, "  tools available (%s): %d sessions, %d with no call\n",
		availability.Agent, availability.AvailableSessions, availability.AvailableUnusedSessions)
	if availability.Note != "" {
		fmt.Fprintf(w, "  note: %s\n", availability.Note)
	}
	if len(report.Transcript.Detail) > 0 {
		fmt.Fprintf(w, "  calls (--local-detail):\n")
		for _, detail := range report.Transcript.Detail {
			latency := "-"
			if detail.LatencyMS != nil {
				latency = fmt.Sprintf("%.0f ms", *detail.LatencyMS)
			}
			size := "-"
			if detail.ResultBytes != nil {
				size = fmt.Sprintf("%d B", *detail.ResultBytes)
			}
			fmt.Fprintf(w, "    %s %s %s …%s %s %s %s\n",
				detail.Time, detail.Agent, detail.DeviceID, shortSession(detail.SessionID),
				detail.Tool, latency, size)
			if detail.QueryText != "" {
				fmt.Fprintf(w, "      %s\n", strings.ReplaceAll(detail.QueryText, "\n", " "))
			}
		}
	}
	fmt.Fprintf(w, "\nServer usage records (%s):\n", filepath.Join(stateDir, "observe"))
	if !report.Server.Found {
		fmt.Fprintf(w, "  none found\n")
	} else {
		fmt.Fprintf(w, "  records: %d from %d instances", report.Server.Records, report.Server.Instances)
		if report.Server.MalformedRecords > 0 {
			fmt.Fprintf(w, " (%d malformed lines skipped)", report.Server.MalformedRecords)
		}
		fmt.Fprintln(w)
		for _, row := range report.Server.ByToolClientWeek {
			fmt.Fprintf(w, "  %-9s %-11s %-7s %s  %3d calls\n",
				row.Tool, row.Client, row.Path, row.Week, row.Calls)
		}
		if len(report.Server.Outcomes) > 0 {
			fmt.Fprintf(w, "  outcomes:")
			for _, row := range report.Server.Outcomes {
				fmt.Fprintf(w, " %s: %s %d;", row.Tool, row.Key, row.Calls)
			}
			fmt.Fprintln(w)
		}
		for _, stat := range report.Server.LatencyMS {
			fmt.Fprintf(w, "  latency ms (server) %s: n %d  p50 %.0f  p95 %.0f  max %.0f\n",
				stat.Tool, stat.N, stat.P50, stat.P95, stat.Max)
		}
		fmt.Fprintf(w, "  rows %d  result max %d B  fts calls %d  raw_json calls %d\n",
			report.Server.Rows, report.Server.ResultBytesMax, report.Server.FTSCalls, report.Server.RawJSONCalls)
		if len(report.Server.Reads) > 0 {
			var parts []string
			for i, read := range report.Server.Reads {
				if i >= 8 {
					parts = append(parts, "…")
					break
				}
				parts = append(parts, fmt.Sprintf("%s %d", read.Key, read.Calls))
			}
			fmt.Fprintf(w, "  reads: %s\n", strings.Join(parts, ", "))
		}
		if len(report.Server.Functions) > 0 {
			var parts []string
			for i, function := range report.Server.Functions {
				if i >= 8 {
					parts = append(parts, "…")
					break
				}
				parts = append(parts, fmt.Sprintf("%s %d", function.Key, function.Calls))
			}
			fmt.Fprintf(w, "  functions: %s\n", strings.Join(parts, ", "))
		}
		if report.Server.AnnotateOps != nil {
			fmt.Fprintf(w, "  annotate ops: upsert %d  delete %d\n",
				report.Server.AnnotateOps.Upsert, report.Server.AnnotateOps.Delete)
		}
	}
	if report.Partial {
		fmt.Fprintf(w, "\nPartial report:\n")
		for _, note := range report.Notes {
			fmt.Fprintf(w, "  %s\n", note)
		}
	}
}

func shortSession(sessionID string) string {
	if i := strings.LastIndexByte(sessionID, ':'); i >= 0 && i+1 < len(sessionID) {
		return sessionID[i+1 : min(i+13, len(sessionID))]
	}
	return sessionID[:min(12, len(sessionID))]
}
