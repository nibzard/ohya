package setup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSkillInstallIdempotencePreserveAndRemove(t *testing.T) {
	for _, agent := range []string{"claude", "codex", "pi", "agy"} {
		t.Run(agent, func(t *testing.T) {
			o := engineOptions(t)
			o.Clients.Agents = agent
			o.Clients.LookPath = func(string) (string, error) { return "/bin/" + agent, nil }
			o.ProbePi = func(context.Context, string) error { return nil }

			clients, err := DiscoverClients(o.Clients)
			if err != nil || len(clients) != 1 {
				t.Fatalf("discover %s = %+v %v", agent, clients, err)
			}
			skillPath, err := SkillPath(o.Clients, clients[0])
			if err != nil {
				t.Fatal(err)
			}

			report, err := Run(context.Background(), o)
			if err != nil || !report.Complete() {
				t.Fatalf("first setup = %+v %v", report, err)
			}
			raw, err := os.ReadFile(skillPath)
			if err != nil {
				t.Fatalf("missing skill at %s: %v", skillPath, err)
			}
			text := string(raw)
			for _, want := range []string{
				"name: ohya",
				"events_settled",
				"snippet(events_fts",
				"ohya sql --json",
				"ohya purge --marked tags.garbage",
				"session_token_usage",
			} {
				if !strings.Contains(text, want) {
					t.Fatalf("skill misses %q", want)
				}
			}

			report, err = Run(context.Background(), o)
			if err != nil || !report.Complete() {
				t.Fatalf("second setup = %+v %v", report, err)
			}
			last := report.Results[len(report.Results)-1]
			if last.Changed {
				t.Fatalf("second setup reported changed: %+v", last)
			}

			inspected, err := Inspect(context.Background(), o.Clients.State, func(context.Context, []string) error { return nil })
			if err != nil {
				t.Fatal(err)
			}
			foundReady := false
			for _, r := range inspected {
				if r.Component == "skill:"+agent && r.Status == "ready" {
					foundReady = true
				}
			}
			if !foundReady {
				t.Fatalf("inspect did not report skill:%s ready: %+v", agent, inspected)
			}

			if err := os.WriteFile(skillPath, []byte("user customized skill"), 0o600); err != nil {
				t.Fatal(err)
			}
			report, err = Run(context.Background(), o)
			if err != nil || report.Complete() {
				t.Fatalf("setup overwrote edited skill: %+v %v", report, err)
			}
			if got, _ := os.ReadFile(skillPath); string(got) != "user customized skill" {
				t.Fatalf("edited skill changed to %q", got)
			}

			if err := os.WriteFile(skillPath, SkillContent(), 0o600); err != nil {
				t.Fatal(err)
			}
			removed, err := Remove(context.Background(), o.Clients.State, nil)
			if err != nil || !removed.Complete() {
				t.Fatalf("remove = %+v %v", removed, err)
			}
			if _, err := os.Stat(skillPath); !os.IsNotExist(err) {
				t.Fatalf("skill remained after uninstall: %v", err)
			}
			if _, err := os.Stat(filepath.Dir(skillPath)); !os.IsNotExist(err) {
				t.Fatalf("empty skill directory remained after uninstall: %v", err)
			}
		})
	}
}

func TestSkillPathsRespectHomeOverrides(t *testing.T) {
	home := t.TempDir()
	overrides := map[string]string{
		"CLAUDE_CONFIG_DIR":   filepath.Join(home, "custom-claude"),
		"CODEX_HOME":          filepath.Join(home, "custom-codex"),
		"PI_CODING_AGENT_DIR": filepath.Join(home, "custom-pi"),
		"AGY_CONFIG_DIR":      filepath.Join(home, "custom-agy"),
	}
	o := ClientOptions{
		Home:     home,
		State:    filepath.Join(home, "state"),
		Binary:   "/bin/ohya",
		Args:     []string{"mcp"},
		Agents:   "claude,codex,pi,agy",
		Getenv:   func(k string) string { return overrides[k] },
		LookPath: func(name string) (string, error) { return "/bin/" + name, nil },
	}
	clients, err := DiscoverClients(o)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"claude": filepath.Join(overrides["CLAUDE_CONFIG_DIR"], "skills", "ohya", "SKILL.md"),
		"codex":  filepath.Join(overrides["CODEX_HOME"], "skills", "ohya", "SKILL.md"),
		"pi":     filepath.Join(overrides["PI_CODING_AGENT_DIR"], "skills", "ohya", "SKILL.md"),
		"agy":    filepath.Join(overrides["AGY_CONFIG_DIR"], "skills", "ohya", "SKILL.md"),
	}
	for _, c := range clients {
		got, err := SkillPath(o, c)
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if got != want[c.Name] {
			t.Fatalf("%s skill path = %q, want %q", c.Name, got, want[c.Name])
		}
	}
}
