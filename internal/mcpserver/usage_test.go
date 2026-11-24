package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nibzard/ohya/internal/observe"
	"github.com/nibzard/ohya/internal/store"
)

type recordingSink struct {
	mu      sync.Mutex
	records []observe.UsageRecord
}

func (s *recordingSink) Record(r observe.UsageRecord) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, r)
	return true
}

func (s *recordingSink) all() []observe.UsageRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]observe.UsageRecord(nil), s.records...)
}

func (s *recordingSink) last() observe.UsageRecord {
	all := s.all()
	return all[len(all)-1]
}

func newUsageTestEngine(t *testing.T, o EngineOptions, sink UsageSink) (*Engine, *recordingSink) {
	t.Helper()
	if sink == nil {
		sink = &recordingSink{}
	}
	o.WriterID = "dev1"
	o.Usage = sink
	return newTestEngine(t, o), sink.(*recordingSink)
}

func mustRecord(t *testing.T, sink *recordingSink, want int) observe.UsageRecord {
	t.Helper()
	records := sink.all()
	if len(records) != want {
		t.Fatalf("usage records = %d, want %d: %+v", len(records), want, records)
	}
	return records[want-1]
}

func TestUsageRecordsAuthorizerReadsAndFunctions(t *testing.T) {
	e, sink := newUsageTestEngine(t, EngineOptions{}, nil)

	r := mustQuery(t, e, `SELECT id, raw_json FROM events`)
	_ = r
	record := mustRecord(t, sink, 1)
	if !slices.Contains(record.Reads, "events.id") || !slices.Contains(record.Reads, "events.raw_json") {
		t.Fatalf("reads = %v, want events.id and events.raw_json", record.Reads)
	}
	if record.FTS {
		t.Error("plain table scan marked as FTS")
	}
	if record.Outcome != observe.UsageOutcomeOK || record.Tool != observe.UsageToolSQL {
		t.Fatalf("outcome/tool = %s/%s", record.Outcome, record.Tool)
	}

	r = mustQuery(t, e, `SELECT e.id, s.cwd FROM events e JOIN sessions s ON s.id = e.session_id`)
	_ = r
	record = mustRecord(t, sink, 2)
	if !slices.Contains(record.Reads, "sessions.cwd") {
		t.Fatalf("reads = %v, want sessions.cwd from the join", record.Reads)
	}

	r = mustQuery(t, e, `SELECT e.id, bm25(events_fts) AS rank FROM events_fts
		JOIN events e ON e.id = events_fts.rowid
		WHERE events_fts MATCH 'authentication' ORDER BY rank`)
	_ = r
	record = mustRecord(t, sink, 3)
	if !record.FTS {
		t.Fatalf("reads = %v, functions = %v; want the FTS flag", record.Reads, record.Functions)
	}
	if !slices.Contains(record.Functions, "bm25") || !slices.Contains(record.Functions, "match") {
		t.Fatalf("functions = %v, want bm25 and match", record.Functions)
	}

	if _, err := e.Query(context.Background(), `SELECT * FROM no_such_table`); err == nil {
		t.Fatal("missing table query succeeded")
	}
	record = mustRecord(t, sink, 4)
	if record.Outcome != observe.UsageOutcomeSQLError {
		t.Fatalf("outcome = %s, want sql_error", record.Outcome)
	}
	if record.SQLiteCode != 1 {
		t.Fatalf("sqlite_code = %d, want SQLITE_ERROR (1)", record.SQLiteCode)
	}
	if record.Rows != 0 || record.ResultBytes != 0 {
		t.Fatalf("failed query recorded rows=%d bytes=%d", record.Rows, record.ResultBytes)
	}
}

func TestUsageRecordsLikeFunction(t *testing.T) {
	e, sink := newUsageTestEngine(t, EngineOptions{}, nil)

	mustQuery(t, e, `SELECT id FROM events WHERE text LIKE '%auth%'`)
	if record := mustRecord(t, sink, 1); !slices.Contains(record.Functions, "like") {
		t.Fatalf("functions = %v, want like", record.Functions)
	}
}

