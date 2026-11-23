package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nibzard/ohya/internal/setup"
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

func TestSetupHubFlagValidation(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"remote", []string{"setup", "--hub", "--remote", "--state", state}, "cannot be combined with remote setup"},
		{"code env", []string{"setup", "--hub", "--join-code-env", "OHYA_X", "--state", state}, "cannot be combined with remote setup"},
		{"no service", []string{"setup", "--hub", "--no-service", "--state", state}, "omit --no-service"},
		{"bad ttl", []string{"setup", "--hub", "--ttl", "0s", "--state", state}, "--ttl must be positive"},
	}
	for _, tc := range cases {
		var stdout, stderr bytes.Buffer
		if code := cliMain(tc.args, &stdout, &stderr); code != 2 || !bytes.Contains(stderr.Bytes(), []byte(tc.want)) {
			t.Fatalf("%s: code %d stderr %q; want usage error naming %q", tc.name, code, stderr.String(), tc.want)
		}
	}
}

func TestSetupHubDryRunPlansServeService(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	var stdout, stderr bytes.Buffer
	code := cliMain([]string{"setup", "--hub", "--dry-run", "--no-agents", "--no-modify-path", "--state", state, "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("hub dry run %d %s %s", code, stdout.String(), stderr.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("ohya-serve.service")) && !bytes.Contains(stdout.Bytes(), []byte("com.nibzard.ohya.serve")) {
		t.Fatalf("dry run planned no hub service: %s", stdout.String())
	}
	if bytes.Contains(stdout.Bytes(), []byte("code_secret")) {
		t.Fatalf("dry run issued a join code: %s", stdout.String())
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("dry run created state: %v", err)
	}
}

func TestManagedIngestWaitsForMissingRoots(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	args := []string{"ingest", "--once", "--allow-empty", "--db", filepath.Join(dir, "hub.db"), "--state", filepath.Join(dir, "state"), "--claude-root", filepath.Join(dir, "not-yet-created"), "--codex-root", "", "--pi-root", "", "--antigravity-root", ""}
	if code := cliMain(args, &stdout, &stderr); code != 0 {
		t.Fatalf("empty managed ingest = %d %s %s", code, stdout.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "not-yet-created")); !os.IsNotExist(err) {
		t.Fatalf("created agent root: %v", err)
	}
}

func TestRemoteSetupDryRunAndSecretHandling(t *testing.T) {
	state := filepath.Join(t.TempDir(), "state")
	t.Setenv("OHYA_TEST_JOIN_CODE", "ohya1.synthetic-one-time-code.tcaddress")
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
	t.Setenv("OHYA_TEST_JOIN_CODE", "retired-format:code")
	stdout.Reset()
	stderr.Reset()
	code = cliMain([]string{"setup", "--no-agents", "--no-service", "--no-modify-path", "--state", state, "--join-code-env", "OHYA_TEST_JOIN_CODE", "--json"}, &stdout, &stderr)
	if code != 2 || !bytes.Contains(stderr.Bytes(), []byte("ohya1")) {
		t.Fatalf("retired code format exit %d: %s %s", code, stdout.String(), stderr.String())
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
	if err := setup.SaveConnection(state, setup.Connection{Mode: "remote"}); err != nil {
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

func TestSetupPreservesStablePackageManagerLink(t *testing.T) {
	dir := t.TempDir()
	actual := filepath.Join(dir, "Cellar", "ohya", "0.1.0", "ohya")
	if err := os.MkdirAll(filepath.Dir(actual), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(actual, []byte("fixture"), 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "ohya")
	if err := os.Symlink(actual, link); err != nil {
		t.Fatal(err)
	}
	if got := stableBinaryPath(actual, link); got != link {
		t.Fatalf("registered %s instead of stable link", got)
	}
	other := filepath.Join(dir, "other")
	if err := os.WriteFile(other, []byte("different"), 0700); err != nil {
		t.Fatal(err)
	}
	if got := stableBinaryPath(actual, other); got != actual {
		t.Fatalf("adopted another executable: %s", got)
	}
}

func TestSetupServicePreservesExcludeFlags(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	state := filepath.Join(t.TempDir(), "state")
	previous := serviceRunnersHook
	serviceRunnersHook = func() (func(context.Context, []string) error, func(context.Context, []string) error) {
		notLoaded := func(context.Context, []string) error { return errors.New("not loaded in tests") }
		noOp := func(context.Context, []string) error { return nil }
		return noOp, notLoaded
	}
	t.Cleanup(func() { serviceRunnersHook = previous })

	guardian := filepath.Join(home, "guardian", "*")
	secret := "/opt/sessions/secret-*.jsonl"
	run := func(extra ...string) {
		t.Helper()
		args := append([]string{"setup", "--state", state, "--binary", "/nonexistent-ohya", "--no-agents", "--no-modify-path"}, extra...)
		var stdout, stderr bytes.Buffer
		cliMain(args, &stdout, &stderr)
	}
	run("--exclude", guardian, "--exclude", secret)

	service, err := setup.ServicePlan(runtime.GOOS, home, state, "/nonexistent-ohya", nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(service.Path)
	if err != nil {
		t.Fatalf("service file: %v", err)
	}
	for _, want := range []string{"--exclude", guardian, secret} {
		if !strings.Contains(string(first), want) {
			t.Fatalf("planned unit lacks %s:\n%s", want, first)
		}
	}

	run()
	regenerated, err := os.ReadFile(service.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, regenerated) {
		t.Fatalf("a regenerated service lost the exclude flags:\n%s\n%s", first, regenerated)
	}
	saved, err := setup.LoadPreferences(state)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Flags["exclude"] != guardian+"\n"+secret {
		t.Fatalf("saved excludes = %q", saved.Flags["exclude"])
	}
}

func TestPrintSetupResultMarksHealthyStatusesAsSuccess(t *testing.T) {
	t.Setenv("FORCE_COLOR", "1")
	t.Setenv("NO_COLOR", "")
	cases := map[string]string{
		"running":    "✓",
		"ready":      "✓",
		"saved":      "✓",
		"configured": "✓",
		"not needed": "·",
		"unchanged":  "·",
		"failed":     "✗",
		"planned":    "○",
		"warning":    "▲",
		"changed":    "▲",
	}
	for status, mark := range cases {
		var out bytes.Buffer
		printSetupResult(&out, setup.Result{Component: "service", Status: status})
		if !strings.Contains(out.String(), mark) {
			t.Errorf("status %q printed %q; want mark %q", status, out.String(), mark)
		}
	}
}
