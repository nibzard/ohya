package setup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestClaudePolicyMatchingAndPrecedence(t *testing.T) {
	dir := t.TempDir()
	managed := filepath.Join(dir, "managed")
	if err := os.MkdirAll(managed, 0700); err != nil {
		t.Fatal(err)
	}
	policy := filepath.Join(dir, "settings.json")
	command := Command{Command: "/bin/ohya", Args: []string{"mcp"}}
	cases := []struct {
		text    string
		allowed bool
	}{
		{`{}`, true}, {`{"allowedMcpServers":[]}`, false},
		{`{"allowedMcpServers":[{"serverName":"ohya"}]}`, true},
		{`{"allowedMcpServers":[{"serverName":"ohya"},{"serverCommand":["other"]}]}`, false},
		{`{"allowedMcpServers":[{"serverCommand":["/bin/ohya","mcp"]}]}`, true},
		{`{"deniedMcpServers":[{"serverName":"ohya"}]}`, false},
		{`{"deniedMcpServers":[{"serverCommand":["/bin/ohya","mcp"]}]}`, false},
	}
	for _, c := range cases {
		if err := os.WriteFile(policy, []byte(c.text), 0600); err != nil {
			t.Fatal(err)
		}
		if allowed := CheckClaudePolicy([]string{policy}, managed, command) == nil; allowed != c.allowed {
			t.Fatalf("policy %s allowed=%v", c.text, allowed)
		}
	}
	if err := os.WriteFile(filepath.Join(managed, "managed-mcp.json"), []byte(`{"mcpServers":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := CheckClaudePolicy(nil, managed, command); err == nil {
		t.Fatal("ignored exclusive managed configuration")
	}
}

func TestProjectOverridesAreReadOnly(t *testing.T) {
	o := engineOptions(t)
	root := t.TempDir()
	cwd := filepath.Join(root, "nested")
	if err := os.MkdirAll(cwd, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".mcp.json")
	before := []byte(`{"mcpServers":{"ohya":{"command":"project-server"}}}`)
	if err := os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	c := Client{Name: "claude", Config: filepath.Join(o.Clients.Home, ".claude.json")}
	if err := PreflightClient(o.Clients, c, cwd); err == nil {
		t.Fatal("ignored project entry")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatal("modified project config")
	}
}

func TestPolicyBlockPreventsSetupPreparation(t *testing.T) {
	o := engineOptions(t)
	o.WorkingDirectory = t.TempDir()
	if err := os.WriteFile(filepath.Join(o.WorkingDirectory, ".mcp.json"), []byte(`{"mcpServers":{"ohya":{}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	o.Prepare = func(context.Context) error { t.Fatal("prepared setup despite blocked client"); return nil }
	report, err := Run(context.Background(), o)
	if err != nil || report.Complete() {
		t.Fatalf("policy block %+v %v", report, err)
	}
	if _, err := os.Stat(o.Clients.State); !os.IsNotExist(err) {
		t.Fatal("created state despite blocked client")
	}
}
