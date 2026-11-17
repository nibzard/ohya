package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"tailscale.com/derp/derpserver"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"

	"github.com/nibzard/ohya/internal/device"
)

func transportTestDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ohya-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

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

func waitServeReady(t *testing.T, lines <-chan string, serveLog *bytes.Buffer) string {
	t.Helper()
	timer := time.NewTimer(120 * time.Second)
	defer timer.Stop()
	for {
		select {
		case line := <-lines:
			if strings.Contains(line, "serving devices on the tailcat transport") {
				t.Logf("serve: %s", line)
				return line
			}
		case <-timer.C:
			t.Fatalf("serve never became ready; stderr:\n%s", serveLog.String())
		}
	}
}

func startLocalDERP(t *testing.T) *tailcfg.DERPRegion {
	t.Helper()
	srv := derpserver.New(key.NewNode(), logger.Discard)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the DERP relay: %v", err)
	}
	httpsrv := httptest.NewUnstartedServer(derpserver.Handler(srv))
	httpsrv.Listener.Close()
	httpsrv.Listener = ln
	httpsrv.Config.TLSNextProto = make(map[string]func(*http.Server, *tls.Conn, http.Handler))
	httpsrv.StartTLS()
	t.Cleanup(func() {
		httpsrv.CloseClientConnections()
		httpsrv.Close()
		srv.Close()
	})
	port := httpsrv.Listener.Addr().(*net.TCPAddr).Port
	return &tailcfg.DERPRegion{
		RegionID:   1,
		RegionCode: "ohyatest",
		RegionName: "ohya local test relay",
		Nodes: []*tailcfg.DERPNode{{
			Name:             "ohyatest1",
			RegionID:         1,
			HostName:         "127.0.0.1",
			IPv4:             "127.0.0.1",
			IPv6:             "none",
			DERPPort:         port,
			InsecureForTests: true,
		}},
	}
}

func writeDERPRegionFile(t *testing.T, region *tailcfg.DERPRegion) string {
	t.Helper()
	data, err := json.Marshal(region)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "derp-region.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runChildServe(args []string) error {
	fs := flag.NewFlagSet("child-serve", flag.ContinueOnError)
	db := fs.String("db", "", "hub database path")
	state := fs.String("state", "", "state directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := os.Getenv("OHYA_TEST_DERP_REGION_FILE")
	if path == "" {
		return errors.New("child-serve needs OHYA_TEST_DERP_REGION_FILE")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	region := &tailcfg.DERPRegion{}
	if err := json.Unmarshal(data, region); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runServe(ctx, serveOptions{
		dbPath:   *db,
		stateDir: *state,
		stdout:   os.Stdout,
		region:   region,
	})
}

func startServeChild(t *testing.T, hubDB, hubState, regionFile string) (*exec.Cmd, *bytes.Buffer, <-chan string, string) {
	t.Helper()
	log := &bytes.Buffer{}
	cmd := exec.Command(os.Args[0], "child-serve",
		"--db", hubDB, "--state", hubState)
	cmd.Env = append(os.Environ(), "OHYA_TEST_DERP_REGION_FILE="+regionFile)
	cmd.Stderr = log
	serveOut, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	lines := make(chan string, 16)
	go func() {
		sc := bufio.NewScanner(serveOut)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	ready := waitServeReady(t, lines, log)
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			cmd.Wait()
		}
	})
	return cmd, log, lines, ready
}

func inviteCode(t *testing.T, hubState string) string {
	t.Helper()
	var out bytes.Buffer
	if err := runDevicesInvite([]string{"--state", hubState}, &out, os.Stderr); err != nil {
		t.Fatal(err)
	}
	code := strings.SplitN(strings.TrimSpace(out.String()), "\n", 2)[0]
	if _, _, err := parseJoinCode(code); err != nil {
		t.Fatalf("invite printed %q: %v", code, err)
	}
	return code
}

func startJoinChild(t *testing.T, devState, claudeRoot, code string) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	args := []string{
		"child-join",
		"--state", devState,
		"--claude-root", claudeRoot, "--codex-root", "", "--pi-root", "",
		"--interval", "500ms",
	}
	if code != "" {
		args = append(args, code)
	}
	log := &bytes.Buffer{}
	cmd := exec.Command(os.Args[0], args...)
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			cmd.Wait()
		}
	})
	return cmd, log
}

