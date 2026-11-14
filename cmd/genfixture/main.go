// Command genfixture writes a deterministic synthetic session corpus to
// disk, for performance measurements against the real binary. It exists so
// the numbers in docs/performance.md reproduce without real transcripts: the
// same arguments always produce the same bytes.
//
// Usage:
//
//	go run ./cmd/genfixture -agent FORMAT -root DIR [-sessions N] [-mean-bytes B] [-subagents S] [-seed S]
//
// FORMAT is claude-code (default), codex, or pi; each mirrors the on-disk
// layout its agent CLI discovers (docs/performance.md names the matching
// ingest flags). The default shape mirrors the verified real corpus
// averages: sessions of about 512 KiB (real mean 756 KB per file including
// subagent transcripts) with two subagent files each.
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
