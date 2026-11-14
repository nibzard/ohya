// End-to-end test of the serving and joining commands: a real `ohya serve`
// child process provisions the coordination plane, enrolls its transport
// node, and serves the sync endpoint; `ohya devices invite` issues a join
// code; a real `ohya join` child pairs with it and uploads the fixture
// sessions; and `ohya mcp --remote` serves the sql and annotate tools to a
// real MCP client over the same tailnet. Everything runs on loopback with
// free ports and per-test state directories — no external coordination
// service, matching the local-first control-plane decision of ADR 0002.
package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// freeTCPPort reserves one free loopback TCP port and releases it.
func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

// freeUDPPort reserves one free loopback UDP port and releases it.
func freeUDPPort(t *testing.T) int {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	return pc.LocalAddr().(*net.UDPAddr).Port
}

// stopChild terminates a child gracefully and requires a clean exit.
func stopChild(t *testing.T, cmd *exec.Cmd, stderr *bytes.Buffer, timeout time.Duration) {
	t.Helper()
	if cmd.Process == nil || cmd.ProcessState != nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("child exited with %v; stderr:\n%s", err, stderr.String())
		}
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		cmd.Wait()
		t.Fatalf("child did not stop within %s; stderr:\n%s", timeout, stderr.String())
	}
}

// hubCounts reads the hub database read-only: its events, sessions and
// annotations, proving the joined device's uploads committed through serve.
func hubCounts(t *testing.T, dbPath string) (events, sessions, annotations int) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []struct {
		sql  string
		dest *int
	}{
		{"SELECT count(*) FROM events", &events},
		{"SELECT count(*) FROM sessions", &sessions},
		{"SELECT count(*) FROM event_annotations", &annotations},
	} {
		if err := db.QueryRow(q.sql).Scan(q.dest); err != nil {
			t.Fatalf("query %q: %v", q.sql, err)
		}
	}
	return events, sessions, annotations
}

// readDeviceID reads a state directory's device identity file.
func readDeviceID(t *testing.T, stateDir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(stateDir, "device.json"))
	if err != nil {
		t.Fatal(err)
	}
	var id struct {
		DeviceID string `json:"device_id"`
	}
	if err := json.Unmarshal(data, &id); err != nil {
		t.Fatal(err)
	}
	return id.DeviceID
}

// e2eCall calls one tool on the remote bridge session and decodes its
// structured content into out.
func e2eCall(t *testing.T, ctx context.Context, session *mcp.ClientSession, tool string, args map[string]any, out any) {
	t.Helper()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s): %v", tool, err)
	}
	if res.IsError {
		t.Fatalf("tool %s errored: %+v", tool, res)
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("decode %s result %s: %v", tool, raw, err)
	}
}

// waitServeReady waits until the serve child prints its serving line, and
// fails with its collected log otherwise.
func waitServeReady(t *testing.T, lines <-chan string, serveLog *bytes.Buffer) {
	t.Helper()
	timer := time.NewTimer(120 * time.Second)
	defer timer.Stop()
	for {
		select {
		case line := <-lines:
			if strings.Contains(line, "serving devices at") {
				t.Logf("serve: %s", line)
				return
			}
		case <-timer.C:
			t.Fatalf("serve never became ready; stderr:\n%s", serveLog.String())
		}
	}
}

