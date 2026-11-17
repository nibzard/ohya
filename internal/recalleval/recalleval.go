package recalleval

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/nibzard/ohya/internal/claudecode"
	"github.com/nibzard/ohya/internal/codex"
	"github.com/nibzard/ohya/internal/events"
	"github.com/nibzard/ohya/internal/mcpserver"
	"github.com/nibzard/ohya/internal/pi"
	"github.com/nibzard/ohya/internal/store"
)

const DeviceID = "dev-recall"

const TopK = 10

type Fixture struct {
	Agent string
	Name  string
}

var Fixtures = []Fixture{
	{Agent: claudecode.AgentName, Name: "claude-main.jsonl"},
	{Agent: claudecode.AgentName, Name: "claude-subagent.jsonl"},
	{Agent: codex.AgentName, Name: "codex-session.jsonl"},
	{Agent: pi.AgentName, Name: "pi-session.jsonl"},
}

type Question struct {
	Key    string
	Agent  string
	Ask    string
	Match  string
	Repo   string
	Marker string
}

var Questions = []Question{
	{
		Key:    "uploader-backoff",
		Agent:  "claude-code",
		Ask:    "What retry backoff cap was decided for the device uploader?",
		Match:  "retry AND backoff AND cap",
		Marker: "oyr-41a1",
	},
	{
		Key:    "cursor-diagnosis",
		Agent:  "claude-code",
		Ask:    "Have we seen the upload cursor regression failure before, and what was the root cause?",
		Match:  "cursor AND acknowledged AND commit",
		Marker: "oyr-52b2",
	},
	{
		Key:    "batch-bound",
		Agent:  "claude-code",
		Ask:    "What is the ingestion batch byte bound after the correction?",
		Match:  "ingestion AND batch AND bound",
		Marker: "oyr-63c3",
	},
	{
		Key:    "invitation-ttl",
		Agent:  "claude-code",
		Ask:    "How long is a pairing invitation valid, and is it single-use?",
		Match:  "invitation AND ttl",
		Marker: "oyr-74d4",
	},
	{
		Key:    "registry-locked",
		Agent:  "codex",
		Ask:    "Have we seen SQLITE_BUSY on the registry, and under what conditions?",
		Match:  "database AND locked AND invitation",
		Marker: "oyr-85e5",
	},
	{
		Key:    "registry-txlock",
		Agent:  "codex",
		Ask:    "What was decided about registry locking under concurrent joins?",
		Match:  "registry AND txlock",
		Marker: "oyr-96f6",
	},
	{
		Key:    "server-repo-locked",
		Agent:  "codex",
		Ask:    "In the server repository, which invitation consumption was reported locked?",
		Match:  "invitation AND locked",
		Repo:   "example.com/agent/server",
		Marker: "oyr-85e5",
	},
	{
		Key:    "uploader-test-failure",
		Agent:  "claude-code",
		Ask:    "Which uploader test failed before the fix, and what did it report?",
		Match:  "fail AND regressed",
		Marker: "oyr-b8c8",
	},
	{
		Key:    "pi-usage-key",
		Agent:  "pi",
		Ask:    "How does the Pi parser key token usage?",
		Match:  "responseid AND fallback",
		Marker: "oyr-a7b7",
	},
}

type QuestionResult struct {
	Question Question
	AnswerID int64
	Rank     int
	Hit      bool
}

type Report struct {
	Results []QuestionResult
	Hits    int
	Misses  int
}

func (r Report) Result(key string) (QuestionResult, bool) {
	for _, res := range r.Results {
		if res.Question.Key == key {
			return res, true
		}
	}
	return QuestionResult{}, false
}

func Ingest(ctx context.Context, hub *store.DB, dir string) error {
	for _, f := range Fixtures {
		data, err := os.ReadFile(filepath.Join(dir, f.Name))
		if err != nil {
			return fmt.Errorf("recalleval: read %s: %w", f.Name, err)
		}
		sourceID := "recall/" + f.Name
		if err := hub.RegisterSource(ctx, sourceID, DeviceID, f.Agent, "/"+sourceID); err != nil {
			return fmt.Errorf("recalleval: register %s: %w", sourceID, err)
		}
		parsed, err := events.Parse(f.Agent, data, 0, events.Options{SourceScope: sourceID})
		if err != nil {
			return fmt.Errorf("recalleval: parse %s: %w", f.Name, err)
		}
		if _, err := hub.Ingest(ctx, store.Batch{
			SourceID: sourceID, DeviceID: DeviceID, Events: parsed.Events}); err != nil {
			return fmt.Errorf("recalleval: ingest %s: %w", f.Name, err)
		}
	}
	return nil
}

func Run(ctx context.Context, engine *mcpserver.Engine) (Report, error) {
	report := Report{}
	for _, q := range Questions {
		answer, err := answerID(ctx, engine, q)
		if err != nil {
			return report, fmt.Errorf("recalleval: question %s: %w", q.Key, err)
		}
		ids, err := rankedIDs(ctx, engine, q)
		if err != nil {
			return report, fmt.Errorf("recalleval: question %s: %w", q.Key, err)
		}
		rank := 0
		for i, id := range ids {
			if id == answer {
				rank = i + 1
				break
			}
		}
		result := QuestionResult{Question: q, AnswerID: answer, Rank: rank, Hit: rank > 0}
		report.Results = append(report.Results, result)
		if result.Hit {
			report.Hits++
		} else {
			report.Misses++
		}
	}
	return report, nil
}

func (q Question) query() string {
	sql := "SELECT CAST(e.id AS TEXT) AS id FROM events_fts " +
		"JOIN events_settled e ON e.id = events_fts.rowid "
	if q.Repo != "" {
		sql += "JOIN sessions s ON s.id = e.session_id "
	}
	sql += "WHERE events_fts MATCH '" + q.Match + "'"
	if q.Repo != "" {
		sql += " AND s.repo = '" + q.Repo + "'"
	}
	return sql + " ORDER BY events_fts.rank LIMIT " + strconv.Itoa(TopK)
}

func answerID(ctx context.Context, engine *mcpserver.Engine, q Question) (int64, error) {
	res, err := engine.Query(ctx,
		"SELECT CAST(id AS TEXT) AS id FROM events WHERE text LIKE '%"+q.Marker+"%'")
	if err != nil {
		return 0, err
	}
	if len(res.Rows) != 1 {
		return 0, fmt.Errorf("marker %s matched %d events, want exactly 1",
			q.Marker, len(res.Rows))
	}
	return rowID(res.Rows[0])
}

func rankedIDs(ctx context.Context, engine *mcpserver.Engine, q Question) ([]int64, error) {
	res, err := engine.Query(ctx, q.query())
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(res.Rows))
	for _, row := range res.Rows {
		id, err := rowID(row)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func rowID(row []any) (int64, error) {
	if len(row) != 1 {
		return 0, fmt.Errorf("id row has %d columns, want 1", len(row))
	}
	text, ok := row[0].(string)
	if !ok {
		return 0, fmt.Errorf("id column is %T, want string", row[0])
	}
	id, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("id column %q: %w", text, err)
	}
	return id, nil
}
