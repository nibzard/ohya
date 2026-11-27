package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nibzard/ohya/internal/device"
	"github.com/nibzard/ohya/internal/hubsync"
	"github.com/nibzard/ohya/internal/mcpserver"
	"github.com/nibzard/ohya/internal/setup"
	"github.com/nibzard/ohya/internal/store"
	"github.com/nibzard/ohya/internal/transport/address"
)

const (
	searchHitLimit      = 20
	recallExcerptChars  = 200
	showDefaultLast     = 20
	sessionsDefaultLim  = 20
	sessionsMaxLimit    = 500
	settledWindowClause = "(s.last_event_at_ms IS NULL OR s.last_event_at_ms <= (CAST(strftime('%s', 'now') AS INTEGER) - 600) * 1000)"
)

var sqlSpec = commandSpec{
	name:      "ohya sql",
	synopsis:  "run one bounded read-only SQL query against stored history",
	usageLine: "ohya sql <query | -> [flags]",
	description: "Runs one bounded read-only SQL statement (SELECT or allow-listed read-only PRAGMA) through the sql MCP tool's engine and prints the result as a table or as one JSON object with --json (docs/adr/0005). " +
		"Pass - as the query argument to read one SQL statement from stdin (bounded to 64 KiB). " +
		"Without --remote the command reads the local hub database and never writes or migrates it; with --remote it queries the joined hub through the saved pairing credential. " +
		"All sql tool safeguards apply unchanged: single statement, SQLite authorizer, 500 rows, 256 KiB output, 5 second deadline, and ADR 0015 egress secret redaction.",
	args: "<query | ->  one SQL SELECT or read-only PRAGMA statement, or - to read it from stdin",
	examples: []string{
		`ohya sql "SELECT agent, count(*) FROM sessions GROUP BY agent"`,
		`ohya sql --json "SELECT id, repo, last_event_at_ms FROM sessions ORDER BY last_event_at_ms DESC LIMIT 5"`,
		`ohya sql --remote --json - < query.sql`,
	},
}

var searchSpec = commandSpec{
	name:      "ohya search",
	synopsis:  "find sessions by keyword in the stored events",
	usageLine: "ohya search <query> [flags]",
	description: "Runs one bounded read-only FTS5 query with BM25 ranking over stored events and prints the best " +
		strconv.Itoa(searchHitLimit) + " matches, each with its event id, session id, agent, repo, time, and a text excerpt. " +
		"The query is SQLite FTS5 syntax, for example 'auth AND refresh' or '\"exact phrase\"'. " +
		"A term with punctuation — an apostrophe, or a hyphenated directory or branch name — must sit inside double quotes, as in '\"auth-refresh\"'. " +
		"FTS5 parses '-name' as a column filter, so a bare hyphenated term fails with a no-such-column error; other bare punctuation fails with a syntax error. " +
		"Without --remote the command reads the local hub database and never writes or migrates it; with --remote it queries the joined hub through the saved pairing credential. " +
		"Bounds and the read-only policy are the sql tool's engine: at most 500 rows and 256 KiB per call, 5 seconds per query.",
	args: "<query>  FTS5 query text, for example 'auth AND refresh'",
	examples: []string{
		`ohya search "auth AND refresh"`,
		`ohya search '"hyphenated-name"'`,
		`ohya search panic --repo github.com/owner/name --agent claude-code`,
		`ohya search --remote --json "cursor regression"`,
	},
}

var showSpec = commandSpec{
	name:      "ohya show",
	synopsis:  "print one session's newest events",
	usageLine: "ohya show <session-id> [flags]",
	description: "Prints one session's header — agent, device, native id, repo, working directory, branch, model, first prompt, timing, event count — followed by its newest events, excerpted, oldest first. " +
		"Find the session id with ohya search or ohya sessions. Byte-identical replay duplicates stay hidden. " +
		"Without --remote the command reads the local hub database and never writes or migrates it; with --remote it reads the joined hub through the saved pairing credential.",
	args: "<session-id>  session id as printed by ohya search or ohya sessions",
	examples: []string{
		"ohya show claude-code:dev1:sess-1",
		"ohya show --last 50 claude-code:dev1:sess-1",
		"ohya show --remote --json claude-code:dev1:sess-1",
	},
}

var sessionsSpec = commandSpec{
	name:      "ohya sessions",
	synopsis:  "list recent sessions",
	usageLine: "ohya sessions [flags]",
	description: "Lists the most recently active non-synthetic sessions with their agent, device, repository, working directory, branch, model, timing, event count, settled state (idle at least 10 minutes), and excerpted first prompt. " +
		"Without --remote the command reads the local hub database and never writes or migrates it; with --remote it queries the joined hub through the saved pairing credential. " +
		"The query runs through the sql tool's read-only engine with egress secret redaction.",
	examples: []string{
		"ohya sessions",
		"ohya sessions --limit 10 --agent claude-code",
		"ohya sessions --repo github.com/owner/name --json",
		"ohya sessions --remote --json",
	},
}

