package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestSetupCommandDryRun(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	var stdout, stderr bytes.Buffer
	code := cliMain([]string{"setup", "--dry-run", "--no-agents", "--no-service", "--state", state, "--json"}, &stdout, &stderr)
	if code != 0 || !bytes.Contains(stdout.Bytes(), []byte(`"dry_run":true`)) {
		t.Fatalf("dry run = %d %s %s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("created state: %v", err)
	}
}

func TestSetupRejectsArguments(t *testing.T) {
	for _, command := range []string{"setup", "uninstall"} {
		var stdout, stderr bytes.Buffer
		if code := cliMain([]string{command, "extra"}, &stdout, &stderr); code != 2 {
			t.Fatalf("%s code = %d: %s", command, code, stderr.String())
		}
	}
}

func TestManagedIngestWaitsForMissingRoots(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	args := []string{"ingest", "--once", "--allow-empty", "--db", filepath.Join(dir, "hub.db"), "--state", filepath.Join(dir, "state"), "--claude-root", filepath.Join(dir, "not-yet-created"), "--codex-root", "", "--pi-root", ""}
	if code := cliMain(args, &stdout, &stderr); code != 0 {
		t.Fatalf("empty managed ingest = %d %s %s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "not-yet-created")); !os.IsNotExist(err) {
		t.Fatalf("created agent root: %v", err)
	}
}