// TestServeJoinEndToEnd walks the whole device path: serve provisions the
// coordination plane and serves the endpoint; invite issues one join code;
// join pairs a device and uploads the fixture sessions; the remote MCP
// bridge queries and annotates the shared history through the same bounded
// engine, with the annotation writer derived from the authenticated device.
func TestServeJoinEndToEnd(t *testing.T) {
	if os.Getenv("CI") != "" && runtime.GOOS == "darwin" {
		t.Skip("darwin CI: the pinned transport's macOS runtime behavior is unverified (backlog T046); the stack runs on the development platform")
	}
	if testing.Short() {
		t.Skip("end-to-end stack test")
	}
	dir := t.TempDir()
	hubState := filepath.Join(dir, "hub-state")
	hubDB := filepath.Join(dir, "hub", "hub.db")
	devState := filepath.Join(dir, "dev-state")
	claudeRoot := filepath.Join(dir, "claude-projects")
	if err := os.MkdirAll(claudeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile(filepath.Join("..", "..", "testdata", "claude-code", "main-session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeRoot, "session.jsonl"), fixture, 0o644); err != nil {
		t.Fatal(err)
	}
	const wantEvents = 16 // the fixture's complete records

	ctrlAddr := fmt.Sprintf("127.0.0.1:%d", freeTCPPort(t))
	stunAddr := fmt.Sprintf("127.0.0.1:%d", freeUDPPort(t))

	// The hub: a real serve child on free loopback ports.
	serveLog := &bytes.Buffer{}
	serveCmd := exec.Command(os.Args[0], "child-serve",
		"--db", hubDB, "--state", hubState,
		"--control-listen", ctrlAddr, "--stun-listen", stunAddr)
	serveCmd.Stderr = serveLog
	serveOut, err := serveCmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := serveCmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if serveCmd.ProcessState == nil {
			_ = serveCmd.Process.Kill()
			serveCmd.Wait()
		}
	}()
	lines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(serveOut)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	waitServeReady(t, lines, serveLog)

	// One join code from the real invite surface.
	var invOut bytes.Buffer
	if err := runDevicesInvite([]string{"--state", hubState}, &invOut, os.Stderr); err != nil {
		t.Fatal(err)
	}
	code := strings.SplitN(strings.TrimSpace(invOut.String()), "\n", 2)[0]
	if !strings.Contains(code, ":") {
		t.Fatalf("join code = %q; want invitation and preauth key halves", code)
	}

	// The device: a real join child, pairing with the code and uploading the
	// fixture sessions to the hub.
	joinLog := &bytes.Buffer{}
	joinCmd := exec.Command(os.Args[0], "child-join",
		"--control", "http://"+ctrlAddr,
		"--state", devState,
		"--claude-root", claudeRoot, "--codex-root", "", "--pi-root", "",
		"--interval", "500ms",
		code)
	joinCmd.Stderr = joinLog
	if err := joinCmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if joinCmd.ProcessState == nil {
			_ = joinCmd.Process.Kill()
			joinCmd.Wait()
		}
	}()

	// Wait for the fixture to land in the hub database.
	upCtx, upCancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer upCancel()
	for {
		events, sessions, _ := hubCounts(t, hubDB)
		if events >= wantEvents && sessions >= 1 {
			break
		}
		select {
		case <-upCtx.Done():
			t.Fatalf("joined events never reached the hub (have %d of %d); serve stderr:\n%s\njoin stderr:\n%s",
				events, wantEvents, serveLog.String(), joinLog.String())
		case <-time.After(500 * time.Millisecond):
		}
	}
	t.Log("joined device's fixture sessions reached the hub")

	// The remote MCP bridge in the README's bare form: a real mcp child
	// with `--remote` alone (the default hub address), over the joined
	// device's credential and transport node, driven by a real MCP client.
	mcpCmd := exec.Command(os.Args[0], "child-mcp",
		"--remote",
		"--control", "http://"+ctrlAddr,
		"--state", devState)
	client := mcp.NewClient(&mcp.Implementation{Name: "e2e-client", Version: "v0"}, nil)
	mcpCtx, mcpCancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer mcpCancel()
	session, err := client.Connect(mcpCtx, &mcp.CommandTransport{Command: mcpCmd}, nil)
	if err != nil {
		t.Fatalf("remote mcp connect: %v; serve stderr:\n%s\njoin stderr:\n%s",
			err, serveLog.String(), joinLog.String())
	}
	defer session.Close()

	var count struct {
		Rows [][]any `json:"rows"`
	}
	e2eCall(t, mcpCtx, session, "sql",
		map[string]any{"query": "SELECT count(*) AS n FROM events"}, &count)
	if len(count.Rows) != 1 || fmt.Sprint(count.Rows[0][0]) != fmt.Sprint(wantEvents) {
		t.Fatalf("remote count rows = %+v; want %d events", count.Rows, wantEvents)
	}

	// Annotate one event through the bridge; the hub derives the writer from
	// the authenticated device, and the database records it.
	var ann struct {
		Applied   int `json:"applied"`
		Unchanged int `json:"unchanged"`
	}
	e2eCall(t, mcpCtx, session, "annotate", map[string]any{
		"operations": []map[string]any{{
			"op": "upsert", "event_id": 1, "namespace": "e2e", "key": "proof",
			"value": map[string]string{"status": "annotated"},
		}},
	}, &ann)
	if ann.Applied != 1 {
		t.Fatalf("remote annotate applied = %d; want 1", ann.Applied)
	}

	events, sessions, annotations := hubCounts(t, hubDB)
	if events != wantEvents || annotations != 1 {
		t.Fatalf("hub holds %d events, %d sessions, %d annotations; want %d, >=1, 1",
			events, sessions, annotations, wantEvents)
	}
	roDB, err := sql.Open("sqlite3", "file:"+hubDB+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer roDB.Close()
	var writer string
	if err := roDB.QueryRow(`SELECT writer_id FROM event_annotations`).Scan(&writer); err != nil {
		t.Fatal(err)
	}
	if want := readDeviceID(t, devState); writer != want {
		t.Fatalf("annotation writer = %q; want the joined device identity %q", writer, want)
	}

	// Both children stop cleanly on SIGTERM.
	stopChild(t, joinCmd, joinLog, 30*time.Second)
	stopChild(t, serveCmd, serveLog, 30*time.Second)
}
