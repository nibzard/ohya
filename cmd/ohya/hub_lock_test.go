package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nibzard/ohya/internal/store"
)

const lockWaitLine = "ohya: hub database is busy, waiting for its write lock"

func holdHubDatabase(t *testing.T, dbPath string) func() {
	t.Helper()
	dsn := url.URL{Scheme: "file", Path: filepath.ToSlash(dbPath), RawQuery: "_busy_timeout=5000&_journal_mode=WAL&_locking_mode=EXCLUSIVE"}
	holder, err := sql.Open("sqlite3", dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	conn, err := holder.Conn(context.Background())
	if err != nil {
		holder.Close()
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), `BEGIN IMMEDIATE`); err != nil {
		conn.Close()
		holder.Close()
		t.Fatal(err)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
			conn.Close()
			holder.Close()
		})
	}
	t.Cleanup(release)
	return release
}

type hubChild struct {
	cmd  *exec.Cmd
	log  string
	done chan struct{}
	err  error
}

func startHubChild(t *testing.T, cmd *exec.Cmd) *hubChild {
	t.Helper()
	path := filepath.Join(t.TempDir(), "child.log")
	log, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	child := &hubChild{cmd: cmd, log: path, done: make(chan struct{})}
	go func() {
		child.err = cmd.Wait()
		close(child.done)
	}()
	t.Cleanup(func() {
		select {
		case <-child.done:
		default:
			_ = cmd.Process.Kill()
			<-child.done
		}
	})
	return child
}

func (c *hubChild) output() string {
	out, _ := os.ReadFile(c.log)
	return string(out)
}

func (c *hubChild) waitForOutput(t *testing.T, want string) {
	t.Helper()
	timer := time.NewTimer(60 * time.Second)
	defer timer.Stop()
	for !strings.Contains(c.output(), want) {
		select {
		case <-c.done:
			t.Fatalf("%s exited with %v before it logged %q; output:\n%s", strings.Join(c.cmd.Args, " "), c.err, want, c.output())
		case <-timer.C:
			t.Fatalf("%s did not log %q in time; output:\n%s", strings.Join(c.cmd.Args, " "), want, c.output())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func waitForOutput(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		out, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(out), want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never logged %q; it holds:\n%s", path, want, out)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestMCPStdioServesWhileTheHubDatabaseIsLocked(t *testing.T) {
	dbPath, stateDir := seedMcpDatabase(t)
	release := holdHubDatabase(t, dbPath)
	cmd := mcpProcessCommand(dbPath, stateDir)
	stderr := filepath.Join(t.TempDir(), "stderr.log")
	log, err := os.Create(stderr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.Close() })
	cmd.Stderr = log
	session := connectMcpProcess(t, cmd)

	type reply struct {
		result *mcp.CallToolResult
		err    error
	}
	first := make(chan reply, 1)
	go func() {
		result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
			Name:      "sql",
			Arguments: map[string]any{"query": "SELECT count(*) FROM events"},
		})
		first <- reply{result, err}
	}()
	waitForOutput(t, stderr, lockWaitLine)
	got := <-first
	if got.err != nil {
		t.Fatalf("sql while the hub database is locked: %v; want a tool error", got.err)
	}
	text, _ := got.result.Content[0].(*mcp.TextContent)
	if !got.result.IsError || !strings.Contains(text.Text, "still opening") {
		t.Fatalf("sql while the hub database is locked = %+v; want an error that says the database is still opening", got.result)
	}

	release()
	result := callSql(t, session, `SELECT e.id FROM events_fts JOIN events e ON e.id = events_fts.rowid WHERE events_fts MATCH 'stdio'`)
	raw, _ := json.Marshal(result.StructuredContent)
	if result.IsError || !strings.Contains(string(raw), "[[7]]") {
		t.Fatalf("sql after the lock was released = %+v, %s; want the seeded row in the same session", result, raw)
	}
	result = callAnnotate(t, session, `[{"op":"upsert","event_id":7,"namespace":"tags","key":"keep","value":{"done":true}}]`)
	if result.IsError {
		t.Fatalf("annotate after the lock was released failed: %+v", result)
	}
}

func TestMCPStdioExitsWithTheOpenError(t *testing.T) {
	dbPath, stateDir := seedMcpDatabase(t)
	raw := rawOpen(t, dbPath)
	if _, err := raw.Exec(`INSERT INTO schema_migrations (version, name, checksum, applied_at_ms) VALUES (99, 'future', 'x', 0)`); err != nil {
		t.Fatal(err)
	}

	cmd := mcpProcessCommand(dbPath, stateDir)
	stdin, keepOpen, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stdin.Close(); keepOpen.Close() })
	cmd.Stdin = stdin
	child := startHubChild(t, cmd)
	select {
	case <-child.done:
	case <-time.After(30 * time.Second):
		t.Fatalf("the MCP server did not exit after its open failed; output:\n%s", child.output())
	}
	var exitErr *exec.ExitError
	if !errors.As(child.err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("the MCP server exited with %v; want exit status 1", child.err)
	}
	if out := child.output(); !strings.Contains(out, "ohya mcp test server: store: migration 099 is applied but missing from the binary") {
		t.Fatalf("the MCP server did not report its open error; output:\n%s", out)
	}
}

func TestHubServicesWaitForALockedHubDatabase(t *testing.T) {
	if testing.Short() {
		t.Skip("end-to-end stack test")
	}
	t.Parallel()
	dir := transportTestDir(t)
	hubDB := filepath.Join(dir, "hub", "hub.db")
	hubState := filepath.Join(dir, "hub-state")
	devState := filepath.Join(dir, "dev-state")
	claudeRoot := filepath.Join(dir, "claude-projects")
	for _, dir := range []string{filepath.Dir(hubDB), hubState, devState, claudeRoot} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	hub, err := store.Open(context.Background(), hubDB)
	if err != nil {
		t.Fatal(err)
	}
	if err := hub.Close(); err != nil {
		t.Fatal(err)
	}

	release := holdHubDatabase(t, hubDB)
	regionFile := writeDERPRegionFile(t, startLocalDERP(t))
	serveCmd := exec.Command(os.Args[0], "child-serve", "--db", hubDB, "--state", hubState)
	serveCmd.Env = append(os.Environ(), "OHYA_TEST_DERP_REGION_FILE="+regionFile)
	serve := startHubChild(t, serveCmd)
	ingest := startHubChild(t, exec.Command(os.Args[0], "child-run", "ingest", "--allow-empty",
		"--db", hubDB, "--state", devState,
		"--claude-root", claudeRoot, "--codex-root", "", "--pi-root", "", "--antigravity-root", ""))

	for _, child := range []*hubChild{serve, ingest} {
		child.waitForOutput(t, lockWaitLine)
	}
	release()
	serve.waitForOutput(t, "serving devices on the tailcat transport")
	ingest.waitForOutput(t, "ohya: imported 0 events from 0 sources")
}
