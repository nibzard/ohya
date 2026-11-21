package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nibzard/ohya/internal/setup"
)

func installServiceHookStub(t *testing.T, initialLoaded, initialRunning bool) (*bool, *bool) {
	t.Helper()
	loaded := initialLoaded
	running := initialRunning
	prev := serviceControlHooks
	serviceControlHooks.loaded = func(_ context.Context, _ []string) error {
		if loaded {
			return nil
		}
		return errors.New("not loaded")
	}
	serviceControlHooks.run = func(_ context.Context, args []string) error {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "disable --now"), strings.Contains(joined, "bootout"):
			loaded = false
			running = false
		case strings.Contains(joined, "restart"), strings.Contains(joined, "kickstart"):
			loaded = true
			running = true
		case strings.Contains(joined, "bootstrap"), strings.Contains(joined, "enable"):
			loaded = true
		case strings.Contains(joined, "ActiveState,SubState,MainPID"), strings.Contains(joined, "launchctl print"):
			if !running {
				return errors.New("not running")
			}
		}
		return nil
	}
	serviceControlHooks.wait = func(_ context.Context, _ setup.Service, _ func(context.Context, []string) error) error {
		if !running {
			return errors.New("service did not become ready")
		}
		return nil
	}
	t.Cleanup(func() { serviceControlHooks = prev })
	return &loaded, &running
}

func seedCLIOwnedService(t *testing.T, hub bool) string {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	state := filepath.Join(root, "state")
	planFn := setup.ServicePlan
	receiptName := "service.json"
	if hub {
		planFn = setup.HubServicePlan
		receiptName = "hub-service.json"
	}
	service, err := planFn("linux", home, state, "/usr/local/bin/ohya", []string{"ingest"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := setup.ApplyFile(service.Path, filepath.Join(state, "setup", receiptName), service.Content); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestServiceCLIJSONAndTextFlow(t *testing.T) {
	state := seedCLIOwnedService(t, false)
	installServiceHookStub(t, true, true)

	code, out, stderr := runCLI("service", "status", "--state", state, "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("status exit=%d stderr=%q", code, stderr)
	}
	env := decodeEnvelope(t, out)
	if env.Command != "ohya service status" || env.Error != nil {
		t.Fatalf("unexpected envelope: %+v", env)
	}
	var st setup.ServiceStatus
	if err := json.Unmarshal(env.Data, &st); err != nil {
		t.Fatal(err)
	}
	if st.Service != "service" || !st.Installed || !st.Loaded || !st.Running || st.Changed {
		t.Fatalf("status data = %+v", st)
	}

	code, out, stderr = runCLI("service", "stop", "--state", state, "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("stop exit=%d stderr=%q", code, stderr)
	}
	env = decodeEnvelope(t, out)
	if err := json.Unmarshal(env.Data, &st); err != nil {
		t.Fatal(err)
	}
	if !st.Changed || st.Loaded || st.Running {
		t.Fatalf("stop data = %+v", st)
	}

	code, out, stderr = runCLI("service", "start", "--state", state)
	if code != 0 || stderr != "" {
		t.Fatalf("start exit=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(out, "memory service") || !strings.Contains(out, "running") || !strings.Contains(out, "started") {
		t.Fatalf("start text = %q", out)
	}

	code, out, stderr = runCLI("service", "restart", "--state", state, "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("restart exit=%d stderr=%q", code, stderr)
	}
	env = decodeEnvelope(t, out)
	if err := json.Unmarshal(env.Data, &st); err != nil {
		t.Fatal(err)
	}
	if env.Command != "ohya service restart" || !st.Changed || !st.Running {
		t.Fatalf("restart data = %+v", st)
	}
}

func TestServiceCLIUnconfiguredErrorJSON(t *testing.T) {
	state := t.TempDir()
	code, out, _ := runCLI("service", "stop", "--state", state, "--json")
	if code != 1 {
		t.Fatalf("exit=%d; want 1", code)
	}
	env := decodeEnvelope(t, out)
	if env.Error == nil || env.Error.Code != "service_not_configured" {
		t.Fatalf("unexpected error envelope: %+v", env)
	}
	if !strings.Contains(env.Error.Hint, "ohya setup") {
		t.Fatalf("hint = %q; want ohya setup guidance", env.Error.Hint)
	}
}

func TestServiceCLIUsageErrors(t *testing.T) {
	code, _, stderr := runCLI("service", "bogus")
	if code != 2 || !strings.Contains(stderr, "unknown service subcommand") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	code, _, stderr = runCLI("service", "status", "extra")
	if code != 2 || !strings.Contains(stderr, "unexpected argument") {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
}
