package main

import (
	"flag"
	"path/filepath"
	"slices"
	"testing"

	"github.com/nibzard/ohya/internal/codex"
)

func TestAgentRootFlags(t *testing.T) {
	fs := flag.NewFlagSet("roots", flag.ContinueOnError)
	f := newAgentRootFlags(fs, t.TempDir())
	if err := fs.Parse([]string{"--claude-root", "/sessions", "--codex-root", ""}); err != nil {
		t.Fatal(err)
	}
	roots := f.roots(fs)
	if len(roots) != 3 {
		t.Fatalf("roots = %+v", roots)
	}
	if roots[0].dirs[0].dir != "/sessions" || roots[0].dirs[0].optional {
		t.Fatalf("explicit root = %+v", roots[0])
	}
	if roots[1].agent != "pi" || !roots[1].dirs[0].optional {
		t.Fatalf("default root = %+v", roots[1])
	}
	if roots[2].agent != "antigravity" || !roots[2].dirs[0].optional {
		t.Fatalf("default antigravity root = %+v", roots[2])
	}
}

func TestCodexRootFlags(t *testing.T) {
	home := t.TempDir()
	archived := filepath.Join(home, ".codex", "archived_sessions")
	for _, tc := range []struct {
		name                string
		codexHome           string
		args                []string
		wantDirs            []string
		wantPrimaryOptional bool
	}{
		{name: "defaults", codexHome: "", wantDirs: []string{
			filepath.Join(home, ".codex", "sessions"), archived}, wantPrimaryOptional: true},
		{name: "CODEX_HOME", codexHome: "/opt/codex", wantDirs: []string{
			filepath.Join("/opt/codex", "sessions"), filepath.Join("/opt/codex", "archived_sessions")},
			wantPrimaryOptional: true},
		{name: "explicit sessions keeps archived", args: []string{"--codex-root", "/elsewhere/sessions"},
			wantDirs: []string{"/elsewhere/sessions", archived}},
		{name: "sessions set to the archived dir watches it once", args: []string{"--codex-root", archived},
			wantDirs: []string{archived}},
		{name: "empty disables codex", args: []string{"--codex-root", ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CODEX_HOME", tc.codexHome)
			fs := flag.NewFlagSet("roots", flag.ContinueOnError)
			f := newAgentRootFlags(fs, home)
			if err := fs.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			var got *agentRoot
			for _, r := range f.roots(fs) {
				if r.agent == codex.AgentName {
					got = &r
				}
			}
			if len(tc.wantDirs) == 0 {
				if got != nil {
					t.Fatalf("codex roots = %+v, want none", got)
				}
				return
			}
			if got == nil {
				t.Fatal("codex root missing")
			}
			var dirs []string
			for _, d := range got.dirs {
				dirs = append(dirs, d.dir)
			}
			if !slices.Equal(dirs, tc.wantDirs) {
				t.Fatalf("dirs = %v, want %v", dirs, tc.wantDirs)
			}
			if got.dirs[0].optional != tc.wantPrimaryOptional {
				t.Fatalf("primary dir optional = %v, want %v", got.dirs[0].optional, tc.wantPrimaryOptional)
			}
			for _, d := range got.dirs[1:] {
				if !d.optional {
					t.Fatalf("extra dir %s must be optional", d.dir)
				}
			}
		})
	}
}
