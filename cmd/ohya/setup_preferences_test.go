package main

import (
	"flag"
	"path/filepath"
	"testing"

	"github.com/nibzard/ohya/internal/setup"
)

func TestSetupPreservesChoicesAndExplicitOverrides(t *testing.T) {
	state := t.TempDir()
	saved := setup.Preferences{Flags: map[string]string{"agents": "codex", "no-service": "true", "no-agents": "false", "no-modify-path": "true", "claude-root": "", "codex-root": filepath.Join(state, "session history")}, Homes: map[string]string{"CODEX_HOME": filepath.Join(state, "custom codex")}}
	if err := setup.SavePreferences(state, saved); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "PI_CODING_AGENT_DIR"} {
		t.Setenv(name, "")
	}
	for _, override := range []bool{false, true} {
		fs := flag.NewFlagSet("setup", flag.ContinueOnError)
		fs.String("agents", "auto", "")
		fs.Bool("no-service", false, "")
		fs.Bool("no-agents", false, "")
		fs.Bool("no-modify-path", false, "")
		newAgentRootFlags(fs, state)
		args := []string{}
		if override {
			args = []string{"--no-service=false", "--agents=auto", "--codex-root="}
		}
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		got, err := setupPreferences(fs, state)
		if err != nil {
			t.Fatal(err)
		}
		expectedAgent, expectedService, expectedRoot := "codex", "true", saved.Flags["codex-root"]
		if override {
			expectedAgent, expectedService, expectedRoot = "auto", "false", ""
		}
		if got.Flags["agents"] != expectedAgent || got.Flags["no-service"] != expectedService || got.Flags["codex-root"] != expectedRoot || got.Flags["claude-root"] != "" || got.Homes["CODEX_HOME"] != saved.Homes["CODEX_HOME"] {
			t.Fatalf("choices %+v", got)
		}
	}
}
