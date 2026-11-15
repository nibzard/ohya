package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func TestHubRestartReconnect(t *testing.T) {
	if os.Getenv("CI") != "" && runtime.GOOS == "darwin" {
		t.Skip("darwin CI: the pinned transport's macOS runtime behavior is unverified (backlog T046)")
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
	sessionFile := filepath.Join(claudeRoot, "session.jsonl")
	if err := os.WriteFile(sessionFile, fixture, 0o644); err != nil {
		t.Fatal(err)
	}
	const wantEvents = 16

	ctrlAddr := fmt.Sprintf("127.0.0.1:%d", freeTCPPort(t))
	stunAddr := fmt.Sprintf("127.0.0.1:%d", freeUDPPort(t))

	startServe := func() (*exec.Cmd, *bytes.Buffer, <-chan string) {
		t.Helper()
		log := &bytes.Buffer{}
		cmd := exec.Command(os.Args[0], "child-serve",
			"--db", hubDB, "--state", hubState,
			"--control-listen", ctrlAddr, "--stun-listen", stunAddr)
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
		waitServeReady(t, lines, log)
		return cmd, log, lines
	}

	serveCmd, serveLog, _ := startServe()
	t.Cleanup(func() {
		if serveCmd.ProcessState == nil {
			_ = serveCmd.Process.Kill()
			serveCmd.Wait()
		}
	})

	var invOut bytes.Buffer
	if err := runDevicesInvite([]string{"--state", hubState}, &invOut, os.Stderr); err != nil {
		t.Fatal(err)
	}
	code := strings.SplitN(strings.TrimSpace(invOut.String()), "\n", 2)[0]

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
	t.Cleanup(func() {
		if joinCmd.ProcessState == nil {
			_ = joinCmd.Process.Kill()
			joinCmd.Wait()
		}
	})

	upCtx, upCancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer upCancel()
	for {
		events, _, _ := hubCounts(t, hubDB)
		if events >= wantEvents {
			break
		}
		select {
		case <-upCtx.Done():
			t.Fatalf("joined events never reached the hub (have %d of %d); serve stderr:\n%s\njoin stderr:\n%s",
				events, wantEvents, serveLog.String(), joinLog.String())
		case <-time.After(500 * time.Millisecond):
		}
	}
	t.Log("joined device's fixture sessions reached the hub; connection is established")
	time.Sleep(3 * time.Second)

	appendMarker := func(marker string) {
		t.Helper()
		f, err := os.OpenFile(sessionFile, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		record := fmt.Sprintf(`{"type":"user","uuid":%q,"sessionId":"00000000-0000-4000-8000-0000000000a1","message":{"role":"user","content":%q},"timestamp":"2026-11-13T10:00:00Z"}`+"\n",
			marker, marker)
		_, werr := f.WriteString(record)
		cerr := f.Close()
		if werr != nil || cerr != nil {
			t.Fatalf("append marker: write %v, close %v", werr, cerr)
		}
	}
	markerVisible := func(marker string) bool {
		t.Helper()
		db, err := sql.Open("sqlite3", "file:"+hubDB+"?mode=ro")
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM events WHERE text LIKE ?`,
			"%"+marker+"%").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n > 0
	}

	stopChild(t, serveCmd, serveLog, 30*time.Second)
	t.Log("hub stopped; restarting")
	appendMarker("hubrestart-marker")

	serveCmd, serveLog, _ = startServe()
	t.Cleanup(func() {
		if serveCmd.ProcessState == nil {
			_ = serveCmd.Process.Kill()
			serveCmd.Wait()
		}
	})

	const bound = 2 * time.Minute
	waitForMarker := func(marker string, restartStart time.Time) {
		t.Helper()
		for {
			if markerVisible(marker) {
				t.Logf("%s reached the hub %s after the restart", marker, time.Since(restartStart).Round(time.Millisecond))
				return
			}
			if time.Since(restartStart) > bound {
				t.Fatalf("the joined device never re-uploaded within %s after the hub restart; serve stderr:\n%s\njoin stderr:\n%s",
					bound, serveLog.String(), joinLog.String())
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	restartStart := time.Now()
	waitForMarker("hubrestart-marker", restartStart)
	if strings.Contains(serveLog.String(), "panic") {
		t.Fatalf("restarted hub panicked; stderr:\n%s", serveLog.String())
	}

	stopChild(t, serveCmd, serveLog, 30*time.Second)
	t.Log("second hub stop; restarting again")
	appendMarker("hubrestart-again")
	serveCmd, serveLog, _ = startServe()
	t.Cleanup(func() {
		if serveCmd.ProcessState == nil {
			_ = serveCmd.Process.Kill()
			serveCmd.Wait()
		}
	})
	restartStart = time.Now()
	waitForMarker("hubrestart-again", restartStart)
	if strings.Contains(serveLog.String(), "panic") {
		t.Fatalf("restarted hub panicked; stderr:\n%s", serveLog.String())
	}

	events, _, _ := hubCounts(t, hubDB)
	if want := wantEvents + 2; events != want {
		t.Fatalf("hub holds %d events after restarts; want %d (no duplicates, no losses)", events, want)
	}
	stopChild(t, joinCmd, joinLog, 30*time.Second)
	stopChild(t, serveCmd, serveLog, 30*time.Second)
}