func waitForEvents(t *testing.T, hubDB string, want int, serveLog, joinLog *bytes.Buffer) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	for {
		events, _, _ := hubCounts(t, hubDB)
		if events >= want {
			return
		}
		select {
		case <-ctx.Done():
			messages := ""
			if serveLog != nil {
				messages += serveLog.String()
			}
			if joinLog != nil {
				messages += joinLog.String()
			}
			t.Fatalf("joined events never reached the hub (have %d of %d); stderr:\n%s", events, want, messages)
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func TestServeJoinEndToEnd(t *testing.T) {
	if os.Getenv("CI") != "" && runtime.GOOS == "darwin" {
		t.Skip("darwin CI: the pinned transport's macOS runtime behavior is unverified (backlog T046)")
	}
	if testing.Short() {
		t.Skip("end-to-end stack test")
	}
	dir := transportTestDir(t)
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
	const wantEvents = 16

	regionFile := writeDERPRegionFile(t, startLocalDERP(t))
	serveCmd, serveLog, _, _ := startServeChild(t, hubDB, hubState, regionFile)
	code := inviteCode(t, hubState)
	joinCmd, joinLog := startJoinChild(t, devState, claudeRoot, code)
	waitForEvents(t, hubDB, wantEvents, serveLog, joinLog)
	t.Log("joined device's fixture sessions reached the hub")

	savedAddr, err := device.LoadHubAddress(devState)
	if err != nil {
		t.Fatalf("paired device saved no hub address: %v", err)
	}
	if !strings.HasPrefix(savedAddr, "tc") {
		t.Fatalf("saved hub address %q is not a tailcat address", savedAddr)
	}

	mcpCmdFor := func() *exec.Cmd {
		cmd := exec.Command(os.Args[0], "child-mcp", "--remote", "--state", devState)
		cmd.Stderr = serveLog
		return cmd
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "e2e-client", Version: "v0"}, nil)
	mcpCtx, mcpCancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer mcpCancel()
	session, err := client.Connect(mcpCtx, &mcp.CommandTransport{Command: mcpCmdFor()}, nil)
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

	secondSession, err := client.Connect(mcpCtx, &mcp.CommandTransport{Command: mcpCmdFor()}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer secondSession.Close()
	assertRemoteCount := func(remote *mcp.ClientSession, want int) {
		t.Helper()
		e2eCall(t, mcpCtx, remote, "sql",
			map[string]any{"query": "SELECT count(*) AS n FROM events"}, &count)
		if len(count.Rows) != 1 || fmt.Sprint(count.Rows[0][0]) != fmt.Sprint(want) {
			t.Fatalf("remote count = %+v; want %d", count.Rows, want)
		}
	}
	assertRemoteCount(secondSession, wantEvents)
	t.Log("two remote MCP processes serve queries next to the uploader")

	appendAndWait := func(uuid string, want int, timeout time.Duration) {
		t.Helper()
		appendFile, err := os.OpenFile(filepath.Join(claudeRoot, "session.jsonl"), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		record := fmt.Sprintf(`{"type":"user","uuid":%q,"sessionId":"00000000-0000-4000-8000-0000000000a1","message":{"role":"user","content":"Append while remote MCP queries the hub."},"timestamp":"2026-11-13T10:00:00Z"}`, uuid)
		_, writeErr := appendFile.WriteString(record + "\n")
		closeErr := appendFile.Close()
		if writeErr != nil || closeErr != nil {
			t.Fatalf("append: write %v, close %v", writeErr, closeErr)
		}
		deadline := time.Now().Add(timeout)
		for {
			events, _, _ := hubCounts(t, hubDB)
			if events == want {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("live append while remote MCP runs: have %d events, want %d", events, want)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	appendAndWait("concurrent-append", wantEvents+1, 30*time.Second)
	assertRemoteCount(session, wantEvents+1)
	assertRemoteCount(secondSession, wantEvents+1)
	t.Log("live append reached the hub while two remote MCP clients stayed connected")

	if err := joinCmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := joinCmd.Wait(); err == nil {
		t.Fatal("killed join exited successfully")
	}
	assertRemoteCount(session, wantEvents+1)
	assertRemoteCount(secondSession, wantEvents+1)
	t.Log("remote MCP kept working after the uploader stopped; each process dials on its own")

	restartedJoin, restartedLog := startJoinChild(t, devState, claudeRoot, "")
	appendAndWait("after-uploader-restart", wantEvents+2, 60*time.Second)
	assertRemoteCount(session, wantEvents+2)
	assertRemoteCount(secondSession, wantEvents+2)

	var revokeOutput bytes.Buffer
	if err := runDevicesRevoke([]string{"--state", hubState, readDeviceID(t, devState)}, &revokeOutput, io.Discard); err != nil {
		t.Fatal(err)
	}
	result, err := session.CallTool(mcpCtx, &mcp.CallToolParams{
		Name: "sql", Arguments: map[string]any{"query": "SELECT count(*) FROM events"},
	})
	if err != nil || !result.IsError {
		t.Fatalf("revoked remote call: result %+v, err %v", result, err)
	}
	t.Log("revocation still denies remote queries")

	stopChild(t, restartedJoin, restartedLog, 30*time.Second)
	stopChild(t, serveCmd, serveLog, 30*time.Second)
}
