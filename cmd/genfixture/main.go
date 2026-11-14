package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/nibzard/ohya/internal/fixgen"
)

func main() {
	agent := flag.String("agent", "claude-code", "corpus `format`: claude-code, codex, or pi")
	root := flag.String("root", "", "output `directory` for the corpus (must not be a real projects tree)")
	sessions := flag.Int("sessions", 200, "number of main `sessions`")
	meanBytes := flag.Int64("mean-bytes", 512<<10, "approximate `bytes` of one main session file")
	subs := flag.Float64("subagents", 2, "mean `subagent` transcripts per session")
	seed := flag.Int64("seed", 7, "corpus `seed`; the same seed reproduces the same bytes")
	flag.Parse()
	if *root == "" {
		fmt.Fprintln(os.Stderr, "genfixture: -root is required")
		os.Exit(1)
	}
	c := fixgen.Corpus{
		Sessions:            *sessions,
		MainMeanBytes:       *meanBytes,
		SubagentsPerSession: *subs,
		Seed:                *seed,
	}
	var total int64
	var err error
	switch *agent {
	case "claude-code":
		total, err = fixgen.Write(*root, c)
	case "codex":
		total, err = fixgen.WriteCodex(*root, c)
	case "pi":
		total, err = fixgen.WritePi(*root, c)
	default:
		fmt.Fprintf(os.Stderr, "genfixture: unknown -agent %q (claude-code, codex, pi)\n", *agent)
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "genfixture:", err)
		os.Exit(1)
	}
	fmt.Printf("genfixture: wrote %s: %d %s sessions, %.1f MiB\n",
		*root, *sessions, *agent, float64(total)/(1<<20))
}
