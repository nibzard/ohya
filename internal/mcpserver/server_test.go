package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func connect(t *testing.T, o EngineOptions) *mcp.ClientSession {
	t.Helper()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	hub, readers := openSeededHub(t)
	server := New(NewEngine(readers, hub.Writer(), o), o)
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { serverSession.Close() })
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "v0"}, nil)
	session, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func call(t *testing.T, session *mcp.ClientSession, query string) *mcp.CallToolResult {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      ToolName,
		Arguments: map[string]any{"query": query},
	})
	if err != nil {
		t.Fatalf("CallTool(%q): %v", query, err)
	}
	return result
}

func TestServerServesSQLTool(t *testing.T) {
	session := connect(t, EngineOptions{WriterID: "dev1"})

	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 2 {
		t.Fatalf("tool list = %v, want exactly sql and annotate", tools.Tools)
	}
	var sql, annotate *mcp.Tool
	for _, tl := range tools.Tools {
		switch tl.Name {
		case ToolName:
			sql = tl
		case AnnotateToolName:
			annotate = tl
		}
	}
	if sql == nil || annotate == nil {
		t.Fatalf("tool list = %v, want both sql and annotate", tools.Tools)
	}
	description := sql.Description
	for _, want := range []string{
		"Current writer ID", "dev1", "events_fts", "events_settled", "event_annotations",
		"session_token_usage", "sqlite_master", "one statement per call",
		"truncated", "MATCH", "Secret redaction", "[redacted:", "redactions",
		"best-effort risk reduction, never a guarantee", "ohya purge",
		"date-bounded recall", "strftime('%s','now')",
		"resume, step 1", "resume, step 2", "duplicate_of IS NULL",
		"SELECT DISTINCT model FROM sessions", "SELECT DISTINCT device_id FROM sessions",
		"Garbage audit", "ohya purge --marked tags.garbage", "length(e.raw_json) > 65536",
		"AS replay_copies", "GROUP BY s.model", "a.key = 'garbage'",
		"never run them yourself",
	} {
		if !strings.Contains(description, want) {
			t.Fatalf("tool description misses %q:\n%s", want, description)
		}
	}

	result := call(t, session, `SELECT count(*) AS events FROM events`)
	if result.IsError {
		t.Fatalf("select failed: %+v", result)
	}
	var payload Result
	if err := redecode(result, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.RowCount != 1 || fmt.Sprint(payload.Rows) != "[[2]]" || payload.Columns[0] != "events" {
		t.Fatalf("payload = %+v", payload)
	}

	result = call(t, session, `SELECT id FROM events WHERE id > 99`)
	if err := redecode(result, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.RowCount != 0 || payload.Rows == nil || len(payload.Columns) != 1 {
		t.Fatalf("empty payload = %+v", payload)
	}
}

func TestServerReportsToolErrorsForBadSQL(t *testing.T) {
	session := connect(t, EngineOptions{Deadline: 500 * time.Millisecond})
	for _, q := range []string{
		`DELETE FROM events`,
		`SELECT 1; SELECT 2`,
		``,
		`SELECT * FROM missing_table`,
		`ATTACH '/tmp/x.db' AS x`,
		`SELECT load_extension('x')`,
		`PRAGMA journal_mode = DELETE`,
		`WITH RECURSIVE c(n) AS (VALUES(0) UNION ALL SELECT n+1 FROM c) SELECT sum(n) FROM c`,
	} {
		result := call(t, session, q)
		if !result.IsError {
			t.Fatalf("query %q: result = %+v, want a tool error", q, result)
		}
		if len(result.Content) == 0 {
			t.Fatalf("query %q: error carries no content the agent can read", q)
		}
	}
	result := call(t, session, `SELECT 1 AS ok`)
	if result.IsError {
		t.Fatalf("session unusable after errors: %+v", result)
	}
}

func redecode(result *mcp.CallToolResult, payload *Result) error {
	if result.IsError {
		return fmt.Errorf("unexpected tool error: %+v", result)
	}
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, payload)
}

func TestServerProtocolCompatibility(t *testing.T) {
	for _, version := range mcp.SupportedProtocolVersions() {
		if version != "2025-06-18" && version != "2025-03-26" && version != "2024-11-05" {
			continue
		}
		serverTransport, clientTransport := mcp.NewInMemoryTransports()
		hub, readers := openSeededHub(t)
		server := New(NewEngine(readers, hub.Writer(), EngineOptions{WriterID: "dev1"}), EngineOptions{WriterID: "dev1"})
		serverSession, err := server.Connect(context.Background(), serverTransport, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { serverSession.Close() })
		client := mcp.NewClient(&mcp.Implementation{Name: "pinned", Version: "v0"}, nil)
		session, err := client.Connect(context.Background(), clientTransport,
			&mcp.ClientSessionOptions{ProtocolVersion: version})
		if err != nil {
			t.Fatalf("handshake at %s: %v", version, err)
		}
		t.Cleanup(func() { session.Close() })
		result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
			Name: ToolName, Arguments: map[string]any{"query": "SELECT 41 + 1"},
		})
		if err != nil {
			t.Fatalf("call at %s: %v", version, err)
		}
		var payload Result
		if err := redecode(result, &payload); err != nil || payload.RowCount != 1 {
			t.Fatalf("call at %s = %+v (%v)", version, result, err)
		}
	}
	if !slices.Contains(mcp.SupportedProtocolVersions(), "2025-06-18") {
		t.Fatal("SDK no longer supports 2025-06-18; re-check protocol compatibility")
	}
}

func TestMemoryGuidanceDeliveredAtInitialization(t *testing.T) {
	session := connect(t, EngineOptions{})
	if got := session.InitializeResult().Instructions; got != MemoryInstructions {
		t.Fatalf("server instructions = %q", got)
	}
}