func TestUsageRecordsOutcomeCodes(t *testing.T) {
	t.Run("busy", func(t *testing.T) {
		e, sink := newUsageTestEngine(t, EngineOptions{MaxConcurrency: 1}, nil)
		e.slots <- struct{}{}
		defer func() { <-e.slots }()
		if _, err := e.Query(context.Background(), `SELECT 1`); !errors.Is(err, ErrBusy) {
			t.Fatalf("error = %v, want ErrBusy", err)
		}
		if record := mustRecord(t, sink, 1); record.Outcome != observe.UsageOutcomeBusy {
			t.Fatalf("outcome = %s, want busy", record.Outcome)
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		e, sink := newUsageTestEngine(t, EngineOptions{}, nil)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := e.Query(ctx, `SELECT 1`); !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
		if record := mustRecord(t, sink, 1); record.Outcome != observe.UsageOutcomeCancelled {
			t.Fatalf("outcome = %s, want cancelled", record.Outcome)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		e, sink := newUsageTestEngine(t, EngineOptions{}, nil)
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		if _, err := e.Query(ctx, `SELECT 1`); err == nil {
			t.Fatal("expired deadline accepted")
		}
		if record := mustRecord(t, sink, 1); record.Outcome != observe.UsageOutcomeTimeout {
			t.Fatalf("outcome = %s, want timeout", record.Outcome)
		}
	})

	t.Run("rejected statements", func(t *testing.T) {
		e, sink := newUsageTestEngine(t, EngineOptions{}, nil)
		if _, err := e.Query(context.Background(), `SELECT 1; SELECT 2`); !errors.Is(err, ErrMultipleStatements) {
			t.Fatalf("error = %v", err)
		}
		if record := mustRecord(t, sink, 1); record.Outcome != observe.UsageOutcomeRejected {
			t.Fatalf("multi-statement outcome = %s, want rejected", record.Outcome)
		}
		if _, err := e.Query(context.Background(), ""); !errors.Is(err, ErrEmptyQuery) {
			t.Fatalf("error = %v", err)
		}
		if record := mustRecord(t, sink, 2); record.Outcome != observe.UsageOutcomeRejected {
			t.Fatalf("empty outcome = %s, want rejected", record.Outcome)
		}
		oversized := "SELECT '" + strings.Repeat("a", 64<<10) + "'"
		if _, err := e.Query(context.Background(), oversized); !errors.Is(err, ErrQueryTooLarge) {
			t.Fatalf("error = %v, want ErrQueryTooLarge", err)
		}
		if record := mustRecord(t, sink, 3); record.Outcome != observe.UsageOutcomeRejected {
			t.Fatalf("oversized outcome = %s, want rejected", record.Outcome)
		}
	})

	t.Run("denied write is rejected with auth code", func(t *testing.T) {
		e, sink := newUsageTestEngine(t, EngineOptions{}, nil)
		if _, err := e.Query(context.Background(), `DELETE FROM events`); !errors.Is(err, ErrDenied) {
			t.Fatalf("error = %v, want ErrDenied", err)
		}
		record := mustRecord(t, sink, 1)
		if record.Outcome != observe.UsageOutcomeRejected {
			t.Fatalf("outcome = %s, want rejected", record.Outcome)
		}
		if record.SQLiteCode != 23 {
			t.Fatalf("sqlite_code = %d, want SQLITE_AUTH (23)", record.SQLiteCode)
		}
	})

	t.Run("truncated rows", func(t *testing.T) {
		e, sink := newUsageTestEngine(t, EngineOptions{MaxRows: 1}, nil)
		r := mustQuery(t, e, `SELECT id FROM events`)
		_ = r
		record := mustRecord(t, sink, 1)
		if record.Outcome != observe.UsageOutcomeTruncatedRows {
			t.Fatalf("outcome = %s, want truncated_rows", record.Outcome)
		}
		if record.Rows != 1 {
			t.Fatalf("rows = %d, want the returned row count", record.Rows)
		}
	})

	t.Run("truncated bytes", func(t *testing.T) {
		e, sink := newUsageTestEngine(t, EngineOptions{MaxOutputBytes: 4}, nil)
		r := mustQuery(t, e, `SELECT id FROM events`)
		_ = r
		if record := mustRecord(t, sink, 1); record.Outcome != observe.UsageOutcomeTruncatedBytes {
			t.Fatalf("outcome = %s, want truncated_bytes", record.Outcome)
		}
	})
}

func TestUsageRecordsPhasesRowsAndBytes(t *testing.T) {
	e, sink := newUsageTestEngine(t, EngineOptions{}, nil)
	r := mustQuery(t, e, `SELECT id, text FROM events ORDER BY id`)
	if r.RowCount != 2 {
		t.Fatalf("rows = %d", r.RowCount)
	}
	record := mustRecord(t, sink, 1)
	if err := record.Validate(); err != nil {
		t.Fatalf("record invalid: %v", err)
	}
	if record.PhaseMS.TotalMS <= 0 {
		t.Errorf("total_ms = %v, want a positive duration", record.PhaseMS.TotalMS)
	}
	if record.PhaseMS.TotalMS < record.PhaseMS.FirstRowMS ||
		record.PhaseMS.TotalMS < record.PhaseMS.IterationMS ||
		record.PhaseMS.TotalMS < record.PhaseMS.ReaderWaitMS {
		t.Errorf("phase durations exceed the total: %+v", record.PhaseMS)
	}
	if record.Rows != 2 || record.ResultBytes <= 0 {
		t.Errorf("rows/bytes = %d/%d, want 2 and encoded bytes", record.Rows, record.ResultBytes)
	}
}

func TestUsageRecordsAnnotateCalls(t *testing.T) {
	e, sink := newUsageTestEngine(t, EngineOptions{}, nil)

	raw := json.RawMessage(`{"operations":[
		{"op":"upsert","event_id":1,"namespace":"t","key":"a","value":{"note":"x"}},
		{"op":"delete","event_id":2,"namespace":"t","key":"b"}]}`)
	if _, err := e.Annotate(context.Background(), raw); err != nil {
		t.Fatal(err)
	}
	record := mustRecord(t, sink, 1)
	if record.Tool != observe.UsageToolAnnotate || record.Outcome != observe.UsageOutcomeOK {
		t.Fatalf("tool/outcome = %s/%s", record.Tool, record.Outcome)
	}
	if record.AnnotateOps == nil || record.AnnotateOps.Upsert != 1 || record.AnnotateOps.Delete != 1 {
		t.Fatalf("annotate_ops = %+v, want one upsert and one delete", record.AnnotateOps)
	}
	if record.DeviceID != "dev1" {
		t.Errorf("device_id = %q, want the writer id", record.DeviceID)
	}
	if len(record.Reads) != 0 || record.FTS {
		t.Errorf("annotate reads = %v fts = %v; annotate runs no authorized statement", record.Reads, record.FTS)
	}
	if err := record.Validate(); err != nil {
		t.Fatalf("record invalid: %v", err)
	}

	if _, err := e.Annotate(context.Background(), json.RawMessage(`{"operations":[{"op":"upsert"}]}`)); !errors.Is(err, ErrAnnotateRejected) {
		t.Fatalf("error = %v, want ErrAnnotateRejected", err)
	}
	record = mustRecord(t, sink, 2)
	if record.Outcome != observe.UsageOutcomeRejected {
		t.Fatalf("outcome = %s, want rejected", record.Outcome)
	}
	if record.AnnotateOps.Upsert != 0 {
		t.Fatalf("annotate_ops = %+v, want the counts of decoded operations only", record.AnnotateOps)
	}

	if _, err := e.Annotate(context.Background(), json.RawMessage(`{"operations":[
		{"op":"upsert","event_id":999999,"namespace":"t","key":"missing","value":1}]}`)); !errors.Is(err, store.ErrAnnotationRejected) {
		t.Fatalf("error = %v, want store rejection for a missing event", err)
	}
	record = mustRecord(t, sink, 3)
	if record.Outcome != observe.UsageOutcomeRejected {
		t.Fatalf("outcome = %s, want rejected", record.Outcome)
	}
	if record.AnnotateOps.Upsert != 1 {
		t.Fatalf("annotate_ops = %+v, want the decoded upsert counted", record.AnnotateOps)
	}
}

func TestUsageRecordsOriginFromContext(t *testing.T) {
	e, sink := newUsageTestEngine(t, EngineOptions{}, nil)
	ctx := WithOrigin(context.Background(), Origin{
		Client:        "codex",
		ClientVersion: "0.166.0",
		Path:          observe.UsagePathRemote,
		DeviceID:      "0123456789abcdef0123456789abcdef",
		SessionHint:   "sess-hint",
	})
	if _, err := e.Query(ctx, `SELECT 1`); err != nil {
		t.Fatal(err)
	}
	record := mustRecord(t, sink, 1)
	if record.Client != observe.UsageClientCodex || record.ClientVersion != "0.166" {
		t.Fatalf("client/version = %s/%s", record.Client, record.ClientVersion)
	}
	if record.Path != observe.UsagePathRemote || record.DeviceID != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("path/device = %s/%s", record.Path, record.DeviceID)
	}
	if record.SessionHint != "sess-hint" {
		t.Errorf("session_hint = %q", record.SessionHint)
	}

	if _, err := e.Query(context.Background(), `SELECT 1`); err != nil {
		t.Fatal(err)
	}
	record = mustRecord(t, sink, 2)
	if record.Path != observe.UsagePathLocal || record.DeviceID != "dev1" {
		t.Fatalf("default path/device = %s/%s, want local and the engine writer", record.Path, record.DeviceID)
	}
	if record.Client != observe.UsageClientUnknown {
		t.Errorf("client = %s, want unknown without an origin", record.Client)
	}
}

func TestUsageRecordsClientFromMCPInitialize(t *testing.T) {
	sink := &recordingSink{}
	hub, readers := openSeededHub(t)
	server := New(NewEngine(readers, hub.Writer(), EngineOptions{WriterID: "dev1", Usage: sink}),
		EngineOptions{WriterID: "dev1"})
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "claude-code", Version: "2.1.280"}, nil)
	session, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })

	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      ToolName,
		Arguments: map[string]any{"query": "SELECT count(*) FROM events"},
	}); err != nil {
		t.Fatal(err)
	}
	record := mustRecord(t, sink, 1)
	if record.Client != observe.UsageClientClaudeCode {
		t.Fatalf("client = %s, want claude-code from initialize", record.Client)
	}
	if record.ClientVersion != "2.1" {
		t.Fatalf("client_version = %s, want 2.1", record.ClientVersion)
	}
	if record.Path != observe.UsagePathLocal || record.Tool != observe.UsageToolSQL {
		t.Fatalf("path/tool = %s/%s", record.Path, record.Tool)
	}
	if err := record.Validate(); err != nil {
		t.Fatalf("record invalid: %v", err)
	}

	if _, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: AnnotateToolName,
		Arguments: map[string]any{"operations": []any{
			map[string]any{"op": "upsert", "event_id": 1, "namespace": "t", "key": "mcp", "value": 1},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	record = mustRecord(t, sink, 2)
	if record.Tool != observe.UsageToolAnnotate || record.Client != observe.UsageClientClaudeCode {
		t.Fatalf("tool/client = %s/%s", record.Tool, record.Client)
	}
}

func TestUsageRecordsNeverHoldSecrets(t *testing.T) {
	const (
		secretLiteral = "OHYA-SECRET-literal-8f21c0"
		secretValue   = "OHYA-SECRET-annotation-5b3a97"
		secretText    = "OHYA-SECRET-transcript-d40e11"
	)
	hub, readers := openSeededHub(t)
	if _, err := hub.Writer().Exec(`INSERT INTO events(id,session_id,source_id,source_offset,time_ms,kind,role,text,raw_json)
		VALUES(50,'claude:dev1:sess-1','src-1',200,3000,'message','user',?,'{}')`, secretText); err != nil {
		t.Fatal(err)
	}
	sink := &recordingSink{}
	e := NewEngine(readers, hub.Writer(), EngineOptions{WriterID: "dev1", Usage: sink})

	result := mustQuery(t, e, fmt.Sprintf(`SELECT id, text FROM events WHERE text != '%s' ORDER BY id`, secretLiteral))
	if len(result.Rows) != 3 || !strings.Contains(fmt.Sprint(result.Rows), secretText) {
		t.Fatalf("control failed: the seeded transcript secret must reach the query result: %v", result.Rows)
	}
	if _, err := e.Query(context.Background(), fmt.Sprintf(
		`SELECT count(*) FROM events WHERE text = '%s'`, secretLiteral)); err != nil {
		t.Fatalf("literal query: %v", err)
	}
	raw := json.RawMessage(fmt.Sprintf(`{"operations":[{"op":"upsert","event_id":1,"namespace":"secrets","key":"probe","value":{"note":"%s"}}]}`, secretValue))
	if _, err := e.Annotate(context.Background(), raw); err != nil {
		t.Fatal(err)
	}

	records := sink.all()
	if len(records) != 3 {
		t.Fatalf("usage records = %d, want 3", len(records))
	}
	for i, record := range records {
		if err := record.Validate(); err != nil {
			t.Fatalf("record %d invalid: %v", i, err)
		}
		encoded, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{secretLiteral, secretValue, secretText} {
			if strings.Contains(string(encoded), secret) {
				t.Fatalf("usage record %d leaks the secret: %s", i, encoded)
			}
		}
	}
}

func TestUsageSinkDisabledLeavesBehaviorUnchanged(t *testing.T) {
	e, _ := newUsageTestEngine(t, EngineOptions{}, nil)
	e.options.Usage = nil
	if _, err := e.Query(context.Background(), `SELECT count(*) FROM events`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Query(context.Background(), `DELETE FROM events`); !errors.Is(err, ErrDenied) {
		t.Fatalf("error = %v, want ErrDenied", err)
	}
}
