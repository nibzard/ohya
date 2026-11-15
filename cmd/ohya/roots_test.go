package main

import (
	"flag"
	"testing"
)

func TestAgentRootFlags(t *testing.T) {
	fs := flag.NewFlagSet("roots", flag.ContinueOnError)
	f := newAgentRootFlags(fs, t.TempDir())
	if err := fs.Parse([]string{"--claude-root", "/sessions", "--codex-root", ""}); err != nil {
		t.Fatal(err)
	}
	roots := f.roots(fs)
	if len(roots) != 2 {
		t.Fatalf("roots = %+v", roots)
	}
	if roots[0].dir != "/sessions" || roots[0].optional {
		t.Fatalf("explicit root = %+v", roots[0])
	}
	if roots[1].agent != "pi" || !roots[1].optional {
		t.Fatalf("default root = %+v", roots[1])
	}
}
