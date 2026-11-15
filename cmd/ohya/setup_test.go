package main

import (
	"bytes"
	"github.com/nibzard/ohya/internal/setup"
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

func TestRemoteSetupDryRunAndSecretHandling(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	t.Setenv("OHYA_TEST_JOIN_CODE", "synthetic-one-time:code")
	var stdout, stderr bytes.Buffer
	code := cliMain([]string{"setup", "--dry-run", "--no-agents", "--no-service", "--no-modify-path", "--state", state, "--join-code-env", "OHYA_TEST_JOIN_CODE", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("remote plan %d %s %s", code, stdout.String(), stderr.String())
	}
	if bytes.Contains(stdout.Bytes(), []byte("synthetic-one-time")) || bytes.Contains(stderr.Bytes(), []byte("synthetic-one-time")) {
		t.Fatal("join code leaked")
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatal("dry run created state")
	}
	stdout.Reset()
	stderr.Reset()
	code = cliMain([]string{"setup", "--local", "--remote", "--dry-run", "--json"}, &stdout, &stderr)
	if code != 2 {
		t.Fatalf("conflicting mode exit %d", code)
	}
}

func TestDoctorUnderstandsRemoteSetupWithoutCreatingLocalMemory(t *testing.T) {
	state := t.TempDir()
	if err := setup.SaveConnection(state, setup.Connection{Mode: "remote", ControlURL: "http://127.0.0.1:1", HubAddress: "hub:9027"}); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := cliMain([]string{"doctor", "--state", state, "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("remote doctor %d %s %s", code, stdout.String(), stderr.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("no local database is required")) {
		t.Fatal(stdout.String())
	}
	if _, err := os.Stat(filepath.Join(state, "hub.db")); !os.IsNotExist(err) {
		t.Fatal("created local database")
	}
	if _, err := os.Stat(filepath.Join(state, "join")); !os.IsNotExist(err) {
		t.Fatal("created remote state")
	}
}
