package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nibzard/ohya/internal/setup"
)

type childServiceManager struct {
	t          *testing.T
	db         string
	state      string
	regionFile string
	claudeRoot string

	mu        sync.Mutex
	ingest    *exec.Cmd
	serve     *exec.Cmd
	ingestLog *bytes.Buffer
	serveLog  *bytes.Buffer
}

func newChildServiceManager(t *testing.T, db, state, regionFile, claudeRoot string) *childServiceManager {
	m := &childServiceManager{t: t, db: db, state: state, regionFile: regionFile, claudeRoot: claudeRoot}
	t.Cleanup(m.stopAll)
	return m
}

func (m *childServiceManager) hook() (func(context.Context, []string) error, func(context.Context, []string) error) {
	return m.run, m.run
}

func (m *childServiceManager) run(_ context.Context, args []string) error {
	joined := strings.Join(args, " ")
	m.mu.Lock()
	defer m.mu.Unlock()
	serveCmd := strings.Contains(joined, "ohya-serve.service") || strings.Contains(joined, "com.nibzard.ohya.serve")
	var cmd **exec.Cmd
	if serveCmd {
		cmd = &m.serve
	} else if strings.Contains(joined, "ohya.service") || strings.Contains(joined, "com.nibzard.ohya") {
		cmd = &m.ingest
	} else {
		return nil
	}
	switch {
	case strings.Contains(joined, "disable") || strings.Contains(joined, "bootout"):
		return m.stopLocked(cmd)
	case strings.Contains(joined, "restart") || strings.Contains(joined, "bootstrap") || strings.Contains(joined, "kickstart"):
		if err := m.stopLocked(cmd); err != nil {
			return err
		}
		if serveCmd {
			return m.startServeLocked()
		}
		return m.startIngestLocked()
	case strings.Contains(joined, "show") || strings.Contains(joined, "print"):
		if *cmd == nil || (*cmd).Process == nil || (*cmd).ProcessState != nil {
			if strings.Contains(joined, "LoadState") {
				return fmt.Errorf("%s is not loaded", serviceUnitName(serveCmd))
			}
			return fmt.Errorf("%s is not running", serviceUnitName(serveCmd))
		}
		return nil
	}
	return nil
}

func serviceUnitName(serve bool) string {
	if serve {
		return "hub service"
	}
	return "memory service"
}

func (m *childServiceManager) startServeLocked() error {
	log := &bytes.Buffer{}
	cmd := exec.Command(os.Args[0], "child-serve", "--db", m.db, "--state", m.state)
	cmd.Env = append(os.Environ(), "OHYA_TEST_DERP_REGION_FILE="+m.regionFile)
	cmd.Stderr = log
	cmd.Stdout = log
	if err := cmd.Start(); err != nil {
		return err
	}
	m.serve, m.serveLog = cmd, log
	return nil
}

func (m *childServiceManager) startIngestLocked() error {
	log := &bytes.Buffer{}
	cmd := exec.Command(os.Args[0], "child-run", "ingest", "--allow-empty",
		"--db", m.db, "--state", m.state,
		"--claude-root", m.claudeRoot, "--codex-root", "", "--pi-root", "", "--antigravity-root", "")
	cmd.Env = os.Environ()
	cmd.Stderr = log
	cmd.Stdout = log
	if err := cmd.Start(); err != nil {
		return err
	}
	m.ingest, m.ingestLog = cmd, log
	return nil
}

func (m *childServiceManager) stopLocked(cmd **exec.Cmd) error {
	c := *cmd
	*cmd = nil
	if c == nil || c.Process == nil || c.ProcessState != nil {
		return nil
	}
	_ = c.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		_ = c.Process.Kill()
		<-done
	}
	return nil
}

func (m *childServiceManager) stopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	_ = m.stopLocked(&m.ingest)
	_ = m.stopLocked(&m.serve)
}

