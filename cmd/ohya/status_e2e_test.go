package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func waitForFile(t *testing.T, path string, deadline time.Duration) bool {
	t.Helper()
	until := time.Now().Add(deadline)
	for time.Now().Before(until) {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func runChildStatus(t *testing.T, stateDir string) (statusEnvelope, int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "child-run", "status", "--state", stateDir, "--db", filepath.Join(stateDir, "hub.db"), "--json")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	code := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			t.Fatalf("status child: %v (stderr %s)", runErr, stderr.String())
		}
		code = exitErr.ExitCode()
	}
	var envelope statusEnvelope
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("status child output must decode: %v: %s", err, stdout.String())
	}
	return envelope, code
}

func startIngestChild(t *testing.T, stateDir string, extra ...string) *exec.Cmd {
	t.Helper()
	args := append([]string{
		"child-run", "ingest",
		"--db", filepath.Join(stateDir, "hub.db"),
		"--state", stateDir,
		"--allow-empty",
		"--interval", "50ms",
		"--claude-root", filepath.Join(stateDir, "projects"),
		"--codex-root", "",
		"--pi-root", "",
		"--antigravity-root", "",
	}, extra...)
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "HOME="+stateDir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start ingest child: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	return cmd
}

func TestStatusChildLifecycleHealthyKilledDisabled(t *testing.T) {
	stateDir := t.TempDir()
	child := startIngestChild(t, stateDir)
	instanceDir := waitForInstance(t, stateDir, 5*time.Second)
	if instanceDir == "" {
		t.Fatal("the ingest child must publish an instance directory at startup")
	}

	envelope, code := runChildStatus(t, stateDir)
	if code != 0 {
		t.Fatalf("a healthy publishing child must exit zero, got %d", code)
	}
	if len(envelope.Data.Instances) != 1 {
		t.Fatalf("instances = %d, want 1: %+v", len(envelope.Data.Instances), envelope.Data)
	}
	instance := envelope.Data.Instances[0]
	if !instance.Fresh || instance.Process != "running" {
		t.Errorf("the live child must be fresh and running: %+v", instance)
	}
	if instance.Roles[0] != "ingest" {
		t.Errorf("role = %s, want ingest", instance.Roles[0])
	}
	if envelope.Data.Instances[0].UptimeMS == nil || *instance.UptimeMS < 0 {
		t.Error("uptime must be reported")
	}

	metricsCmd := exec.Command(os.Args[0], "child-run", "metrics", "--state", stateDir, "--db", filepath.Join(stateDir, "hub.db"), "--window", "1m", "--json")
	var metricsOut bytes.Buffer
	metricsCmd.Stdout = &metricsOut
	metricsCmd.Stderr = &metricsOut
	runErr := metricsCmd.Run()
	metricsCode := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			t.Fatalf("metrics child: %v (%s)", runErr, metricsOut.String())
		}
		metricsCode = exitErr.ExitCode()
	}
	var metricsEnvelope struct {
		SchemaVersion int         `json:"schema_version"`
		Command       string      `json:"command"`
		Data          metricsData `json:"data"`
		Error         *jsonError  `json:"error"`
	}
	if err := json.Unmarshal(metricsOut.Bytes(), &metricsEnvelope); err != nil {
		t.Fatalf("metrics child output must decode: %v: %s", err, metricsOut.String())
	}
	if metricsEnvelope.Command != metricsSpec.name || metricsEnvelope.Error != nil {
		t.Fatalf("metrics envelope wrong: %+v", metricsEnvelope)
	}
	if metricsEnvelope.Data.Instances[0].SampleCount < 1 {
		t.Errorf("the startup history sample must be visible: %+v", metricsEnvelope.Data.Instances[0])
	}
	if metricsCode != 3 {
		t.Errorf("a child younger than the window must exit three, got %d", metricsCode)
	}

	if err := child.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	envelope, code = runChildStatus(t, stateDir)
	if code != 3 {
		t.Fatalf("a killed child must exit three, got %d", code)
	}
	if !hasFindingCode(envelope.Data.Findings, "instance_terminated", sevWarn) {
		t.Errorf("the killed child must be reported as terminated: %+v", envelope.Data.Findings)
	}

	disabledState := t.TempDir()
	disabled := startIngestChild(t, disabledState, "--observe", "off")
	time.Sleep(400 * time.Millisecond)
	if err := disabled.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = disabled.Wait()
	if _, err := os.Stat(filepath.Join(disabledState, "observe")); err == nil {
		t.Error("--observe off must not publish an observe tree")
	}
	envelope, code = runChildStatus(t, disabledState)
	if code != 3 {
		t.Fatalf("disabled observation must exit three, got %d", code)
	}
	if !hasFindingCode(envelope.Data.Findings, "no_instances", sevWarn) {
		t.Errorf("disabled observation must be reported as absent, not healthy: %+v", envelope.Data.Findings)
	}
}

func waitForInstance(t *testing.T, stateDir string, deadline time.Duration) string {
	t.Helper()
	observeDir := filepath.Join(stateDir, "observe")
	until := time.Now().Add(deadline)
	for time.Now().Before(until) {
		entries, err := os.ReadDir(observeDir)
		if err == nil {
			for _, entry := range entries {
				if entry.IsDir() && len(entry.Name()) == 32 {
					path := filepath.Join(observeDir, entry.Name(), "snapshot.json")
					if _, err := os.Stat(path); err == nil {
						return filepath.Join(observeDir, entry.Name())
					}
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return ""
}

func TestStatusChildCleanShutdownPublishesFinalSnapshot(t *testing.T) {
	stateDir := t.TempDir()
	child := startIngestChild(t, stateDir)
	instanceDir := waitForInstance(t, stateDir, 5*time.Second)
	if instanceDir == "" {
		t.Fatal("the ingest child must publish an instance directory at startup")
	}
	if err := child.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitErr := child.Wait()
	if waitErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(waitErr, &exitErr) || exitErr.ExitCode() != 0 {
			t.Fatalf("SIGTERM must stop the ingest child cleanly with exit 0: %v", waitErr)
		}
	}
	raw, err := os.ReadFile(filepath.Join(instanceDir, "snapshot.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"truncated"`)) && !bytes.Contains(raw, []byte(`"counters"`)) {
		t.Fatalf("the final snapshot must be a complete snapshot document: %s", raw)
	}
	var snapshot struct {
		UptimeMS int64 `json:"uptime_ms"`
		Stopped  bool  `json:"stopped"`
	}
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatalf("final snapshot must decode: %v", err)
	}
	if snapshot.UptimeMS <= 0 {
		t.Errorf("final snapshot uptime = %d, want positive", snapshot.UptimeMS)
	}
	if !snapshot.Stopped {
		t.Errorf("final snapshot from clean shutdown must set stopped=true: %s", raw)
	}
	if !strings.Contains(string(raw), `"roles":["ingest"]`) {
		t.Errorf("final snapshot must carry the ingest role: %s", raw)
	}
}
