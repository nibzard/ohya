package setup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func engineOptions(t *testing.T) Options {
	t.Helper()
	dir := t.TempDir()
	return Options{Clients: ClientOptions{Home: dir, State: filepath.Join(dir, "state"), Binary: "/bin/ohya", Args: []string{"mcp"}, Agents: "claude", Getenv: func(string) string { return "" }, LookPath: func(string) (string, error) { return "/bin/claude", nil }}, ServiceArgs: []string{"ingest"}, Platform: "linux", NoService: true, Verify: func(context.Context, Command) error { return nil }}
}

func TestSetupDryRunDoesNotCreateState(t *testing.T) {
	o := engineOptions(t)
	o.DryRun = true
	report, err := Run(context.Background(), o)
	if err != nil || !report.Complete() || !report.DryRun {
		t.Fatalf("dry run = %+v %v", report, err)
	}
	if _, err := os.Stat(o.Clients.State); !os.IsNotExist(err) {
		t.Fatalf("created state: %v", err)
	}
	if _, err := os.Stat(filepath.Join(o.Clients.Home, ".claude.json")); !os.IsNotExist(err) {
		t.Fatalf("changed config: %v", err)
	}
}

func TestSetupRepairsAndRemovesIntegration(t *testing.T) {
	o := engineOptions(t)
	for i := 0; i < 2; i++ {
		report, err := Run(context.Background(), o)
		if err != nil || !report.Complete() {
			t.Fatalf("setup = %+v %v", report, err)
		}
		last := report.Results[len(report.Results)-1]
		if last.Status != "configured" || last.Changed != (i == 0) {
			t.Fatal(last)
		}
	}
	report, err := Remove(context.Background(), o.Clients.State, nil)
	if err != nil || !report.Complete() {
		t.Fatalf("remove = %+v %v", report, err)
	}
	root, _, _, err := readEntry(JSONEntry{Path: filepath.Join(o.Clients.Home, ".claude.json"), Parent: "mcpServers"})
	if err != nil || root["mcpServers"] == nil {
		t.Fatalf("remaining config = %+v %v", root, err)
	}
}

func TestSetupReportsVerificationAndServiceFailures(t *testing.T) {
	o := engineOptions(t)
	o.NoService = false
	o.RunService = func(context.Context, []string) error { return errors.New("user manager unavailable") }
	o.Verify = func(context.Context, Command) error { return errors.New("MCP unavailable") }
	report, err := Run(context.Background(), o)
	if err != nil || report.Complete() {
		t.Fatalf("partial = %+v %v", report, err)
	}
	if report.Results[0].Status != "failed" || report.Results[1].Status != "failed" {
		t.Fatal(report)
	}
}

func TestSetupPreservesUnownedLoadedService(t *testing.T) {
	o := engineOptions(t)
	o.NoService = false
	calls := 0
	o.RunService = func(context.Context, []string) error { calls++; return nil }
	report, err := Run(context.Background(), o)
	if err != nil || report.Complete() {
		t.Fatalf("unowned = %+v %v", report, err)
	}
	if calls != 1 {
		t.Fatalf("modified unowned service: %d calls", calls)
	}
}
