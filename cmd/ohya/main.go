// Command ohya runs the local hub: it captures agent session logs into one
// hub-owned SQLite database with full-text search, and serves the read-only
// sql MCP tool to coding agents. The annotate MCP tool arrives in a later
// round.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

const usageText = `ohya — one memory for all your coding agents

Usage:

  ohya ingest   Import Claude Code history once, then poll incrementally
  ohya mcp      Serve the read-only sql MCP tool on stdio
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "ohya:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usageText)
		return errors.New("missing subcommand")
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "ingest":
		return runIngestCommand(rest, os.Stdout, os.Stderr)
	case "mcp":
		return runMcpCommand(rest, os.Stderr)
	default:
		fmt.Fprint(os.Stderr, usageText)
		return fmt.Errorf("unknown subcommand %q", cmd)
	}
}

// runIngestCommand parses the ingest flags and runs local ingestion until the
// process receives SIGINT or SIGTERM.
func runIngestCommand(args []string, stdout, stderr io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	fs := flag.NewFlagSet("ohya ingest", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", filepath.Join(home, ".ohya", "hub.db"),
		"hub database `path`")
	state := fs.String("state", "",
		"state `directory` for the device identity and source checkpoints (default: the database directory)")
	claudeRoot := fs.String("claude-root", filepath.Join(home, ".claude", "projects"),
		"Claude Code projects `directory` to watch")
	interval := fs.Duration("interval", 2*time.Second,
		"polling `interval` in continuous mode")
	once := fs.Bool("once", false, "run exactly one pass and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if !*once && *interval <= 0 {
		return errors.New("-interval must be positive")
	}
	stateDir := *state
	if stateDir == "" {
		stateDir = filepath.Dir(*db)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runIngest(ctx, ingestOptions{
		dbPath:     *db,
		stateDir:   stateDir,
		claudeRoot: *claudeRoot,
		interval:   *interval,
		once:       *once,
		stdout:     stdout,
		stderr:     stderr,
	})
}
