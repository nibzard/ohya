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

const launchAgentPrintRunning = `gui/501/com.nibzard.ohya = {
	active count = 1
	path = /Users/example/Library/LaunchAgents/com.nibzard.ohya.plist
	type = LaunchAgent
	state = running

	program = /Users/example/.local/bin/ohya
	arguments = {
		/Users/example/.local/bin/ohya
		ingest
	}

	default environment = {
		PATH => /usr/bin:/bin:/usr/sbin:/sbin
	}

	domain = gui/501 [100016]
	runs = 2
	pid = 4242
	last terminating signal = Terminated: 15

	resource coalition = {
		ID = 17528
		type = resource
		state = active
		active count = 1
		name = com.nibzard.ohya
	}

	jetsam coalition = {
		ID = 17529
		type = jetsam
		state = active
		active count = 1
		name = com.nibzard.ohya
	}
}
`

const launchAgentPrintWaiting = `gui/501/com.nibzard.ohya = {
	state = waiting
	pid = -

	resource coalition = {
		ID = 17528
		type = resource
		state = active
	}
}
`

func TestServiceReadinessRequiresRunningProcess(t *testing.T) {
	cases := []struct {
		platform, output string
		ready            bool
	}{
		{"linux", "ActiveState=active\nSubState=running\nMainPID=123\n", true},
		{"linux", "ActiveState=active\nSubState=exited\nMainPID=0\n", false},
		{"linux", "ActiveState=activating\nSubState=auto-restart\nMainPID=123\n", false},
		{"darwin", "gui/501/com.nibzard.ohya = {\n\tstate = running\n\tpid = 123\n}\n", true},
		{"darwin", "gui/501/com.nibzard.ohya = {\n\tstate = waiting\n\tpid = 123\n}\n", false},
		{"darwin", "gui/501/com.nibzard.ohya = {\n\tstate = running\n}\n", false},
		{"darwin", launchAgentPrintRunning, true},
		{"darwin", launchAgentPrintWaiting, false},
	}
	for _, c := range cases {
		if ready := serviceRunning(c.platform, c.output, "memory service") == nil; ready != c.ready {
			t.Fatalf("%s %q readiness = %v", c.platform, c.output, ready)
		}
	}
}