type searchJSONData struct {
	Query            string          `json:"query"`
	Matches          []searchHitJSON `json:"matches"`
	Truncated        bool            `json:"truncated"`
	TruncationReason string          `json:"truncation_reason,omitempty"`
	Redactions       int             `json:"redactions,omitempty"`
	InjectionFlags   int             `json:"injection_flags,omitempty"`
}

type searchHitJSON struct {
	EventID   int64  `json:"event_id"`
	SessionID string `json:"session_id"`
	Agent     string `json:"agent"`
	Repo      string `json:"repo,omitempty"`
	Time      string `json:"time,omitempty"`
	TimeMS    *int64 `json:"time_ms,omitempty"`
	Excerpt   string `json:"excerpt"`
}

type showJSONData struct {
	Session          sessionSummaryJSON `json:"session"`
	Events           []showEventJSON    `json:"events"`
	Truncated        bool               `json:"truncated"`
	TruncationReason string             `json:"truncation_reason,omitempty"`
	Redactions       int                `json:"redactions,omitempty"`
	InjectionFlags   int                `json:"injection_flags,omitempty"`
}

type sessionsJSONData struct {
	Sessions         []sessionSummaryJSON `json:"sessions"`
	Truncated        bool                 `json:"truncated"`
	TruncationReason string               `json:"truncation_reason,omitempty"`
	Redactions       int                  `json:"redactions,omitempty"`
	InjectionFlags   int                  `json:"injection_flags,omitempty"`
}

type sessionSummaryJSON struct {
	ID            string `json:"id"`
	Agent         string `json:"agent"`
	DeviceID      string `json:"device_id"`
	NativeID      string `json:"native_id"`
	Repo          string `json:"repo,omitempty"`
	CWD           string `json:"cwd,omitempty"`
	Branch        string `json:"branch,omitempty"`
	Model         string `json:"model,omitempty"`
	FirstPrompt   string `json:"first_prompt,omitempty"`
	StartedAt     string `json:"started_at,omitempty"`
	StartedAtMS   *int64 `json:"started_at_ms,omitempty"`
	LastEventAt   string `json:"last_event_at,omitempty"`
	LastEventAtMS *int64 `json:"last_event_at_ms,omitempty"`
	EventCount    int64  `json:"event_count"`
	Settled       *bool  `json:"settled,omitempty"`
}

type showEventJSON struct {
	ID       int64  `json:"id"`
	Time     string `json:"time,omitempty"`
	TimeMS   *int64 `json:"time_ms,omitempty"`
	Kind     string `json:"kind"`
	Role     string `json:"role,omitempty"`
	ToolName string `json:"tool_name,omitempty"`
	Excerpt  string `json:"excerpt"`
}

