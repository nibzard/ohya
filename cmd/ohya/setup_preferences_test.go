package main

import (
	"flag"
	"path/filepath"
	"testing"

	"github.com/nibzard/ohya/internal/setup"
)

func TestSetupPreservesChoicesAndExplicitOverrides(t *testing.T) {
	state := t.TempDir()
	saved := setup.Preferences{Flags: map[string]string{"agents": "codex", "no-service": "true", "no-agents": "false", "no-modify-path": "true", "claude-root": "", "codex-root": filepath.Join(state, "session history"), "exclude": "/one/*\n/two/*"}, Homes: map[string]string{"CODEX_HOME": filepath.Join(state, "custom codex")}}
	if err := setup.SavePreferences(state, saved); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "PI_CODING_AGENT_DIR", "AGY_CONFIG_DIR"} {
		t.Setenv(name, "")
	}
	for _, override := range []bool{false, true} {
		fs := flag.NewFlagSet("setup", flag.ContinueOnError)
		fs.String("agents", "auto", "")
		fs.Bool("no-service", false, "")
		fs.Bool("no-agents", false, "")
		fs.Bool("no-modify-path", false, "")
		newAgentRootFlags(fs, state)
		newExcludeFlag(fs)
		args := []string{}
		if override {
			args = []string{"--no-service=false", "--agents=auto", "--codex-root=", "--exclude", "/three/*"}
		}
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		got, err := setupPreferences(fs, state)
		if err != nil {
			t.Fatal(err)
		}
		expectedAgent, expectedService, expectedRoot := "codex", "true", saved.Flags["codex-root"]
		expectedExclude := saved.Flags["exclude"]
		if override {
			expectedAgent, expectedService, expectedRoot = "auto", "false", ""
			expectedExclude = "/three/*"
		}
		if got.Flags["agents"] != expectedAgent || got.Flags["no-service"] != expectedService || got.Flags["codex-root"] != expectedRoot || got.Flags["claude-root"] != "" || got.Flags["exclude"] != expectedExclude || got.Homes["CODEX_HOME"] != saved.Homes["CODEX_HOME"] {
			t.Fatalf("choices %+v", got)
		}
	}
}

func TestSetupPreferencesRejectBadSavedExcludes(t *testing.T) {
	for _, bad := range []string{"[", "/one/*\n", "lead\r"} {
		saved := setup.Preferences{Flags: map[string]string{"exclude": bad}, Homes: map[string]string{}}
		if err := saved.Validate(); err == nil {
			t.Fatalf("saved exclude %q accepted", bad)
		}
	}
}
