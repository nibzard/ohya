package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && strings.HasPrefix(os.Args[1], "child-") {
		var err error
		switch os.Args[1] {
		case "child-run":
			os.Exit(cliMain(os.Args[2:], os.Stdout, os.Stderr))
		case "child-serve":
			err = runServeCommand(os.Args[2:], os.Stdout, os.Stderr)
		case "child-join":
			err = runJoinCommand(os.Args[2:], os.Stdout, os.Stderr)
		case "child-mcp":
			err = runMcpCommand(os.Args[2:], os.Stdout, os.Stderr)
		default:
			fmt.Fprintln(os.Stderr, "ohya test child: unknown role", os.Args[1])
			os.Exit(2)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "ohya test child:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if db := os.Getenv("OHYA_MCP_TEST_DB"); db != "" {
		if err := runMcp(context.Background(), db, os.Getenv("OHYA_MCP_TEST_STATE")); err != nil {
			fmt.Fprintln(os.Stderr, "ohya mcp test server:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

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

func TestMcpRemoteBareFlagParses(t *testing.T) {
	err := runMcpCommand([]string{"--remote", "--state", t.TempDir()}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "pairing credential") {
		t.Fatalf("err = %v; want the unpaired state directory error", err)
	}
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

func callAnnotate(t *testing.T, session *mcp.ClientSession, operations string) *mcp.CallToolResult {
	t.Helper()
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "annotate",
		Arguments: map[string]any{"operations": json.RawMessage(operations)},
	})
	if err != nil {
		t.Fatalf("CallTool(annotate, %s): %v", operations, err)
	}
	return result
}

func TestMCPStdioEndToEnd(t *testing.T) {
	dbPath, stateDir := seedMcpDatabase(t)
	session := startMcpProcess(t, dbPath, stateDir)

	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var descriptions = map[string]string{}
	for _, tl := range tools.Tools {
		descriptions[tl.Name] = tl.Description
	}
	if len(tools.Tools) != 2 || descriptions["sql"] == "" || descriptions["annotate"] == "" {
		t.Fatalf("tools = %+v, want exactly sql and annotate", tools.Tools)
	}
	deviceID, err := device.LoadOrCreate(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(descriptions["sql"], "Current writer ID (use as event_annotations.writer_id and for your own annotations): "+deviceID) {
		t.Fatalf("sql description misses the writer ID %q", deviceID)
	}
	if !strings.Contains(descriptions["annotate"], "this device writes as "+deviceID) {
		t.Fatalf("annotate description misses the writer ID %q", deviceID)
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

	result = callSql(t, session, `SELECT count(*) FROM events`)
	raw, _ = json.Marshal(result.StructuredContent)
	if !strings.Contains(string(raw), "[[1]]") {
		t.Fatalf("events after adversarial calls: %s", raw)
	}
}

func TestMCPStdioAnnotateEndToEnd(t *testing.T) {
	dbPath, stateDir := seedMcpDatabase(t)
	session := startMcpProcess(t, dbPath, stateDir)
	deviceID, err := device.LoadOrCreate(stateDir)
	if err != nil {
		t.Fatal(err)
	}

	result := callAnnotate(t, session, `[
		{"op":"upsert","event_id":7,"namespace":"tags","key":"keep","value":{"done": true}},
		{"op":"delete","event_id":7,"namespace":"tags","key":"absent"}]`)
	if result.IsError {
		t.Fatalf("annotate failed: %+v", result)
	}
	if raw, _ := json.Marshal(result.StructuredContent); !strings.Contains(string(raw), `"applied":1,"unchanged":1`) {
		t.Fatalf("first batch payload = %s", raw)
	}

	result = callAnnotate(t, session, `[
		{"op":"upsert","event_id":7,"namespace":"tags","key":"keep","value":{"done": true}},
		{"op":"delete","event_id":7,"namespace":"tags","key":"absent"}]`)
	if result.IsError {
		t.Fatalf("retry failed: %+v", result)
	}
	if raw, _ := json.Marshal(result.StructuredContent); !strings.Contains(string(raw), `"applied":0,"unchanged":2`) {
		t.Fatalf("retry payload = %s", raw)
	}

	result = callSql(t, session, `SELECT value, writer_id FROM event_annotations
		WHERE event_id = 7 AND namespace = 'tags' AND key = 'keep'`)
	raw, _ := json.Marshal(result.StructuredContent)
	var stored struct {
		Rows [][]any `json:"rows"`
	}
	if err := json.Unmarshal(raw, &stored); err != nil || len(stored.Rows) != 1 {
		t.Fatalf("stored annotation = %s (%v)", raw, err)
	}
	if fmt.Sprint(stored.Rows[0]) != `[{"done":true} `+deviceID+`]` {
		t.Fatalf("stored annotation = %v, want compacted {\"done\":true} under writer %s", stored.Rows[0], deviceID)
	}

	for _, ops := range []string{
		`[{"op":"upsert","event_id":7,"namespace":"tags","key":"spoof","value":1,"writer_id":"attacker"}]`,
		`[{"op":"upsert","event_id":999,"namespace":"tags","key":"missing","value":1}]`,
	} {
		if result := callAnnotate(t, session, ops); !result.IsError {
			t.Fatalf("annotate accepted %s over stdio: %+v", ops, result)
		}
	}
	result = callSql(t, session, `SELECT count(*) FROM event_annotations`)
	raw, _ = json.Marshal(result.StructuredContent)
	if !strings.Contains(string(raw), "[[1]]") {
		t.Fatalf("annotations after rejections: %s", raw)
	}
}
