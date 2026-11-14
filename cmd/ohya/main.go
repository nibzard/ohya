// Command ohya runs the local hub: it captures agent session logs into one
// hub-owned SQLite database with full-text search, serves the read-only sql
// and bounded annotate MCP tools to coding agents, and — through serve and
// join — synchronizes devices over an encrypted tailnet with pairing,
// revocation, and the same tools reachable remotely.
package main

import (
	"context"
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

// rootSpec is the top-level usage template. The description carries the
// non-interactive promise and the exit-code contract every command shares.
var rootSpec = commandSpec{
	name:      "ohya",
	synopsis:  "one memory for all your coding agents",
	usageLine: "ohya <command> [flags]",
	description: "ohya captures Claude Code, Codex, and Pi agent sessions into one hub-owned SQLite database with full-text search, serves the read-only sql and bounded annotate MCP tools to coding agents, and synchronizes devices over an encrypted tailnet through serve and join. " +
		"No command prompts for input: ohya is non-interactive by design.\n" +
		"Exit codes: 0 success, 1 failure, 2 usage error, 3 degraded (only ingest --once: at least one source failed or stopped).",
	argsTitle: "Commands",
	args: `ingest     import agent session history, then poll for new records
serve      run the hub for devices: coordination, sync, remote tools
join       pair a device with a hub and upload its agent sessions
mcp        serve the sql and annotate MCP tools on stdio
devices    manage device pairing and revocation
rederive   rebuild normalized rows and events_fts after a parser upgrade
version    print the build, parser, and schema versions`,
	examples: []string{"ohya ingest --once", "ohya mcp"},
}

func main() {
	os.Exit(cliMain(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || isHelpArg(args[0]) {
		writeUsage(stdout, rootSpec, nil)
		return nil
	}
	if isVersionArg(args[0]) {
		return runVersionCommand(args[1:], stdout, stderr)
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "ingest":
		return runIngestCommand(rest, stdout, stderr)
	case "serve":
		return runServeCommand(rest, stdout, stderr)
	case "join":
		return runJoinCommand(rest, stdout, stderr)
	case "mcp":
		return runMcpCommand(rest, stdout, stderr)
	case "devices":
		return runDevicesCommand(rest, stdout, stderr)
	case "rederive":
		return runRederiveCommand(rest, stdout, stderr)
	case "version":
		return runVersionCommand(rest, stdout, stderr)
	default:
		return usageError{cmd: "ohya", msg: fmt.Sprintf("unknown subcommand %q", cmd)}
	}
}

// ingestSpec is the ingest usage template.
var ingestSpec = commandSpec{
	name:      "ohya ingest",
	synopsis:  "import agent session history, then poll for new records",
	usageLine: "ohya ingest [flags]",
	description: "Imports the existing session history of every watched agent once, then polls incrementally until stopped. " +
		"Later polls read only the bytes past each acknowledged checkpoint, so a restart resumes where the last commit ended. " +
		"All writes commit through one transactional write path; a malformed record stops its source with a diagnostic and never skips bytes. " +
		"With --once, a pass in which a source failed or stopped exits 3 (degraded); the poll loop keeps running and keeps its stderr diagnostics.",
	argsTitle: "Arguments",
	args:      "(none)",
	examples:  []string{"ohya ingest --once", "ohya ingest --db ~/.ohya/hub.db"},
}

// runIngestCommand parses the ingest flags and runs local ingestion until the
// process receives SIGINT or SIGTERM.
func runIngestCommand(args []string, stdout, stderr io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	c := newCommandFlags(ingestSpec)
	db := c.fs.String("db", filepath.Join(home, ".ohya", "hub.db"),
		"hub database `path`")
	state := c.fs.String("state", "",
		"state `directory` for the device identity and checkpoints; defaults to the database directory")
	claudeRoot := c.fs.String("claude-root", filepath.Join(home, ".claude", "projects"),
		"Claude Code projects `directory` to watch; empty disables Claude Code")
	codexRoot := c.fs.String("codex-root", defaultCodexRoot(home),
		"Codex sessions `directory` to watch; empty disables Codex")
	piRoot := c.fs.String("pi-root", defaultPiRoot(home),
		"Pi agent sessions `directory` to watch; empty disables Pi")
	interval := c.fs.Duration("interval", 2*time.Second,
		"polling `interval` in continuous mode")
	once := c.fs.Bool("once", false, "run exactly one pass and exit")
	maxRecordBytes := c.fs.Int64("max-record-bytes", claudecode.DefaultMaxRecordBytes,
		"maximum `bytes` of one record's JSON text; a larger record stops its source (docs/adr/0004)")
	if err := c.parse(args, stdout); err != nil {
		return err
	}
	if c.fs.NArg() > 0 {
		return c.usageErrorf("unexpected argument %q", c.fs.Arg(0))
	}
	if !*once && *interval <= 0 {
		return c.usageErrorf("--interval must be positive")
	}
	if *maxRecordBytes <= 0 {
		return c.usageErrorf("--max-record-bytes must be positive")
	}
	// A default directory may be absent (the agent is not installed); one
	// the user passes must exist.
	explicit := map[string]bool{}
	c.fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
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
// --pi-root explicitly for such runs.
func defaultPiRoot(home string) string {
	if dir := os.Getenv("PI_CODING_AGENT_SESSION_DIR"); dir != "" {
		return dir
	}
	if dir := os.Getenv("PI_CODING_AGENT_DIR"); dir != "" {
		return filepath.Join(dir, "sessions")
	}
	return filepath.Join(home, ".pi", "agent", "sessions")
}
