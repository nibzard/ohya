package setup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestInspectOwnedIntegrationsDoesNotWrite(t *testing.T) {
	o := engineOptions(t)
	if _, err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(o.Clients.Home, ".claude.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	report, err := Inspect(context.Background(), o.Clients.State, func(context.Context, []string) error { t.Fatal("unexpected service command"); return nil })
	if err != nil || len(report) != 1 || report[0].Status != "ready" {
		t.Fatalf("inspect %+v %v", report, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("inspection changed config")
	}
	if err := os.WriteFile(path, []byte(`{"mcpServers":{"ohya":{"command":"user-edit"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	report, err = Inspect(context.Background(), o.Clients.State, nil)
	if err != nil || len(report) != 1 || report[0].Status != "changed" {
		t.Fatalf("edited %+v %v", report, err)
	}
	missing := filepath.Join(t.TempDir(), "absent")
	report, err = Inspect(context.Background(), missing, nil)
	if err != nil || len(report) != 0 {
		t.Fatalf("missing %+v %v", report, err)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("created state")
	}
}

func TestServiceReadinessRequiresRunningProcess(t *testing.T) {
	cases := []struct {
		platform, output string
		ready            bool
	}{
		{"linux", "ActiveState=active\nSubState=running\nMainPID=123\n", true},
		{"linux", "ActiveState=active\nSubState=exited\nMainPID=0\n", false},
		{"linux", "ActiveState=activating\nSubState=auto-restart\nMainPID=123\n", false},
		{"darwin", "state = running\n pid = 123\n", true},
		{"darwin", "state = waiting\n pid = 123\n", false},
		{"darwin", "state = running\n", false},
	}
	for _, c := range cases {
		if ready := serviceRunning(c.platform, c.output) == nil; ready != c.ready {
			t.Fatalf("%s %q readiness = %v", c.platform, c.output, ready)
		}
	}
}
