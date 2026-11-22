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
	"github.com/nibzard/ohya/internal/observe"
)

var rootSpec = commandSpec{
	name:      "ohya",
	synopsis:  "one memory for all your coding agents",
	usageLine: "ohya <command> [flags]",
	description: "ohya captures Claude Code, Codex, and Pi agent sessions into one hub-owned SQLite database with full-text search, serves the read-only sql and bounded annotate MCP tools to coding agents, and synchronizes devices over an encrypted tailnet through serve and join. " +
		"No command prompts for input: ohya is non-interactive by design.\n" +
		"Exit codes: 0 success, 1 failure, 2 usage error, 3 degraded (ingest --once: at least one source failed or stopped; status, metrics, logs, and diagnose: partial, stale, or degraded observation).",
	argsTitle: "Commands",
	args: `setup      configure memory services and installed agents
service    control the owned ohya background user service
uninstall  remove owned integrations and services; preserve memory
ingest     import agent session history, then poll for new records
serve      run the hub for devices: transport, sync, remote tools
join       pair a device with a hub and upload its agent sessions
mcp        serve the sql and annotate MCP tools on stdio
sql        run one bounded read-only SQL query against stored history
search     find sessions by keyword in the stored events
show       print one session's newest events
sessions   list recent sessions
devices    manage device pairing and revocation
sources    manage stored sources on the hub
purge      plan or apply an owner-decided purge of stored sessions
secrets    report secret-shaped values found in stored records
rederive   rebuild normalized rows and events_fts after a parser upgrade
doctor     run read-only health checks the sql tool cannot answer
status     report runtime state from published local snapshots
metrics    report counter rates and durations over a time window
logs       read structured application logs from local instances
diagnose   export one private, sanitized diagnostics artifact
usage      report how agents use the sql and annotate tools
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
	case "bridge":
		return runBridgeCommand(rest, os.Stdin, stdout)
	case "install":
		return runInstallCommand(rest, stdout, stderr)
	case "setup":
		return runSetupCommand(rest, stdout, stderr)
	case "service":
		return runServiceCommand(rest, stdout, stderr)
	case "uninstall":
		return runUninstallCommand(rest, stdout, stderr)
	case "ingest":
		return runIngestCommand(rest, stdout, stderr)
	case "serve":
		return runServeCommand(rest, stdout, stderr)
	case "join":
		return runJoinCommand(rest, stdout, stderr)
	case "mcp":
		return runMcpCommand(rest, stdout, stderr)
	case "sql":
		return runSQLCommand(rest, os.Stdin, stdout, stderr)
	case "search":
		return runSearchCommand(rest, stdout, stderr)
	case "show":
		return runShowCommand(rest, stdout, stderr)
	case "sessions":
		return runSessionsCommand(rest, stdout, stderr)
	case "devices":
		return runDevicesCommand(rest, stdout, stderr)
	case "purge":
		return runPurgeCommand(rest, stdout, stderr)
	case "secrets":
		return runSecretsCommand(rest, stdout, stderr)
	case "sources":
		return runSourcesCommand(rest, stdout, stderr)
	case "rederive":
		return runRederiveCommand(rest, stdout, stderr)
	case "doctor":
		return runDoctorCommand(rest, stdout, stderr)
	case "status":
		return runStatusCommand(rest, stdout, stderr)
	case "metrics":
		return runMetricsCommand(rest, stdout, stderr)
	case "logs":
		return runLogsCommand(rest, stdout, stderr)
	case "diagnose":
		return runDiagnoseCommand(rest, stdout, stderr)
	case "usage":
		return runUsageCommand(rest, stdout, stderr)
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
		"SIGINT or SIGTERM stops ingest cleanly at any point — during a pass or between passes — and the process exits 0; a restart resumes from the last acknowledged checkpoint. " +
		"Later polls read only the bytes past each acknowledged checkpoint, so a restart resumes where the last commit ended. " +
		"All writes commit through one transactional write path; a malformed record stops its source with a diagnostic and never skips bytes. " +
		"With --once, a pass in which a source failed or stopped exits 3 (degraded); the poll loop keeps running and keeps its stderr diagnostics.\n" +
		"With --json (needs --once), the pass report is one JSON object instead of the text summary (docs/adr/0005).\n" +
		"Repeatable --exclude GLOB flags skip matching session files at discovery: an excluded path is never registered, read, or sent, the pass summary and ohya status report per-agent skip counts, and already-stored sessions stay on the hub.",
	argsTitle: "Arguments",
	args:      "(none)",
	examples:  []string{"ohya ingest --once", "ohya ingest --db ~/.ohya/hub.db", "ohya ingest --exclude '/home/me/.codex/sessions/rollout-*-guardian-*'"},
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
	excludes := newExcludeFlag(c.fs)
	interval := c.fs.Duration("interval", 2*time.Second,
		"polling `interval` in continuous mode")
	allowEmpty := c.fs.Bool("allow-empty", false, "watch missing roots until sessions appear; used by managed setup")
	once := c.fs.Bool("once", false, "run exactly one pass and exit")
	asJSON := c.fs.Bool("json", false,
		"print one JSON object instead of the text summary; needs --once (docs/adr/0005)")
	observeFlag := c.fs.String("observe", "on",
		"in continuous mode, publish local snapshots and metrics under the state directory's observe/ tree: on or off")
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
	observeOn, err := parseObserveFlag(*observeFlag)
	if err != nil {
		return c.usageErrorf("%s", err.Error())
	}
	roots := rootFlags.roots(c.fs)
	stateDir := *state
	if stateDir == "" {
		stateDir = filepath.Dir(*db)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var telem *telemetry
	if !*once {
		telem = startTelemetry(stateDir, observeOn, sqliteSampleFiles(*db), observe.RoleIngest)
		defer telem.close()
	}
	err = runIngest(ctx, ingestOptions{
		dbPath:         *db,
		stateDir:       stateDir,
		roots:          roots,
		excludes:       *excludes,
		interval:       *interval,
		once:           *once,
		allowEmpty:     *allowEmpty,
		maxRecordBytes: *maxRecordBytes,
		jsonOut:        *asJSON,
		stdout:         stdout,
		stderr:         stderr,
		observer:       telem.observer(),
		events:         telem.eventsSink(),
	})
	if err != nil && *asJSON && !errors.Is(err, errDegraded) {
		return printJSONError(stdout, ingestSpec.name, err)
	}
	return err
}

func codexHome(home string) string {
	if dir := os.Getenv("CODEX_HOME"); dir != "" {
		return dir
	}
	return filepath.Join(home, ".codex")
}

func defaultCodexRoot(home string) string {
	return filepath.Join(codexHome(home), "sessions")
}

func defaultCodexArchivedRoot(home string) string {
	return filepath.Join(codexHome(home), "archived_sessions")
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

func defaultAntigravityRoot(home string) string {
	if dir := os.Getenv("AGY_DATA_DIR"); dir != "" {
		return filepath.Join(dir, "brain")
	}
	return filepath.Join(home, ".gemini", "antigravity-cli", "brain")
}
