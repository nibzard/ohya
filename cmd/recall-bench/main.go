// Command recall-bench measures how well a fresh coding agent recalls facts
// from earlier sessions through two access paths: raw transcript files on
// disk versus the ohya sql MCP tool over the hub database.
//
// Modes:
//
//	recall-bench plant -out DIR            generate canary facts and record
//	                                       them through the real agent CLIs
//	recall-bench run -out DIR -questions F run both arms over the question
//	                                       set and write report.md
//
// The design and protocol are documented in the repository docs and in the
// research notes; see docs/reference.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "recall-bench:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: recall-bench plant|run [flags]; see -h per mode")
	}
	switch args[0] {
	case "plant":
		return runPlant(args[1:])
	case "run":
		return runBench(args[1:])
	case "regrade":
		return runRegrade(args[1:])
	default:
		return fmt.Errorf("unknown mode %q; want plant, run, or regrade", args[0])
	}
}

func runPlant(args []string) error {
	fs := flag.NewFlagSet("plant", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	out := fs.String("out", "recall-bench", "output `directory` for canary facts and questions; must be empty or absent")
	claude := fs.String("claude", "claude", "`command` of the Claude Code client")
	codex := fs.String("codex", "codex", "`command` of the Codex client")
	pi := fs.String("pi", "pi", "`command` of the Pi client")
	seed := fs.Int64("seed", time.Now().UnixNano(), "random `seed` for canary codewords and values")
	home := fs.String("home", os.Getenv("HOME"), "working `directory` and HOME for the planting runs (pre-trusted for all three agents)")
	timeout := fs.Duration("timeout", 3*time.Minute, "per-session `timeout` for one planting run")
	var skip stringSliceFlag
	fs.Var(&skip, "skip", "agent to skip planting (claude, codex, pi); repeatable")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return plant(ctx, plantOptions{
		out:     *out,
		claude:  *claude,
		codex:   *codex,
		pi:      *pi,
		seed:    *seed,
		home:    *home,
		timeout: *timeout,
		skip:    skip.values,
	})
}

func runBench(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	out := fs.String("out", "recall-bench", "output `directory` shared with the plant step")
	var questions stringSliceFlag
	fs.Var(&questions, "questions", "`file` with curated questions in JSON array form; repeatable; canary questions load from DIR/canary-questions.json when present")
	ohya := fs.String("ohya", "bin/ohya", "`path` to the ohya binary serving the mcp tool")
	claude := fs.String("claude", "claude", "`command` of the Claude Code client under test")
	hubDB := fs.String("hub-db", "", "path to the hub `database` the ohya mcp tool opens (default the production ~/.ohya/hub.db)")
	state := fs.String("state", "", "`directory` for the mcp resolver state (default DIR/resolver-state)")
	repeats := fs.Int("repeats", 3, "repeats per question per arm")
	timeout := fs.Duration("timeout", 3*time.Minute, "per-run `timeout` for one agent run")
	var roots stringSliceFlag
	roots.values = defaultRoots()
	fs.Var(&roots, "roots", "transcript root `directory` named to the raw arm; give three in claude,codex,pi order (defaults to the standard home locations)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if *repeats < 1 {
		return errors.New("-repeats must be at least 1")
	}
	if len(roots.values) != 3 {
		return fmt.Errorf("-roots wants exactly three directories (claude, codex, pi), got %d", len(roots.values))
	}
	return bench(context.Background(), benchOptions{
		out:       *out,
		questions: questions.values,
		ohya:      *ohya,
		claude:    *claude,
		hubDB:     *hubDB,
		state:     *state,
		repeats:   *repeats,
		timeout:   *timeout,
		roots:     roots.values,
	})
}

func runRegrade(args []string) error {
	fs := flag.NewFlagSet("regrade", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	out := fs.String("out", "recall-bench", "output `directory` of a finished or running bench")
	questions := fs.String("questions", "", "`file` with the curated questions used by the run")
	ohya := fs.String("ohya", "bin/ohya", "`path` to the ohya binary serving the mcp tool")
	hubDB := fs.String("hub-db", "", "path to the hub `database` (default the production ~/.ohya/hub.db)")
	state := fs.String("state", "", "`directory` for the mcp resolver state (default DIR/resolver-state)")
	repeats := fs.Int("repeats", 3, "repeats recorded in the report header")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	var qpaths []string
	if *questions != "" {
		qpaths = append(qpaths, *questions)
	}
	return regrade(context.Background(), benchOptions{
		out:       *out,
		questions: qpaths,
		ohya:      *ohya,
		hubDB:     *hubDB,
		state:     *state,
		repeats:   *repeats,
		roots:     defaultRoots(),
	})
}

func defaultRoots() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "/home/agent"
	}
	return []string{
		filepath.Join(home, ".claude", "projects"),
		filepath.Join(home, ".codex", "sessions"),
		filepath.Join(home, ".pi", "agent", "sessions"),
	}
}

// stringSliceFlag collects a repeatable string flag.
type stringSliceFlag struct {
	values []string
}

func (f *stringSliceFlag) String() string {
	return strings.Join(f.values, ",")
}

func (f *stringSliceFlag) Set(v string) error {
	f.values = append(f.values, v)
	return nil
}
