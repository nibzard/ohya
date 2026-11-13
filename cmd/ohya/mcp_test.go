package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nibzard/ohya/internal/device"
	"github.com/nibzard/ohya/internal/store"
)

// TestMain implements the fork-server trick: with OHYA_MCP_TEST_DB set, the
// test binary runs the production mcp command instead of the tests, so the
// testing framework writes nothing to the protocol stream on stdout.
func TestMain(m *testing.M) {
	if db := os.Getenv("OHYA_MCP_TEST_DB"); db != "" {
		if err := runMcp(context.Background(), db, os.Getenv("OHYA_MCP_TEST_STATE")); err != nil {
			fmt.Fprintln(os.Stderr, "ohya mcp test server:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// seedMcpDatabase creates a hub database with one event and returns its path
// plus the state directory.
func seedMcpDatabase(t *testing.T) (dbPath, stateDir string) {
	t.Helper()
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "hub", "hub.db")
	stateDir = filepath.Join(dir, "state")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	hub, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO sessions(id,agent,device_id,native_id,first_prompt)
		 VALUES('claude:dev1:sess-1','claude','dev1','sess-1','Find primes')`,
		`INSERT INTO sources(id,device_id,agent,path) VALUES('src-1','dev1','claude','/repo/a.jsonl')`,
		`INSERT INTO events(id,session_id,source_id,source_offset,kind,raw_json,text)
		 VALUES(7,'claude:dev1:sess-1','src-1',0,'message','{}','hello stdio world')`,
	} {
		if _, err := hub.Writer().Exec(q); err != nil {
			hub.Close()
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	hub.Close()
	return dbPath, stateDir
}

// startMcpProcess spawns this test binary as the ohya mcp server over real
// stdio pipes, exactly as an MCP client launches the binary.
func startMcpProcess(t *testing.T, dbPath, stateDir string) *mcp.ClientSession {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(),
		"OHYA_MCP_TEST_DB="+dbPath,
		"OHYA_MCP_TEST_STATE="+stateDir,
	)
	client := mcp.NewClient(&mcp.Implementation{Name: "stdio-client", Version: "v0"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("connect over stdio: %v", err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func callSql(t *testing.T, session *mcp.ClientSession, query string) *mcp.CallToolResult {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "sql",
		Arguments: map[string]any{"query": query},
	})
	if err != nil {
		t.Fatalf("CallTool(%q): %v", query, err)
	}
	return result
}

// TestMCPStdioEndToEnd launches the real command as a subprocess and speaks
// MCP over stdio: initialize handshake, tool listing, a working query, and
// an adversarial query rejected as a tool error.
func TestMCPStdioEndToEnd(t *testing.T) {
	dbPath, stateDir := seedMcpDatabase(t)
	session := startMcpProcess(t, dbPath, stateDir)

	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 1 || tools.Tools[0].Name != "sql" {
		t.Fatalf("tools = %+v", tools.Tools)
	}
	deviceID, err := device.LoadOrCreate(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tools.Tools[0].Description, "Current writer ID (use as event_annotations.writer_id and for your own annotations): "+deviceID) {
		t.Fatalf("description misses the writer ID %q", deviceID)
	}

	result := callSql(t, session,
		`SELECT e.id, e.text FROM events_fts JOIN events e ON e.id = events_fts.rowid
		 WHERE events_fts MATCH 'stdio'`)
	if result.IsError {
		t.Fatalf("FTS query failed: %+v", result)
	}
	var payload struct {
		Columns  []string `json:"columns"`
		Rows     [][]any  `json:"rows"`
		RowCount int      `json:"row_count"`
	}
	raw, _ := json.Marshal(result.StructuredContent)
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.RowCount != 1 || fmt.Sprint(payload.Rows[0][0]) != "7" {
		t.Fatalf("payload = %s", raw)
	}

	for _, q := range []string{
		`DELETE FROM events`,
		`INSERT INTO events(id,session_id,source_id,source_offset,kind,raw_json) VALUES(8,'x','y',0,'k','{}'); SELECT 1`,
		`SELECT load_extension('x')`,
	} {
		if result := callSql(t, session, q); !result.IsError {
			t.Fatalf("query %q accepted over stdio: %+v", q, result)
		}
	}

	// The rejected writes changed nothing.
	result = callSql(t, session, `SELECT count(*) FROM events`)
	raw, _ = json.Marshal(result.StructuredContent)
	if !strings.Contains(string(raw), "[[1]]") {
		t.Fatalf("events after adversarial calls: %s", raw)
	}
}
