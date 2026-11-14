// Command ohya runs the local hub: it captures agent session logs into one
// hub-owned SQLite database with full-text search, serves the read-only sql
// and bounded annotate MCP tools to coding agents, and — through serve and
// join — synchronizes devices over an encrypted tailnet with pairing,
// revocation, and the same tools reachable remotely.
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

	"github.com/nibzard/ohya/internal/claudecode"
	"github.com/nibzard/ohya/internal/codex"
	"github.com/nibzard/ohya/internal/pi"
)

const usageText = `ohya — one memory for all your coding agents

Usage:

  ohya ingest    Import Claude Code, Codex, and Pi agent history once, then poll incrementally
  ohya serve     Serve the hub to devices: coordination plane, sync endpoint, remote tools
  ohya join      Pair a device with a hub and upload its agent sessions
  ohya mcp       Serve the sql and annotate MCP tools on stdio (locally or from the joined hub)
  ohya devices   Manage device pairing, credentials, and revocation
  ohya rederive  Rebuild normalized rows and events_fts after a parser upgrade
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
	case "serve":
		return runServeCommand(rest, os.Stdout, os.Stderr)
	case "join":
		return runJoinCommand(rest, os.Stdout, os.Stderr)
	case "mcp":
		return runMcpCommand(rest, os.Stderr)
	case "devices":
		return runDevicesCommand(rest, os.Stdout, os.Stderr)
	case "rederive":
		return runRederiveCommand(rest, os.Stdout, os.Stderr)
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
		"Claude Code projects `directory` to watch; empty disables Claude Code")
	codexRoot := fs.String("codex-root", defaultCodexRoot(home),
		"Codex sessions `directory` to watch ($CODEX_HOME/sessions when set); empty disables Codex")
	piRoot := fs.String("pi-root", defaultPiRoot(home),
		"Pi agent sessions `directory` to watch ($PI_CODING_AGENT_SESSION_DIR or $PI_CODING_AGENT_DIR/sessions when set); empty disables Pi")
	interval := fs.Duration("interval", 2*time.Second,
		"polling `interval` in continuous mode")
	once := fs.Bool("once", false, "run exactly one pass and exit")
	maxRecordBytes := fs.Int64("max-record-bytes", claudecode.DefaultMaxRecordBytes,
		"maximum `bytes` of one record's JSON text; a larger record stops its source, and the diagnostic names this flag as the remedy (docs/adr/0004)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if !*once && *interval <= 0 {
		return errors.New("-interval must be positive")
	}
	if *maxRecordBytes <= 0 {
		return errors.New("-max-record-bytes must be positive")
	}
	// A default directory may be absent (the agent is not installed); one
	// the user passes must exist.
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	var roots []agentRoot
	for _, r := range []agentRoot{
		{agent: claudecode.AgentName, dir: *claudeRoot, flag: "claude-root"},
		{agent: codex.AgentName, dir: *codexRoot, flag: "codex-root"},
		{agent: pi.AgentName, dir: *piRoot, flag: "pi-root"},
	} {
		if r.dir == "" {
			continue
		}
		r.optional = !explicit[r.flag]
		roots = append(roots, r)
	}
	stateDir := *state
	if stateDir == "" {
		stateDir = filepath.Dir(*db)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runIngest(ctx, ingestOptions{
		dbPath:         *db,
		stateDir:       stateDir,
		roots:          roots,
		interval:       *interval,
		once:           *once,
		maxRecordBytes: *maxRecordBytes,
		stdout:         stdout,
		stderr:         stderr,
	})
}

// defaultCodexRoot is the Codex sessions directory: Codex keeps everything
// under $CODEX_HOME, which defaults to ~/.codex (testdata/codex/README.md).
func defaultCodexRoot(home string) string {
	if dir := os.Getenv("CODEX_HOME"); dir != "" {
		return filepath.Join(dir, "sessions")
	}
	return filepath.Join(home, ".codex", "sessions")
}

// defaultPiRoot is the Pi agent sessions root: session files live under
// `<encoded-cwd>/` directories below it. Pi resolves its session directory
// per run from --session-dir, $PI_CODING_AGENT_SESSION_DIR, then the agent
// dir's `sessions/` (testdata/pi/README.md). The environment variables cover
// the persistent cases; a per-run --session-dir is not discoverable, so pass
// -pi-root explicitly for such runs.
func defaultPiRoot(home string) string {
	if dir := os.Getenv("PI_CODING_AGENT_SESSION_DIR"); dir != "" {
		return dir
	}
	if dir := os.Getenv("PI_CODING_AGENT_DIR"); dir != "" {
		return filepath.Join(dir, "sessions")
	}
	return filepath.Join(home, ".pi", "agent", "sessions")
}
