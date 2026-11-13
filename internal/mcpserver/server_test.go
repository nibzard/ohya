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

// connect starts the server on one end of an in-memory transport pair and a
// real SDK client on the other, completing the initialize handshake.
func connect(t *testing.T, o EngineOptions) *mcp.ClientSession {
	t.Helper()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	hub, readers := openSeededHub(t)
	server := New(readers, hub.Writer(), o)
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

// call runs one tools/call and returns the result.
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
		"Current writer ID", "dev1", "events_fts", "event_annotations",
		"session_token_usage", "sqlite_master", "one statement per call",
		"truncated", "MATCH",
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

	// Zero rows still carry columns; rows encode as [].
	result = call(t, session, `SELECT id FROM events WHERE id > 99`)
	if err := redecode(result, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.RowCount != 0 || payload.Rows == nil || len(payload.Columns) != 1 {
		t.Fatalf("empty payload = %+v", payload)
	}
}

func TestServerReportsToolErrorsForBadSQL(t *testing.T) {
	// A short deadline keeps the expensive final case fast; the others are
	// rejected before execution.
	session := connect(t, EngineOptions{Deadline: 500 * time.Millisecond})
	for _, q := range []string{
		`DELETE FROM events`,           // denied by the authorizer
		`SELECT 1; SELECT 2`,           // two statements
		``,                             // nothing to run
		`SELECT * FROM missing_table`,  // SQLite error
		`ATTACH '/tmp/x.db' AS x`,      // attach denied
		`SELECT load_extension('x')`,   // unsafe function
		`PRAGMA journal_mode = DELETE`, // state-changing pragma
		// One row that costs seconds to compute: the deadline, not the row
		// limit, must stop it.
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
	// The session stays usable after errors.
	result := call(t, session, `SELECT 1 AS ok`)
	if result.IsError {
		t.Fatalf("session unusable after errors: %+v", result)
	}
}

// redecode unmarshals the structured output of a successful tool call.
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

// TestServerProtocolCompatibility records the compatibility check behind
// pinning the SDK: the server must speak the protocol versions its clients
// use. Claude Code negotiates 2025-06-18; verify a client pinned to it
// completes the handshake and calls the tool, and that the SDK's supported
// list covers every version a current client may send.
func TestServerProtocolCompatibility(t *testing.T) {
	for _, version := range mcp.SupportedProtocolVersions() {
		if version != "2025-06-18" && version != "2025-03-26" && version != "2024-11-05" {
			continue
		}
		serverTransport, clientTransport := mcp.NewInMemoryTransports()
		hub, readers := openSeededHub(t)
		server := New(readers, hub.Writer(), EngineOptions{WriterID: "dev1"})
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
