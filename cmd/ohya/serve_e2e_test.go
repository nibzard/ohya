package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	cryptoRand "crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/modelcontextprotocol/go-sdk/mcp"
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

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func freeUDPPort(t *testing.T) int {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	return pc.LocalAddr().(*net.UDPAddr).Port
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

func TestServeJoinEndToEnd(t *testing.T) {
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

	ctrlAddr := fmt.Sprintf("127.0.0.1:%d", freeTCPPort(t))
	stunAddr := fmt.Sprintf("127.0.0.1:%d", freeUDPPort(t))

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

	var invOut bytes.Buffer
	if err := runDevicesInvite([]string{"--state", hubState}, &invOut, os.Stderr); err != nil {
		t.Fatal(err)
	}
	code := strings.SplitN(strings.TrimSpace(invOut.String()), "\n", 2)[0]
	if !strings.Contains(code, ":") {
		t.Fatalf("join code = %q; want invitation and preauth key halves", code)
	}

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

	secondCmd := exec.Command(os.Args[0], "child-mcp",
		"--remote", "--control", "http://"+ctrlAddr, "--state", devState)
	secondSession, err := client.Connect(mcpCtx, &mcp.CommandTransport{Command: secondCmd}, nil)
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
		appendDeadline := time.Now().Add(timeout)
		for {
			events, _, _ := hubCounts(t, hubDB)
			if events == want {
				break
			}
			if time.Now().After(appendDeadline) {
				t.Fatalf("live append while remote MCP runs: have %d events, want %d", events, want)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	appendAndWait("concurrent-append", wantEvents+1, 15*time.Second)
	assertRemoteCount(session, wantEvents+1)
	assertRemoteCount(secondSession, wantEvents+1)
	t.Log("live append reached the hub while two remote MCP clients remained connected")

	if err := joinCmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := joinCmd.Wait(); err == nil {
		t.Fatal("killed join exited successfully")
	}
	assertRemoteCount(session, wantEvents+1)
	assertRemoteCount(secondSession, wantEvents+1)
	t.Log("remote MCP recovered after the transport owner was killed")

	restartedLog := &bytes.Buffer{}
	restartedJoin := exec.Command(os.Args[0], "child-join",
		"--control", "http://"+ctrlAddr, "--state", devState,
		"--claude-root", claudeRoot, "--codex-root", "", "--pi-root", "",
		"--interval", "100ms")
	restartedJoin.Stderr = restartedLog
	if err := restartedJoin.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if restartedJoin.ProcessState == nil {
			restartedJoin.Process.Kill()
			restartedJoin.Wait()
		}
	}()
	appendAndWait("after-owner-transfer", wantEvents+2, 30*time.Second)
	assertRemoteCount(session, wantEvents+2)
	assertRemoteCount(secondSession, wantEvents+2)
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	appendAndWait("after-mcp-owner-close", wantEvents+3, 30*time.Second)
	assertRemoteCount(secondSession, wantEvents+3)
	t.Log("the uploader recovered when the MCP transport owner stopped")
	stopChild(t, restartedJoin, restartedLog, 30*time.Second)
	assertRemoteCount(secondSession, wantEvents+3)
	t.Log("the remaining MCP client took ownership after the uploader stopped")

	var revokeOutput bytes.Buffer
	if err := runDevicesRevoke([]string{"--state", hubState, readDeviceID(t, devState)}, &revokeOutput, io.Discard); err != nil {
		t.Fatal(err)
	}
	result, err := secondSession.CallTool(mcpCtx, &mcp.CallToolParams{
		Name: "sql", Arguments: map[string]any{"query": "SELECT count(*) FROM events"},
	})
	if err != nil || !result.IsError {
		t.Fatalf("revoked remote call: result %+v, err %v", result, err)
	}
	t.Log("revocation still denies remote queries after transport ownership transfers")

	stopChild(t, serveCmd, serveLog, 30*time.Second)
}

func e2eSelfSignedCert(t *testing.T, dir string) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), cryptoRand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := cryptoRand.Int(cryptoRand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(cryptoRand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func TestServeJoinTLS(t *testing.T) {
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
	certPath, keyPath := e2eSelfSignedCert(t, dir)
	ctrlAddr := fmt.Sprintf("127.0.0.1:%d", freeTCPPort(t))
	stunAddr := fmt.Sprintf("127.0.0.1:%d", freeUDPPort(t))

	serveLog := &bytes.Buffer{}
	serveCmd := exec.Command(os.Args[0], "child-serve",
		"--db", hubDB, "--state", hubState,
		"--control-listen", ctrlAddr, "--stun-listen", stunAddr,
		"--tls-cert", certPath, "--tls-key", keyPath)
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
	if strings.Contains(serveLog.String(), "bad certificate") {
		t.Fatalf("serve rejected its own plane certificate; stderr:\n%s", serveLog.String())
	}

	var invOut bytes.Buffer
	if err := runDevicesInvite([]string{"--state", hubState}, &invOut, os.Stderr); err != nil {
		t.Fatal(err)
	}
	code := strings.SplitN(strings.TrimSpace(invOut.String()), "\n", 2)[0]

	joinLog := &bytes.Buffer{}
	joinCmd := exec.Command(os.Args[0], "child-join",
		"--control", "https://"+ctrlAddr,
		"--state", devState,
		"--claude-root", claudeRoot, "--codex-root", "", "--pi-root", "",
		"--interval", "500ms",
		code)
	joinCmd.Stderr = joinLog
	joinCmd.Env = append(os.Environ(), "SSL_CERT_FILE="+certPath)
	if err := joinCmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if joinCmd.ProcessState == nil {
			_ = joinCmd.Process.Kill()
			joinCmd.Wait()
		}
	}()

	upCtx, upCancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer upCancel()
	for {
		events, sessions, _ := hubCounts(t, hubDB)
		if events >= wantEvents && sessions >= 1 {
			break
		}
		select {
		case <-upCtx.Done():
			t.Fatalf("joined events never reached the hub over TLS (have %d of %d); serve stderr:\n%s\njoin stderr:\n%s",
				events, wantEvents, serveLog.String(), joinLog.String())
		case <-time.After(500 * time.Millisecond):
		}
	}
	stopChild(t, joinCmd, joinLog, 30*time.Second)

	var devOut bytes.Buffer
	if err := runDevicesList([]string{"--state", hubState, "--json"}, &devOut, os.Stderr); err != nil {
		t.Fatal(err)
	}
	var listed struct {
		Data struct {
			Devices []struct {
				ID    string `json:"id"`
				State string `json:"state"`
			} `json:"devices"`
		} `json:"data"`
	}
	if err := json.Unmarshal(devOut.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Data.Devices) != 1 || listed.Data.Devices[0].State != "active" {
		t.Fatalf("devices = %+v; want one active device", listed.Data.Devices)
	}
	events, sessions, _ := hubCounts(t, hubDB)
	if events != wantEvents || sessions < 1 {
		t.Fatalf("hub holds %d events, %d sessions; want %d, >=1", events, sessions, wantEvents)
	}
	stopChild(t, serveCmd, serveLog, 30*time.Second)
	if strings.Contains(serveLog.String(), "bad certificate") {
		t.Fatalf("TLS handshakes failed against the plane; stderr:\n%s", serveLog.String())
	}
}
