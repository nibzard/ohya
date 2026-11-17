package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func runRemoteMcpWithClosedStdin(t *testing.T, devState string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "child-mcp", "--remote", "--state", devState)
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("ohya mcp --remote with a closed stdin exited with %v; stderr:\n%s", err, stderr.String())
	}
	if out := stderr.String(); strings.Contains(out, "panic") {
		t.Fatalf("ohya mcp --remote with a closed stdin panicked; stderr:\n%s", out)
	}
}

func TestNeverDialedDeviceExitsCleanly(t *testing.T) {
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
	stopChild(t, joinCmd, joinLog, 30*time.Second)

	t.Log("paired device: mcp --remote with a closed stdin exits cleanly")
	runRemoteMcpWithClosedStdin(t, devState)

	var revokeOutput bytes.Buffer
	if err := runDevicesRevoke([]string{"--state", hubState, readDeviceID(t, devState)}, &revokeOutput, io.Discard); err != nil {
		t.Fatal(err)
	}
	t.Log("revoked device: mcp --remote with a closed stdin exits cleanly")
	runRemoteMcpWithClosedStdin(t, devState)

	stopChild(t, serveCmd, serveLog, 30*time.Second)

	resumeCmd, resumeLog := startJoinChild(t, devState, claudeRoot, "")
	deadline := time.Now().Add(90 * time.Second)
	for !strings.Contains(resumeLog.String(), "hub unreachable") {
		if resumeCmd.ProcessState != nil {
			t.Fatalf("resume join exited early with %v; stderr:\n%s", resumeCmd.ProcessState, resumeLog.String())
		}
		if time.Now().After(deadline) {
			t.Fatalf("resume join never logged a failed dial against the stopped hub; stderr:\n%s", resumeLog.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err := resumeCmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- resumeCmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("join interrupted with SIGINT while the hub is unreachable exited with %v; stderr:\n%s", err, resumeLog.String())
		}
	case <-time.After(30 * time.Second):
		_ = resumeCmd.Process.Kill()
		resumeCmd.Wait()
		t.Fatalf("join did not exit after SIGINT; stderr:\n%s", resumeLog.String())
	}
	if out := resumeLog.String(); strings.Contains(out, "panic") {
		t.Fatalf("join interrupted while the hub is unreachable panicked; stderr:\n%s", out)
	}
}