type setupReportJSON struct {
	Data struct {
		Results []setup.Result `json:"results"`
		Hub     *setup.HubJoin `json:"hub"`
	} `json:"data"`
}

func resultStatus(t *testing.T, report setupReportJSON, component string) setup.Result {
	t.Helper()
	for _, r := range report.Data.Results {
		if r.Component == component {
			return r
		}
	}
	t.Fatalf("no %s component in %+v", component, report.Data.Results)
	return setup.Result{}
}

func TestHubSetupTwoStepFlow(t *testing.T) {
	skipUnlessTransportE2E(t)
	dir := transportTestDir(t)
	home := filepath.Join(dir, "home")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	hubState := filepath.Join(dir, "ohya")
	hubDB := filepath.Join(hubState, "hub.db")
	claudeRoot := filepath.Join(dir, "claude-projects")
	if err := os.MkdirAll(claudeRoot, 0755); err != nil {
		t.Fatal(err)
	}
	regionFile := writeDERPRegionFile(t, startLocalDERP(t))
	wrapper := filepath.Join(dir, "ohya-wrapper")
	quoted := "'" + strings.ReplaceAll(os.Args[0], "'", "'\\''") + "'"
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexec "+quoted+" child-run \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}

	manager := newChildServiceManager(t, hubDB, hubState, regionFile, claudeRoot)
	previous := serviceRunnersHook
	serviceRunnersHook = manager.hook
	t.Cleanup(func() { serviceRunnersHook = previous })

	hubArgs := []string{"setup", "--hub", "--state", hubState, "--binary", wrapper,
		"--no-agents", "--no-modify-path", "--claude-root", claudeRoot,
		"--codex-root", "", "--pi-root", "", "--antigravity-root", "", "--json"}
	var stdout, stderr bytes.Buffer
	if code := cliMain(hubArgs, &stdout, &stderr); code != 0 {
		t.Fatalf("hub setup = %d %s %s", code, stdout.String(), stderr.String())
	}
	var first setupReportJSON
	if err := json.Unmarshal(stdout.Bytes(), &first); err != nil {
		t.Fatalf("hub setup output %q: %v", stdout.String(), err)
	}
	if first.Data.Hub == nil || first.Data.Hub.Code == "" {
		t.Fatalf("hub setup issued no join code: %s", stdout.String())
	}
	if _, _, err := parseJoinCode(first.Data.Hub.Code); err != nil {
		t.Fatalf("issued code: %v", err)
	}
	if !strings.Contains(first.Data.Hub.DeviceCommand, "--join") || !strings.Contains(first.Data.Hub.DeviceCommand, "install.sh") {
		t.Fatalf("device command = %q", first.Data.Hub.DeviceCommand)
	}
	if first.Data.Hub.NodeKey == "" || first.Data.Hub.RelayRegion == "" || first.Data.Hub.ExpiresAt == "" {
		t.Fatalf("hub report = %+v", first.Data.Hub)
	}
	for _, component := range []string{"service", "hub-service", "hub", "memory"} {
		if r := resultStatus(t, first, component); r.Status != "running" && r.Status != "ready" {
			t.Fatalf("%s = %+v; full report %+v", component, r, first.Data.Results)
		}
	}
	hubPlan, err := setup.HubServicePlan(runtime.GOOS, home, hubState, wrapper, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(hubPlan.Path); err != nil {
		t.Fatalf("hub service file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(hubState, "setup", "hub-service.json")); err != nil {
		t.Fatalf("hub service receipt: %v", err)
	}
	t.Log("step one: hub setup configured both services, probed the relay, and issued a code")

	stdout.Reset()
	stderr.Reset()
	if code := cliMain([]string{"doctor", "--state", hubState, "--db", hubDB, "--json"}, &stdout, &stderr); code != 0 {
		t.Fatalf("hub doctor = %d %s %s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "hub_identity") || !strings.Contains(stdout.String(), `"setup:hub-service","severity":"ok"`) {
		t.Fatalf("doctor missed the hub checks: %s", stdout.String())
	}

	devState := filepath.Join(dir, "dev-ohya")
	t.Setenv("OHYA_HUB_E2E_CODE", first.Data.Hub.Code)
	devArgs := []string{"setup", "--state", devState, "--binary", wrapper, "--join-code-env", "OHYA_HUB_E2E_CODE",
		"--no-agents", "--no-service", "--no-modify-path", "--json"}
	stdout.Reset()
	stderr.Reset()
	if code := cliMain(devArgs, &stdout, &stderr); code != 0 {
		t.Fatalf("device setup = %d %s %s", code, stdout.String(), stderr.String())
	}
	if os.Getenv("OHYA_HUB_E2E_CODE") != "" {
		t.Fatal("join code remains in the environment")
	}
	if _, err := os.Stat(filepath.Join(devState, "join", "credential.json")); err != nil {
		t.Fatalf("device paired no credential: %v", err)
	}
	t.Log("step two: the device command's code paired the device")

	client := mcp.NewClient(&mcp.Implementation{Name: "hub-setup-e2e", Version: "v0"}, nil)
	mcpCtx, mcpCancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer mcpCancel()
	remoteFor := func() *exec.Cmd {
		cmd := exec.Command(os.Args[0], "child-mcp", "--remote", "--state", filepath.Join(devState, "join"))
		cmd.Stderr = &stderr
		return cmd
	}
	session, err := client.Connect(mcpCtx, &mcp.CommandTransport{Command: remoteFor()}, nil)
	if err != nil {
		t.Fatalf("remote mcp connect: %v; %s", err, stderr.String())
	}
	defer session.Close()
	var count struct {
		Rows [][]any `json:"rows"`
	}
	e2eCall(t, mcpCtx, session, "sql", map[string]any{"query": "SELECT count(*) AS n FROM events"}, &count)
	if len(count.Rows) != 1 {
		t.Fatalf("remote count rows = %+v", count.Rows)
	}
	t.Log("remote MCP queried the hub through the joined device")

	stdout.Reset()
	stderr.Reset()
	if code := cliMain(hubArgs, &stdout, &stderr); code != 0 {
		t.Fatalf("hub setup rerun = %d %s %s", code, stdout.String(), stderr.String())
	}
	var rerun setupReportJSON
	if err := json.Unmarshal(stdout.Bytes(), &rerun); err != nil {
		t.Fatal(err)
	}
	for _, component := range []string{"service", "hub-service"} {
		if r := resultStatus(t, rerun, component); r.Changed {
			t.Fatalf("rerun rewrote %s: %+v", component, r)
		}
	}
	if r := resultStatus(t, rerun, "hub"); r.Status != "ready" {
		t.Fatalf("rerun hub check = %+v", r)
	}
	if rerun.Data.Hub == nil || rerun.Data.Hub.Code == first.Data.Hub.Code {
		t.Fatalf("rerun issued no fresh code: %+v", rerun.Data.Hub)
	}
	t.Log("hub setup rerun changed nothing and issued a fresh code")

	t.Setenv("OHYA_HUB_E2E_CODE", "")
	devRerun := []string{"setup", "--state", devState, "--binary", wrapper,
		"--no-agents", "--no-service", "--no-modify-path", "--json"}
	stdout.Reset()
	stderr.Reset()
	if code := cliMain(devRerun, &stdout, &stderr); code != 0 {
		t.Fatalf("device setup rerun = %d %s %s", code, stdout.String(), stderr.String())
	}
	t.Log("device setup rerun reused the saved credential")

	stdout.Reset()
	stderr.Reset()
	e2eCall(t, mcpCtx, session, "sql", map[string]any{"query": "SELECT count(*) AS n FROM events"}, &count)
	if len(count.Rows) != 1 {
		t.Fatalf("remote count after reruns = %+v", count.Rows)
	}
}
