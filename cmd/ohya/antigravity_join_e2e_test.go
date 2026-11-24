package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func startAntigravityJoinChild(t *testing.T, devState, brain, code string) (*exec.Cmd, *bytes.Buffer) {
	t.Helper()
	args := []string{
		"child-join",
		"--state", devState,
		"--claude-root", "", "--codex-root", "", "--pi-root", "", "--antigravity-root", brain,
		"--interval", "500ms",
		code,
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

func TestServeJoinCarriesTheSourceWorkspace(t *testing.T) {
	if os.Getenv("CI") != "" && runtime.GOOS == "darwin" {
		t.Skip("darwin CI: the pinned transport's macOS runtime behavior is unverified (backlog T046)")
	}
	if testing.Short() {
		t.Skip("end-to-end stack test")
	}
	t.Parallel()
	dir := transportTestDir(t)
	hubState := filepath.Join(dir, "hub-state")
	hubDB := filepath.Join(dir, "hub", "hub.db")
	devState := filepath.Join(dir, "dev-state")
	brain := filepath.Join(dir, "agy", "brain")
	app := gitCheckout(t, "https://github.com/nibzard/agy-app.git")
	writeFile(t, transcriptPath(brain, agyConversation), agyFirstRecord+agySecondRecord)
	writeConversationIndex(t, filepath.Join(dir, "agy", conversationIndex), [2]any{agyConversation, `["` + fileURI(app) + `"]`})

	regionFile := writeDERPRegionFile(t, startLocalDERP(t))
	serveCmd, serveLog, _, _ := startServeChild(t, hubDB, hubState, regionFile)
	code := inviteCode(t, hubState)
	joinCmd, joinLog := startAntigravityJoinChild(t, devState, brain, code)
	waitForEvents(t, hubDB, 2, serveLog, joinLog)

	deadline := time.Now().Add(60 * time.Second)
	for {
		cwd, repo := antigravitySession(t, hubDB)
		if cwd.Valid && cwd.String == app && repo.Valid && repo.String == "github.com/nibzard/agy-app" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session = (%+v, %+v); want the workspace %q and its repository; join stderr:\n%s",
				cwd, repo, app, joinLog.String())
		}
		time.Sleep(200 * time.Millisecond)
	}

	stopChild(t, joinCmd, joinLog, 30*time.Second)
	stopChild(t, serveCmd, serveLog, 30*time.Second)
}
