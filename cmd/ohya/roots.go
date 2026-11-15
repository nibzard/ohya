package main

import (
	"flag"
	"path/filepath"

	"github.com/nibzard/ohya/internal/claudecode"
	"github.com/nibzard/ohya/internal/codex"
	"github.com/nibzard/ohya/internal/pi"
)

type agentRootFlags struct{ claude, codex, pi *string }

func newAgentRootFlags(fs *flag.FlagSet, home string) agentRootFlags {
	return agentRootFlags{
		claude: fs.String("claude-root", filepath.Join(home, ".claude", "projects"), "Claude Code projects `directory` to watch; empty disables Claude Code"),
		codex:  fs.String("codex-root", defaultCodexRoot(home), "Codex sessions `directory` to watch; empty disables Codex"),
		pi:     fs.String("pi-root", defaultPiRoot(home), "Pi agent sessions `directory` to watch; empty disables Pi"),
	}
}

func (f agentRootFlags) roots(fs *flag.FlagSet) []agentRoot {
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	var roots []agentRoot
	for _, r := range []agentRoot{
		{agent: claudecode.AgentName, dir: *f.claude, flag: "claude-root"},
		{agent: codex.AgentName, dir: *f.codex, flag: "codex-root"},
		{agent: pi.AgentName, dir: *f.pi, flag: "pi-root"},
	} {
		if r.dir != "" {
			r.optional = !explicit[r.flag]
			roots = append(roots, r)
		}
	}
	return roots
}
