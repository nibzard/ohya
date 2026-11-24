package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nibzard/ohya/internal/antigravity"
	"github.com/nibzard/ohya/internal/store"
)

func TestAntigravityDiscoveryFiltersToTranscriptFullOnly(t *testing.T) {
	brain := t.TempDir()
	convID := "11111111-2222-4333-8444-555555555555"
	logsDir := filepath.Join(brain, convID, ".system_generated", "logs")
	chunksDir := filepath.Join(logsDir, "chunks", "transcript_full")
	scratchDir := filepath.Join(brain, convID, "scratch")
	for _, d := range []string{logsDir, chunksDir, scratchDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	wantPath := filepath.Join(logsDir, "transcript_full.jsonl")
	for _, p := range []string{
		wantPath,
		filepath.Join(logsDir, "transcript.jsonl"),
		filepath.Join(chunksDir, "00000000.jsonl"),
		filepath.Join(scratchDir, "notes.jsonl"),
	} {
		if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	files, _, err := discoverAgentSessionFiles(antigravity.AgentName, []agentDir{{dir: brain}}, nil, false)
	if err != nil {
		t.Fatalf("discoverAgentSessionFiles: %v", err)
	}
	if len(files) != 1 || files[0] != wantPath {
		t.Fatalf("discovered = %v, want [%s]", files, wantPath)
	}
}

func TestAntigravityIngestEndToEndAndSearch(t *testing.T) {
	root := t.TempDir()
	brain := filepath.Join(root, "brain")
	convID := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	logsDir := filepath.Join(brain, convID, ".system_generated", "logs")
	if err := os.MkdirAll(logsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile(filepath.Join("..", "..", "testdata", "antigravity", "main-session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logsDir, "transcript_full.jsonl"), fixture, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logsDir, "transcript.jsonl"), []byte("ignored compact\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeConversationIndex(t, filepath.Join(root, conversationIndex))

	dbPath := filepath.Join(root, "hub.db")
	stateDir := filepath.Join(root, "state")
	var stdout, stderr bytes.Buffer
	code := cliMain([]string{
		"ingest", "--once",
		"--db", dbPath,
		"--state", stateDir,
		"--claude-root", "",
		"--codex-root", "",
		"--pi-root", "",
		"--antigravity-root", brain,
	}, &stdout, &stderr)
	if code != 0 || stderr.Len() > 0 {
		t.Fatalf("ingest exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "imported 6 events from 1 sources") {
		t.Fatalf("unexpected ingest summary: %q", stdout.String())
	}

	hub, err := store.OpenReadOnly(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	reader, err := hub.Reader()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var agent, nativeID, prompt string
	if err := reader.QueryRowContext(context.Background(),
		`SELECT agent, native_id, first_prompt FROM sessions`).Scan(&agent, &nativeID, &prompt); err != nil {
		t.Fatal(err)
	}
	if agent != antigravity.AgentName || nativeID != convID || prompt != "fix the auth bug" {
		t.Fatalf("session = (%q, %q, %q)", agent, nativeID, prompt)
	}
}

func TestDefaultAntigravityRootEnvOverride(t *testing.T) {
	t.Setenv("AGY_DATA_DIR", "")
	if got := defaultAntigravityRoot("/home/me"); got != filepath.Join("/home/me", ".gemini", "antigravity-cli", "brain") {
		t.Fatalf("default = %q", got)
	}
	t.Setenv("AGY_DATA_DIR", "/srv/agy")
	if got := defaultAntigravityRoot("/home/me"); got != filepath.Join("/srv/agy", "brain") {
		t.Fatalf("AGY_DATA_DIR override = %q", got)
	}
}
