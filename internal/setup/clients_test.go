package setup

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestClientConfigurationPathsAndOwnership(t *testing.T) {
	dir := t.TempDir()
	o := ClientOptions{Home: dir, State: filepath.Join(dir, "state"), Binary: filepath.Join(dir, "bin", "ohya"), Args: []string{"mcp", "--db", filepath.Join(dir, "hub.db")}, Getenv: func(key string) string { return filepath.Join(dir, key) }, LookPath: func(name string) (string, error) { return "/bin/" + name, nil }}
	clients, err := DiscoverClients(o)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(dir, "CLAUDE_CONFIG_DIR", ".claude.json"), filepath.Join(dir, "CODEX_HOME", "config.toml"), filepath.Join(dir, "PI_CODING_AGENT_DIR", "mcp.json"), filepath.Join(dir, "AGY_CONFIG_DIR", "mcp_config.json")}
	for i, c := range clients {
		if c.Config != want[i] {
			t.Fatalf("config = %s, want %s", c.Config, want[i])
		}
		if changed, err := ApplyClient(o, c); err != nil || !changed {
			t.Fatalf("%s apply = %v, %v", c.Name, changed, err)
		}
		if changed, err := ApplyClient(o, c); err != nil || changed {
			t.Fatalf("%s repeat = %v, %v", c.Name, changed, err)
		}
		if _, err := RemoveClient(o.State, c.Name); err != nil {
			t.Fatal(err)
		}
	}
}

func TestClientDiscoveryMissingAndInvalid(t *testing.T) {
	o := ClientOptions{Home: t.TempDir(), State: "/state", Binary: "/bin/ohya", Getenv: func(string) string { return "" }, LookPath: func(string) (string, error) { return "", os.ErrNotExist }}
	clients, err := DiscoverClients(o)
	if err != nil || len(clients) != 4 {
		t.Fatalf("auto = %v, %v", clients, err)
	}
	for _, c := range clients {
		if c.Status != "absent" {
			t.Fatal(c)
		}
	}
	o.Agents = "codex"
	clients, err = DiscoverClients(o)
	if err != nil || clients[0].Status != "failed" {
		t.Fatalf("explicit = %v, %v", clients, err)
	}
	for _, agents := range []string{"unknown", "codex,codex"} {
		o.Agents = agents
		if _, err := DiscoverClients(o); err == nil {
			t.Fatalf("accepted %q", agents)
		}
	}
	o.Agents = "claude"
	o.LookPath = func(string) (string, error) { return "/claude", nil }
	o.Getenv = func(string) string { return "relative" }
	if _, err := DiscoverClients(o); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("relative override = %v", err)
	}
}
