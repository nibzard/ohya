package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/nibzard/ohya/internal/claudecode"
)

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
doctor     run read-only health checks the sql tool cannot answer
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
	case "doctor":
		return runDoctorCommand(rest, stdout, stderr)
	case "version":
		return runVersionCommand(rest, stdout, stderr)
	default:
		return usageError{cmd: "ohya", msg: fmt.Sprintf("unknown subcommand %q", cmd)}
	}
}

var ingestSpec = commandSpec{
	name:      "ohya ingest",
	synopsis:  "import agent session history, then poll for new records",
	usageLine: "ohya ingest [flags]",
	description: "Imports the existing session history of every watched agent once, then polls incrementally until stopped. " +
		"Later polls read only the bytes past each acknowledged checkpoint, so a restart resumes where the last commit ended. " +
		"All writes commit through one transactional write path; a malformed record stops its source with a diagnostic and never skips bytes. " +
		"With --once, a pass in which a source failed or stopped exits 3 (degraded); the poll loop keeps running and keeps its stderr diagnostics.\n" +
		"With --json (needs --once), the pass report is one JSON object instead of the text summary (docs/adr/0005).",
	argsTitle: "Arguments",
	args:      "(none)",
	examples:  []string{"ohya ingest --once", "ohya ingest --db ~/.ohya/hub.db"},
}

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
	rootFlags := newAgentRootFlags(c.fs, home)
	interval := c.fs.Duration("interval", 2*time.Second,
		"polling `interval` in continuous mode")
	once := c.fs.Bool("once", false, "run exactly one pass and exit")
	asJSON := c.fs.Bool("json", false,
		"print one JSON object instead of the text summary; needs --once (docs/adr/0005)")
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
	if *asJSON && !*once {
		return c.usageErrorf("--json needs --once; the polling loop prints no JSON")
	}
	if *maxRecordBytes <= 0 {
		return c.usageErrorf("--max-record-bytes must be positive")
	}
	roots := rootFlags.roots(c.fs)
	stateDir := *state
	if stateDir == "" {
		stateDir = filepath.Dir(*db)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = runIngest(ctx, ingestOptions{
		dbPath:         *db,
		stateDir:       stateDir,
		roots:          roots,
		interval:       *interval,
		once:           *once,
		maxRecordBytes: *maxRecordBytes,
		jsonOut:        *asJSON,
		stdout:         stdout,
		stderr:         stderr,
	})
	if err != nil && *asJSON && !errors.Is(err, errDegraded) {
		return printJSONError(stdout, ingestSpec.name, err)
	}
	return err
}

func defaultCodexRoot(home string) string {
	if dir := os.Getenv("CODEX_HOME"); dir != "" {
		return filepath.Join(dir, "sessions")
	}
	return filepath.Join(home, ".codex", "sessions")
}

func defaultPiRoot(home string) string {
	if dir := os.Getenv("PI_CODING_AGENT_SESSION_DIR"); dir != "" {
		return dir
	}
	if dir := os.Getenv("PI_CODING_AGENT_DIR"); dir != "" {
		return filepath.Join(dir, "sessions")
	}
	return filepath.Join(home, ".pi", "agent", "sessions")
}
