package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/nibzard/ohya/internal/fixgen"
)

func main() {
	agent := flag.String("agent", "claude-code", "corpus `format`: claude-code, codex, pi, or mix")
	root := flag.String("root", "", "output `directory` for the corpus (must not be a real projects tree)")
	sessions := flag.Int("sessions", 200, "number of main `sessions`")
	meanBytes := flag.Int64("mean-bytes", 512<<10, "approximate `bytes` of one main session file")
	subs := flag.Float64("subagents", 2, "mean `subagent` transcripts per session")
	seed := flag.Int64("seed", 7, "corpus `seed`; the same seed reproduces the same bytes")
	fixture := flag.String("fixture", "import", "named mix `fixture`: import or query")
	scale := flag.Float64("scale", fixgen.ImportScale,
		"`fraction` of the observed session counts (import fixture only)")
	flag.Parse()
	if *root == "" {
		fmt.Fprintln(os.Stderr, "genfixture: -root is required")
		os.Exit(1)
	}
	if *agent == "mix" {
		writeMix(*root, *fixture, *scale, *seed)
		return
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
		fmt.Fprintf(os.Stderr, "genfixture: unknown -agent %q (claude-code, codex, pi, mix)\n", *agent)
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "genfixture:", err)
		os.Exit(1)
	}
	fmt.Printf("genfixture: wrote %s: %d %s sessions, %.1f MiB\n",
		*root, *sessions, *agent, float64(total)/(1<<20))
}

func writeMix(root, fixture string, scale float64, seed int64) {
	var m fixgen.Mix
	switch fixture {
	case "import":
		m = fixgen.ImportMix(scale, seed)
	case "query":
		m = fixgen.QueryMix(seed)
	default:
		fmt.Fprintf(os.Stderr, "genfixture: unknown -fixture %q (import, query)\n", fixture)
		os.Exit(1)
	}
	corpora := fixgen.GenerateMix(m)
	total, err := fixgen.WriteAgentFiles(root, corpora)
	if err != nil {
		fmt.Fprintln(os.Stderr, "genfixture:", err)
		os.Exit(1)
	}
	stats, err := fixgen.MeasureMix(m, corpora)
	if err != nil {
		fmt.Fprintln(os.Stderr, "genfixture:", err)
		os.Exit(1)
	}
	fmt.Printf("genfixture: wrote %s: %s fixture seed=%d scale=%.4f, one directory per agent\n",
		root, fixture, seed, m.Scale)
	var events int64
	var files int
	for _, s := range stats {
		fmt.Printf("genfixture: %s sessions=%d files=%d records=%d events=%d bytes=%d mean-record=%d max-record=%d\n",
			s.Agent, s.Sessions, s.Files, s.Records, s.Events, s.Bytes, s.MeanRecord, s.MaxRecord)
		events += s.Events
		files += s.Files
	}
	fmt.Printf("genfixture: total files=%d events=%d bytes=%d (%.1f MiB)\n",
		files, events, total, float64(total)/(1<<20))
}
