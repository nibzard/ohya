package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/nibzard/ohya/internal/device"
)

func TestHubRestartReconnect(t *testing.T) {
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
	sessionFile := filepath.Join(claudeRoot, "session.jsonl")
	if err := os.WriteFile(sessionFile, fixture, 0o644); err != nil {
		t.Fatal(err)
	}
	const wantEvents = 16

	regionFile := writeDERPRegionFile(t, startLocalDERP(t))
	serveCmd, serveLog, _, firstReady := startServeChild(t, hubDB, hubState, regionFile)
	code := inviteCode(t, hubState)
	firstNodeKey := readyNodeKey(t, firstReady)

	joinCmd, joinLog := startJoinChild(t, devState, claudeRoot, code)
	waitForEvents(t, hubDB, wantEvents, serveLog, joinLog)
	t.Log("joined device's fixture sessions reached the hub; connection is established")
	savedAddr, err := device.LoadHubAddress(devState)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(3 * time.Second)

	appendMarker := func(marker string) {
		t.Helper()
		f, err := os.OpenFile(sessionFile, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		record := `{"type":"user","uuid":"` + marker + `","sessionId":"00000000-0000-4000-8000-0000000000a1","message":{"role":"user","content":"` + marker + `"},"timestamp":"2026-11-13T10:00:00Z"}` + "\n"
		if _, err := f.WriteString(record); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
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

	serveCmd, serveLog, _, secondReady := startServeChild(t, hubDB, hubState, regionFile)
	if key := readyNodeKey(t, secondReady); key != firstNodeKey {
		t.Fatalf("restarted hub reports node key %q; want the saved %q", key, firstNodeKey)
	}
	if addr, err := device.LoadHubAddress(devState); err != nil || addr != savedAddr {
		t.Fatalf("device hub address changed across the restart: %q, %v; want %q", addr, err, savedAddr)
	}

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
	serveCmd, serveLog, _, _ = startServeChild(t, hubDB, hubState, regionFile)
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

func readyNodeKey(t *testing.T, readyLine string) string {
	t.Helper()
	_, after, ok := strings.Cut(readyLine, "node key ")
	if !ok {
		t.Fatalf("ready line names no node key: %q", readyLine)
	}
	return strings.TrimSpace(after)
}
