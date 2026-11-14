package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nibzard/ohya/internal/events"
	"github.com/nibzard/ohya/internal/store"
)

// ingestFixtureOnce ingests the main Claude Code fixture through the
// production parser and write path, the way `ohya ingest` does, and returns
// the open hub plus the device ID that owns the source.
func ingestFixtureOnce(t *testing.T, dbPath string) (*store.DB, string) {
	t.Helper()
	data, err := os.ReadFile(fixturePath("main-session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	hub, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hub.Close() })
	deviceID := "0123456789abcdef0123456789abcdef"
	if err := hub.RegisterSource(context.Background(), "src-1", deviceID, "claude-code", "/main-session.jsonl"); err != nil {
		t.Fatal(err)
	}
	parsed, err := events.Parse("claude-code", data, 0, events.Options{SourceScope: "src-1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hub.Ingest(context.Background(), store.Batch{
		SourceID: "src-1", DeviceID: deviceID, Events: parsed.Events,
	}); err != nil {
		t.Fatal(err)
	}
	return hub, deviceID
}

func TestRederiveCommandIsNoOpWhenCurrent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "hub.db")
	hub, _ := ingestFixtureOnce(t, dbPath)
	before := countRows(t, hub.Writer(), `SELECT count(*) FROM events`)

	var stdout, stderr bytes.Buffer
	if err := runRederiveCommand([]string{"--db", dbPath}, &stdout, &stderr); err != nil {
		t.Fatalf("rederive: %v (%s)", err, stderr.String())
	}
	if out := stdout.String(); !strings.Contains(out, "nothing re-derived") {
		t.Fatalf("output = %q, want the no-op report", out)
	}
	// The no-op commits nothing: the rows and their version stamps stand.
	if after := countRows(t, hub.Writer(), `SELECT count(*) FROM events`); after != before {
		t.Fatalf("events after rederive = %d, want %d", after, before)
	}
	var stale int
	if err := hub.Writer().QueryRow(
		`SELECT count(*) FROM events WHERE parser_version < ?`, eventsClaudeVersion()).Scan(&stale); err != nil || stale != 0 {
		t.Fatalf("stale rows after rederive = %d, %v; want 0", stale, err)
	}
}

// eventsClaudeVersion reads the Claude Code parser's output version without
// importing internal/claudecode here; the binding under test carries it.
func eventsClaudeVersion() int {
	for _, p := range events.Derivers() {
		if p.Agent == "claude-code" {
			return p.OutputVersion
		}
	}
	return 0
}
