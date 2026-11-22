package main

import (
	"flag"
	"os"

	"github.com/nibzard/ohya/internal/setup"
)

func setupPreferences(fs *flag.FlagSet, state string) (setup.Preferences, error) {
	saved, err := setup.LoadPreferences(state)
	if err != nil {
		return saved, err
	}
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	for name, value := range saved.Flags {
		if !explicit[name] {
			if err := fs.Set(name, value); err != nil {
				return saved, err
			}
		}
	}
	result := setup.Preferences{Flags: map[string]string{}, Homes: map[string]string{}}
	for _, name := range []string{"agents", "no-agents", "no-service", "no-modify-path", "claude-root", "codex-root", "pi-root", "antigravity-root", "exclude"} {
		result.Flags[name] = fs.Lookup(name).Value.String()
	}
	for _, name := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME", "PI_CODING_AGENT_DIR", "AGY_CONFIG_DIR"} {
		value := os.Getenv(name)
		if value == "" {
			value = saved.Homes[name]
		}
		result.Homes[name] = value
	}
	return result, nil
}
