package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestIngestObservesLocalWorkspaces(t *testing.T) {
	e := newSliceEnv(t)
	repo := filepath.Join(t.TempDir(), "dioclet")
	writeFile(t, filepath.Join(repo, ".git", "config"), "[remote \"origin\"]\n\turl = https://github.com/nibzard/dioclet.git\n")
	sub := filepath.Join(repo, "web")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	line := func(session, cwd string) string {
		return fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":%q,"message":{"role":"user","content":"hi"}}`+"\n", session, cwd)
	}
	path := e.session("a.jsonl")
	writeFile(t, path, line("s1", sub)+line("s2", filepath.Join(t.TempDir(), "gone")))
	if _, stderr := e.runPass(t); stderr != "" {
		t.Fatalf("stderr: %s", stderr)
	}
	hub := openHub(t, e.dbPath)
	if n := countRows(t, hub.Writer(), `SELECT count(*) FROM sessions WHERE native_id = 's1' AND repo = 'github.com/nibzard/dioclet'`); n != 1 {
		t.Fatal("local ingestion did not fill the repository of a session in a git checkout")
	}
	if n := countRows(t, hub.Writer(), `SELECT count(*) FROM workspaces WHERE device_id = ? AND repo IS NULL`, e.deviceID(t)); n != 1 {
		t.Fatal("the deleted directory was not recorded as having no repository")
	}
}
