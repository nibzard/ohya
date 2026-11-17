package main

import (
	"flag"
	"path/filepath"

	"github.com/nibzard/ohya/internal/claudecode"
	"github.com/nibzard/ohya/internal/codex"
	"github.com/nibzard/ohya/internal/pi"
)

type agentRootFlags struct {
	claude, codex, pi *string
	codexArchived     string
}

func newAgentRootFlags(fs *flag.FlagSet, home string) agentRootFlags {
	return agentRootFlags{
		claude: fs.String("claude-root", filepath.Join(home, ".claude", "projects"), "Claude Code projects `directory` to watch; empty disables Claude Code"),
		codex:  fs.String("codex-root", defaultCodexRoot(home), "Codex sessions `directory` to watch; empty disables Codex; $CODEX_HOME/archived_sessions is watched in addition"),
		pi:     fs.String("pi-root", defaultPiRoot(home), "Pi agent sessions `directory` to watch; empty disables Pi"),

		codexArchived: defaultCodexArchivedRoot(home),
	}
}

func (f agentRootFlags) roots(fs *flag.FlagSet) []agentRoot {
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	var roots []agentRoot
	for _, r := range []struct {
		agent string
		flag  string
		dirs  []agentDir
	}{
		{agent: claudecode.AgentName, flag: "claude-root",
			dirs: []agentDir{{dir: *f.claude, optional: !explicit["claude-root"]}}},
		{agent: codex.AgentName, flag: "codex-root",
			dirs: []agentDir{
				{dir: *f.codex, optional: !explicit["codex-root"]},
				{dir: f.codexArchived, optional: true},
			}},
		{agent: pi.AgentName, flag: "pi-root",
			dirs: []agentDir{{dir: *f.pi, optional: !explicit["pi-root"]}}},
	} {
		if r.dirs[0].dir == "" {
			continue
		}
		if len(r.dirs) > 1 && filepath.Clean(r.dirs[1].dir) == filepath.Clean(r.dirs[0].dir) {
			r.dirs = r.dirs[:1]
		}
		roots = append(roots, agentRoot{agent: r.agent, flag: r.flag, dirs: r.dirs})
	}
	return roots
}