func runSQLCommand(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	c := newCommandFlags(sqlSpec)
	db := c.fs.String("db", filepath.Join(home, ".ohya", "hub.db"),
		"hub database `path` (ignored with --remote)")
	state := c.fs.String("state", "",
		"state `directory`: the join state with --remote, otherwise the setup state root; defaults to the database directory, or ~/.ohya/join with --remote")
	remote := c.fs.Bool("remote", false,
		"query the joined hub through the tailcat transport instead of a local database")
	asJSON := c.fs.Bool("json", false, "print one JSON object instead of text (docs/adr/0005)")
	if err := c.parse(args, stdout); err != nil {
		return err
	}
	if c.fs.NArg() != 1 {
		return c.usageErrorf("sql needs exactly one SQL query argument (or - for stdin)")
	}
	query := c.fs.Arg(0)
	if query == "-" {
		raw, err := io.ReadAll(io.LimitReader(stdin, mcpserver.DefaultMaxQueryBytes+1))
		if err != nil {
			return jsonFailurePrint(*asJSON, stdout, sqlSpec.name, classify(fmt.Errorf("read query from stdin: %w", err), "query_read_failed", ""))
		}
		if len(raw) > mcpserver.DefaultMaxQueryBytes {
			return jsonFailurePrint(*asJSON, stdout, sqlSpec.name, classify(
				fmt.Errorf("%w: stdin exceeds the %d byte input limit", mcpserver.ErrQueryTooLarge, mcpserver.DefaultMaxQueryBytes),
				"query_too_large",
				"shorten the query to at most 64 KiB",
			))
		}
		query = string(raw)
	}
	target, err := resolveRecallTarget(home, *state, *db, *remote, dbFlagExplicit(c))
	if err != nil {
		return jsonFailurePrint(*asJSON, stdout, sqlSpec.name, err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	backend, closeBackend, err := openRecallBackend(target)
	if err != nil {
		return jsonFailurePrint(*asJSON, stdout, sqlSpec.name, err)
	}
	defer closeBackend()
	res, err := recallQuery(ctx, backend, query)
	if err != nil {
		return jsonFailurePrint(*asJSON, stdout, sqlSpec.name, err)
	}
	if *asJSON {
		return printJSON(stdout, sqlSpec.name, res)
	}
	printSQLResult(stdout, res)
	return nil
}

func runSearchCommand(args []string, stdout, stderr io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	c := newCommandFlags(searchSpec)
	db := c.fs.String("db", filepath.Join(home, ".ohya", "hub.db"),
		"hub database `path` (ignored with --remote)")
	state := c.fs.String("state", "",
		"state `directory`: the join state with --remote, otherwise the setup state root; defaults to the database directory, or ~/.ohya/join with --remote")
	remote := c.fs.Bool("remote", false,
		"query the joined hub through the tailcat transport instead of a local database")
	repo := c.fs.String("repo", "",
		"only sessions of this `repo` (host/path, as stored in sessions.repo)")
	agent := c.fs.String("agent", "",
		"only sessions of this `agent` (for example claude-code)")
	asJSON := c.fs.Bool("json", false, "print one JSON object instead of text (docs/adr/0005)")
	if err := c.parse(args, stdout); err != nil {
		return err
	}
	if c.fs.NArg() != 1 {
		return c.usageErrorf("search needs exactly one FTS query argument")
	}
	target, err := resolveRecallTarget(home, *state, *db, *remote, dbFlagExplicit(c))
	if err != nil {
		return jsonFailurePrint(*asJSON, stdout, searchSpec.name, err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	backend, closeBackend, err := openRecallBackend(target)
	if err != nil {
		return jsonFailurePrint(*asJSON, stdout, searchSpec.name, err)
	}
	defer closeBackend()
	ftsQuery := c.fs.Arg(0)
	res, err := recallQuery(ctx, backend, searchQuery(ftsQuery, *repo, *agent))
	if err != nil {
		return jsonFailurePrint(*asJSON, stdout, searchSpec.name, ftsQuotingHint(err, ftsQuery))
	}
	if *asJSON {
		return printJSON(stdout, searchSpec.name, buildSearchJSON(ftsQuery, res))
	}
	printSearchResult(stdout, res)
	return nil
}

func runShowCommand(args []string, stdout, stderr io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	c := newCommandFlags(showSpec)
	db := c.fs.String("db", filepath.Join(home, ".ohya", "hub.db"),
		"hub database `path` (ignored with --remote)")
	state := c.fs.String("state", "",
		"state `directory`: the join state with --remote, otherwise the setup state root; defaults to the database directory, or ~/.ohya/join with --remote")
	remote := c.fs.Bool("remote", false,
		"read the session from the joined hub through the tailcat transport instead of a local database")
	last := c.fs.Int("last", showDefaultLast,
		"number of newest `events` to print")
	asJSON := c.fs.Bool("json", false, "print one JSON object instead of text (docs/adr/0005)")
	if err := c.parse(args, stdout); err != nil {
		return err
	}
	if c.fs.NArg() != 1 {
		return c.usageErrorf("show needs exactly one session id")
	}
	if *last < 1 {
		return c.usageErrorf("--last must be a positive integer")
	}
	target, err := resolveRecallTarget(home, *state, *db, *remote, dbFlagExplicit(c))
	if err != nil {
		return jsonFailurePrint(*asJSON, stdout, showSpec.name, err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	backend, closeBackend, err := openRecallBackend(target)
	if err != nil {
		return jsonFailurePrint(*asJSON, stdout, showSpec.name, err)
	}
	defer closeBackend()
	sessionID := c.fs.Arg(0)
	header, err := recallQuery(ctx, backend, showSessionQuery(sessionID))
	if err != nil {
		return jsonFailurePrint(*asJSON, stdout, showSpec.name, err)
	}
	if header.RowCount != 1 {
		err := classify(
			fmt.Errorf("no session with id %q; find session ids with ohya search", sessionID),
			"session_not_found",
			"find session ids with ohya search or ohya sessions",
		)
		return jsonFailurePrint(*asJSON, stdout, showSpec.name, err)
	}
	events, err := recallQuery(ctx, backend, showEventsQuery(sessionID, *last))
	if err != nil {
		return jsonFailurePrint(*asJSON, stdout, showSpec.name, err)
	}
	if *asJSON {
		return printJSON(stdout, showSpec.name, buildShowJSON(header, events))
	}
	printShowResult(stdout, header, events)
	return nil
}

func runSessionsCommand(args []string, stdout, stderr io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	c := newCommandFlags(sessionsSpec)
	db := c.fs.String("db", filepath.Join(home, ".ohya", "hub.db"),
		"hub database `path` (ignored with --remote)")
	state := c.fs.String("state", "",
		"state `directory`: the join state with --remote, otherwise the setup state root; defaults to the database directory, or ~/.ohya/join with --remote")
	remote := c.fs.Bool("remote", false,
		"query the joined hub through the tailcat transport instead of a local database")
	repo := c.fs.String("repo", "",
		"only sessions of this `repo` (host/path, as stored in sessions.repo)")
	agent := c.fs.String("agent", "",
		"only sessions of this `agent` (for example claude-code)")
	limit := c.fs.Int("limit", sessionsDefaultLim,
		"maximum number of recent `sessions` to list (1–500)")
	asJSON := c.fs.Bool("json", false, "print one JSON object instead of text (docs/adr/0005)")
	if err := c.parse(args, stdout); err != nil {
		return err
	}
	if c.fs.NArg() > 0 {
		return c.usageErrorf("unexpected argument %q", c.fs.Arg(0))
	}
	if *limit < 1 || *limit > sessionsMaxLimit {
		return c.usageErrorf("--limit must be between 1 and %d", sessionsMaxLimit)
	}
	target, err := resolveRecallTarget(home, *state, *db, *remote, dbFlagExplicit(c))
	if err != nil {
		return jsonFailurePrint(*asJSON, stdout, sessionsSpec.name, err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	backend, closeBackend, err := openRecallBackend(target)
	if err != nil {
		return jsonFailurePrint(*asJSON, stdout, sessionsSpec.name, err)
	}
	defer closeBackend()
	res, err := recallQuery(ctx, backend, sessionsQuery(*repo, *agent, *limit))
	if err != nil {
		return jsonFailurePrint(*asJSON, stdout, sessionsSpec.name, err)
	}
	if *asJSON {
		return printJSON(stdout, sessionsSpec.name, buildSessionsJSON(res))
	}
	printSessionsResult(stdout, res)
	return nil
}

type recallTarget struct {
	remote   bool
	dbPath   string
	stateDir string
}

func resolveRecallTarget(home, state, dbPath string, remoteFlag, dbExplicit bool) (recallTarget, error) {
	setupState := state
	if setupState == "" {
		setupState = filepath.Dir(dbPath)
	}
	connection, err := setup.LoadConnection(setupState)
	if err != nil {
		return recallTarget{}, err
	}
	remote := remoteFlag || (connection.Mode == "remote" && !dbExplicit)
	joinState := state
	if joinState == "" {
		if !remoteFlag && connection.Mode == "remote" {
			joinState = filepath.Join(setupState, "join")
		} else {
			joinState = filepath.Join(home, ".ohya", "join")
		}
	}
	return recallTarget{remote: remote, dbPath: dbPath, stateDir: joinState}, nil
}

func dbFlagExplicit(c *commandFlagSet) bool {
	explicit := false
	c.fs.Visit(func(f *flag.Flag) {
		if f.Name == "db" {
			explicit = true
		}
	})
	return explicit
}

type recallBackend interface {
	Query(ctx context.Context, query string) (mcpserver.Result, error)
}

func missingHubDatabase(path string) error {
	return classify(
		fmt.Errorf("hub database %s does not exist; run ohya setup to start capturing sessions on this machine, or pass --remote to query a joined hub", path),
		"database_open_failed",
		"run ohya setup to start capturing sessions, or pass --remote to query a joined hub",
	)
}

func openRecallBackend(target recallTarget) (recallBackend, func(), error) {
	if target.remote {
		return openRemoteRecallBackend(target.stateDir)
	}
	if _, err := os.Stat(target.dbPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, missingHubDatabase(target.dbPath)
		}
		return nil, nil, classify(
			fmt.Errorf("stat %s: %w", target.dbPath, err),
			"database_open_failed",
			"check the --db path and its permissions",
		)
	}
	hub, err := store.OpenReadOnly(target.dbPath)
	if err != nil {
		return nil, nil, classify(
			fmt.Errorf("open %s read-only: %w; check the --db path and its permissions; another ohya process may hold the database", target.dbPath, err),
			"database_open_failed",
			"check the --db path and its permissions; another ohya process may hold the database",
		)
	}
	reader, err := hub.Reader()
	if err != nil {
		hub.Close()
		return nil, nil, classify(err, "database_open_failed", "check the --db path and its permissions")
	}
	closeBackend := func() {
		reader.Close()
		hub.Close()
	}
	return mcpserver.NewEngine(reader, nil, mcpserver.EngineOptions{}), closeBackend, nil
}

func openRemoteRecallBackend(stateDir string) (recallBackend, func(), error) {
	cred, err := device.LoadCredential(stateDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, classify(
			fmt.Errorf("this state directory holds no pairing credential; run ohya join first"),
			"not_paired",
			"run ohya join with a one-time code from the hub first",
		)
	}
	if err != nil {
		return nil, nil, err
	}
	hubAddr, err := device.LoadHubAddress(stateDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, classify(
			fmt.Errorf("this state directory holds no hub address; run ohya join with an ohya1 code once to save it"),
			"not_paired",
			"run ohya join with an ohya1 code once to save the hub address",
		)
	}
	if err != nil {
		return nil, nil, err
	}
	node := address.NewDevice(address.DeviceOptions{})
	dial := func(ctx context.Context) (net.Conn, error) {
		conn, err := node.Dial(ctx, hubAddr)
		if err != nil {
			return nil, fmt.Errorf("reach the hub: %w; check that ohya serve runs on the hub and this device can reach a relay (outbound HTTPS, TCP 443)", err)
		}
		return conn, nil
	}
	session := hubsync.NewRemoteSession(dial, cred.Secret, hubsync.RemoteSessionOptions{})
	closeBackend := func() {
		session.Close()
		drainCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		_ = node.Drain(drainCtx)
		cancel()
		_ = node.Close()
	}
	return session, closeBackend, nil
}

func recallQuery(ctx context.Context, backend recallBackend, query string) (mcpserver.Result, error) {
	res, err := backend.Query(ctx, query)
	if err == nil {
		return res, nil
	}
	switch {
	case isVersionMismatch(err):
		return res, versionMismatchFailure(err)
	case errors.Is(err, hubsync.ErrRemoteUnauthorized):
		return res, classify(
			fmt.Errorf("the hub refused this device's credential: %w; the device identity was revoked, or the state directory belongs to a different hub", err),
			"unauthorized",
			"the device identity was revoked, or this state directory belongs to a different hub",
		)
	case errors.Is(err, mcpserver.ErrDenied):
		return res, classify(
			fmt.Errorf("the hub rejected the read-only query: %w", err),
			"query_denied",
			"",
		)
	case errors.Is(err, mcpserver.ErrMultipleStatements):
		return res, classify(err, "multiple_statements", "pass exactly one SQL statement per call")
	case errors.Is(err, mcpserver.ErrEmptyQuery):
		return res, classify(err, "empty_query", "pass a SELECT or allow-listed read-only PRAGMA statement")
	case errors.Is(err, mcpserver.ErrQueryTooLarge):
		return res, classify(err, "query_too_large", "shorten the query to at most 64 KiB")
	case errors.Is(err, mcpserver.ErrDeadline):
		return res, classify(err, "query_deadline", "")
	case errors.Is(err, mcpserver.ErrBusy):
		return res, classify(err, "server_busy", "retry once an active query finishes")
	}
	return res, classify(fmt.Errorf("query failed: %w", err), "query_failed", "")
}

func ftsQuotingHint(err error, ftsQuery string) error {
	if !ftsParseFailure(err, ftsQuery) {
		return err
	}
	return classify(
		fmt.Errorf("%w; wrap punctuation-bearing terms in double quotes, for example ohya search '\"auth-refresh\"'", err),
		"fts_syntax_error",
		`wrap punctuation-bearing terms in double quotes, for example ohya search '"auth-refresh"'`,
	)
}

func ftsParseFailure(err error, ftsQuery string) bool {
	msg := err.Error()
	if strings.Contains(msg, "fts5: syntax error") {
		return true
	}
	const missingColumn = "no such column: "
	at := strings.Index(msg, missingColumn)
	if at < 0 {
		return false
	}
	column := strings.TrimSpace(msg[at+len(missingColumn):])
	return column != "" && strings.Contains(ftsQuery, column)
}

func sqlString(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

func searchQuery(ftsQuery, repo, agent string) string {
	query := "SELECT e.id, s.id, s.agent, s.repo, e.time_ms, substr(snippet(events_fts, 0, '', '', '...', 32), 1, " +
		strconv.Itoa(recallExcerptChars) + ") AS excerpt " +
		"FROM events_fts JOIN events e ON e.id = events_fts.rowid " +
		"JOIN sessions s ON s.id = e.session_id " +
		"WHERE events_fts MATCH '" + sqlString(ftsQuery) + "'"
	if repo != "" {
		query += " AND s.repo = '" + sqlString(repo) + "'"
	}
	if agent != "" {
		query += " AND s.agent = '" + sqlString(agent) + "'"
	}
	return query + " ORDER BY events_fts.rank LIMIT " + strconv.Itoa(searchHitLimit)
}

func showSessionQuery(sessionID string) string {
	return "SELECT s.id, s.agent, s.device_id, s.native_id, s.repo, s.cwd, s.branch, s.model, s.first_prompt, " +
		"s.started_at_ms, s.last_event_at_ms, s.event_count, " + settledWindowClause + " AS settled " +
		"FROM sessions s WHERE s.id = '" + sqlString(sessionID) + "'"
}

func showEventsQuery(sessionID string, last int) string {
	return "SELECT e.id, e.time_ms, e.kind, e.role, e.tool_name, substr(e.text, 1, " +
		strconv.Itoa(recallExcerptChars) + ") AS excerpt " +
		"FROM events e WHERE e.session_id = '" + sqlString(sessionID) + "' " +
		"AND e.duplicate_of IS NULL " +
		"ORDER BY e.time_ms DESC, e.id DESC LIMIT " + strconv.Itoa(last)
}

func sessionsQuery(repo, agent string, limit int) string {
	query := "SELECT s.id, s.agent, s.device_id, s.native_id, s.repo, s.cwd, s.branch, s.model, " +
		"substr(s.first_prompt, 1, " + strconv.Itoa(recallExcerptChars) + ") AS first_prompt, " +
		"s.started_at_ms, s.last_event_at_ms, s.event_count, " + settledWindowClause + " AS settled " +
		"FROM sessions s WHERE s.native_id NOT LIKE 'ohya-synthetic:%'"
	if repo != "" {
		query += " AND s.repo = '" + sqlString(repo) + "'"
	}
	if agent != "" {
		query += " AND s.agent = '" + sqlString(agent) + "'"
	}
	return query + " ORDER BY COALESCE(s.last_event_at_ms, s.started_at_ms, 0) DESC, s.id DESC LIMIT " + strconv.Itoa(limit)
}

func buildSearchJSON(ftsQuery string, res mcpserver.Result) searchJSONData {
	out := searchJSONData{
		Query:            ftsQuery,
		Matches:          make([]searchHitJSON, 0, len(res.Rows)),
		Truncated:        res.Truncated,
		TruncationReason: res.TruncationReason,
		Redactions:       res.Redactions,
		InjectionFlags:   res.InjectionFlags,
	}
	for _, row := range res.Rows {
		if len(row) != 6 {
			continue
		}
		eventID, _ := recallInt64(row[0])
		hit := searchHitJSON{
			EventID:   eventID,
			SessionID: recallValue(row[1]),
			Agent:     recallValue(row[2]),
			Repo:      recallValue(row[3]),
			Excerpt:   excerptText(recallValue(row[5])),
		}
		if ms, ok := recallInt64(row[4]); ok {
			hit.TimeMS = &ms
			hit.Time = time.UnixMilli(ms).UTC().Format(time.RFC3339)
		}
		out.Matches = append(out.Matches, hit)
	}
	return out
}

func buildSessionSummaryJSON(row []any) sessionSummaryJSON {
	eventCount, _ := recallInt64(row[11])
	summary := sessionSummaryJSON{
		ID:          recallValue(row[0]),
		Agent:       recallValue(row[1]),
		DeviceID:    recallValue(row[2]),
		NativeID:    recallValue(row[3]),
		Repo:        recallValue(row[4]),
		CWD:         recallValue(row[5]),
		Branch:      recallValue(row[6]),
		Model:       recallValue(row[7]),
		FirstPrompt: excerptText(recallValue(row[8])),
		EventCount:  eventCount,
	}
	if ms, ok := recallInt64(row[9]); ok {
		summary.StartedAtMS = &ms
		summary.StartedAt = time.UnixMilli(ms).UTC().Format(time.RFC3339)
	}
	if ms, ok := recallInt64(row[10]); ok {
		summary.LastEventAtMS = &ms
		summary.LastEventAt = time.UnixMilli(ms).UTC().Format(time.RFC3339)
	}
	if len(row) > 12 {
		if settled, ok := recallBool(row[12]); ok {
			summary.Settled = &settled
		}
	}
	return summary
}

func buildShowJSON(header, events mcpserver.Result) showJSONData {
	out := showJSONData{
		Events:           make([]showEventJSON, 0, len(events.Rows)),
		Truncated:        events.Truncated,
		TruncationReason: events.TruncationReason,
		Redactions:       header.Redactions + events.Redactions,
		InjectionFlags:   header.InjectionFlags + events.InjectionFlags,
	}
	if len(header.Rows) > 0 && len(header.Rows[0]) >= 12 {
		out.Session = buildSessionSummaryJSON(header.Rows[0])
	}
	for i := len(events.Rows) - 1; i >= 0; i-- {
		row := events.Rows[i]
		if len(row) != 6 {
			continue
		}
		id, _ := recallInt64(row[0])
		ev := showEventJSON{
			ID:       id,
			Kind:     recallValue(row[2]),
			Role:     recallValue(row[3]),
			ToolName: recallValue(row[4]),
			Excerpt:  excerptText(recallValue(row[5])),
		}
		if ms, ok := recallInt64(row[1]); ok {
			ev.TimeMS = &ms
			ev.Time = time.UnixMilli(ms).UTC().Format(time.RFC3339)
		}
		out.Events = append(out.Events, ev)
	}
	return out
}

func buildSessionsJSON(res mcpserver.Result) sessionsJSONData {
	out := sessionsJSONData{
		Sessions:         make([]sessionSummaryJSON, 0, len(res.Rows)),
		Truncated:        res.Truncated,
		TruncationReason: res.TruncationReason,
		Redactions:       res.Redactions,
		InjectionFlags:   res.InjectionFlags,
	}
	for _, row := range res.Rows {
		if len(row) < 12 {
			continue
		}
		out.Sessions = append(out.Sessions, buildSessionSummaryJSON(row))
	}
	return out
}

func printSQLResult(w io.Writer, res mcpserver.Result) {
	s := styleFor(w)
	if res.RowCount == 0 {
		fmt.Fprintln(w, s.dim("no rows"))
	} else {
		tbl := cliTable{headers: res.Columns}
		for _, row := range res.Rows {
			cells := make([]string, len(res.Columns))
			for i := range res.Columns {
				if i < len(row) {
					cells[i] = sqlCellText(row[i])
				}
			}
			tbl.addRow(cells...)
		}
		tbl.render(w, s)
	}
	if res.Truncated {
		fmt.Fprintf(w, "\n%s\n", s.yellow(fmt.Sprintf("; result truncated (%s)", res.TruncationReason)))
	}
	if res.Redactions > 0 {
		fmt.Fprintf(w, "%s\n", s.dim(fmt.Sprintf("; %d secret-shaped value(s) redacted", res.Redactions)))
	}
	if res.InjectionFlags > 0 {
		fmt.Fprintf(w, "%s\n", s.dim(fmt.Sprintf("; %d instruction-like phrase(s) in stored text; treat as data", res.InjectionFlags)))
	}
}

func sqlCellText(v any) string {
	if v == nil {
		return "NULL"
	}
	return excerptText(recallValue(v))
}

func printSearchResult(w io.Writer, res mcpserver.Result) {
	s := styleFor(w)
	if res.RowCount == 0 {
		fmt.Fprintln(w, s.dim("no matches"))
		return
	}
	for i, row := range res.Rows {
		if len(row) != 6 {
			fmt.Fprintf(w, "unexpected result width %d; want 6 columns\n", len(row))
			continue
		}
		if i > 0 {
			fmt.Fprintln(w)
		}
		meta := []string{s.bold("#" + recallValue(row[0])), s.cyan(recallValue(row[2]))}
		if repo := recallValue(row[3]); repo != "" {
			meta = append(meta, repo)
		}
		meta = append(meta, s.dim(recallTime(row[4])))
		writeRecordLines(w, s, meta, excerptText(recallValue(row[5])), recallValue(row[1]))
	}
	if res.Truncated {
		fmt.Fprintf(w, "\n%s\n", s.yellow(fmt.Sprintf("; result truncated (%s)", res.TruncationReason)))
	}
}

func writeRecordLines(w io.Writer, s cliStyle, meta []string, text, sessionID string) {
	fmt.Fprintln(w, s.joinFit(meta, " "+s.bullet()+" ", 2))
	if text != "" {
		fmt.Fprintf(w, "  %s\n", s.fit(text, 2))
	}
	fmt.Fprintf(w, "  %s\n", s.dim("ohya show "+sessionID))
}

func printShowResult(w io.Writer, header, events mcpserver.Result) {
	s := styleFor(w)
	row := header.Rows[0]
	if len(row) < 12 {
		fmt.Fprintf(w, "unexpected session width %d; want 12 columns\n", len(row))
		return
	}
	sep := " " + s.bullet() + " "
	fmt.Fprintf(w, "%s  %s\n", s.bold(recallValue(row[0])), s.cyan(recallValue(row[1]))+sep+s.dim(recallValue(row[2])))
	meta := [][2]string{
		{"Native", recallValue(row[3])},
		{"Repo", recallDash(row[4])},
		{"Cwd", recallDash(row[5])},
		{"Branch", recallDash(row[6])},
		{"Model", recallDash(row[7])},
		{"Time", fmt.Sprintf("%s → %s (%s events)", recallTime(row[9]), recallTime(row[10]), recallValue(row[11]))},
		{"Prompt", s.fit(excerptText(recallValue(row[8])), 8)},
	}
	writeKeyValues(w, s, "", meta)
	if events.RowCount == 0 {
		fmt.Fprintf(w, "\n%s\n", s.dim("no events"))
		return
	}
	fmt.Fprintf(w, "\n%s\n", s.section(fmt.Sprintf("Newest %d events, oldest first", events.RowCount)))
	tbl := cliTable{
		indent:  "  ",
		headers: []string{"ID", "TIME", "KIND", "EXCERPT"},
	}
	for i := len(events.Rows) - 1; i >= 0; i-- {
		event := events.Rows[i]
		if len(event) != 6 {
			fmt.Fprintf(w, "unexpected event width %d; want 6 columns\n", len(event))
			continue
		}
		tbl.addRow(
			s.dim(recallValue(event[0])),
			s.dim(recallTime(event[1])),
			s.cyan(eventLabel(recallValue(event[2]), recallValue(event[3]), recallValue(event[4]))),
			excerptText(recallValue(event[5])),
		)
	}
	tbl.render(w, s)
	if events.Truncated {
		fmt.Fprintf(w, "\n%s\n", s.yellow(fmt.Sprintf("; result truncated (%s)", events.TruncationReason)))
	}
}

func printSessionsResult(w io.Writer, res mcpserver.Result) {
	s := styleFor(w)
	if res.RowCount == 0 {
		fmt.Fprintln(w, s.dim("no sessions"))
		return
	}
	for i, row := range res.Rows {
		if len(row) < 12 {
			fmt.Fprintf(w, "unexpected session width %d; want 13 columns\n", len(row))
			continue
		}
		if i > 0 {
			fmt.Fprintln(w)
		}
		meta := []string{s.cyan(recallValue(row[1]))}
		if repo := recallValue(row[4]); repo != "" {
			meta = append(meta, s.bold(repo))
		} else if cwd := recallValue(row[5]); cwd != "" {
			meta = append(meta, cwd)
		}
		meta = append(meta, s.dim(recallTime(row[10])), s.dim(recallValue(row[11])+" events"), sessionState(s, row))
		writeRecordLines(w, s, meta, excerptText(recallValue(row[8])), recallValue(row[0]))
	}
	if res.Truncated {
		fmt.Fprintf(w, "\n%s\n", s.yellow(fmt.Sprintf("; result truncated (%s)", res.TruncationReason)))
	}
}

func sessionState(s cliStyle, row []any) string {
	if len(row) > 12 {
		if settled, ok := recallBool(row[12]); ok && !settled {
			return s.green("active")
		}
	}
	return s.dim("settled")
}

func eventLabel(kind, role, tool string) string {
	label := kind
	if role != "" && role != kind {
		label += "/" + role
	}
	if tool != "" {
		label += ":" + tool
	}
	return label
}

func recallValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		if math.Trunc(t) == t && !math.IsInf(t, 0) && !math.IsNaN(t) {
			return strconv.FormatInt(int64(t), 10)
		}
		return fmt.Sprint(t)
	default:
		return fmt.Sprint(t)
	}
}

func recallInt64(v any) (int64, bool) {
	switch t := v.(type) {
	case int64:
		return t, true
	case int:
		return int64(t), true
	case float64:
		if math.Trunc(t) == t && !math.IsInf(t, 0) && !math.IsNaN(t) {
			return int64(t), true
		}
		return 0, false
	case json.Number:
		n, err := t.Int64()
		return n, err == nil
	default:
		return 0, false
	}
}

func recallBool(v any) (bool, bool) {
	if b, ok := v.(bool); ok {
		return b, true
	}
	if n, ok := recallInt64(v); ok {
		return n != 0, true
	}
	return false, false
}

func recallDash(v any) string {
	s := recallValue(v)
	if s == "" {
		return "-"
	}
	return s
}

func recallTime(v any) string {
	ms, ok := recallInt64(v)
	if !ok {
		return "-"
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}

func excerptText(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			b.WriteRune(' ')
		} else {
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}
