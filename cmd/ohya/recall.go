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
	argsTitle: "Arguments",
	args:      "<query>  FTS5 query text, for example 'auth AND refresh'",
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
	argsTitle: "Arguments",
	args:      "<session-id>  session id as printed by ohya search or ohya sessions",
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
	argsTitle: "Arguments",
	args:      "(none)",
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
}

type sessionsJSONData struct {
	Sessions         []sessionSummaryJSON `json:"sessions"`
	Truncated        bool                 `json:"truncated"`
	TruncationReason string               `json:"truncation_reason,omitempty"`
	Redactions       int                  `json:"redactions,omitempty"`
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

func openRecallBackend(target recallTarget) (recallBackend, func(), error) {
	if target.remote {
		return openRemoteRecallBackend(target.stateDir)
	}
	if _, err := os.Stat(target.dbPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, classify(
				fmt.Errorf("hub database %s does not exist; run ohya ingest --once to create it, or pass --remote to query the joined hub", target.dbPath),
				"database_open_failed",
				"run ohya ingest --once to create the hub database, or pass --remote to query a joined hub",
			)
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
	query := "SELECT e.id, s.id, s.agent, s.repo, e.time_ms, substr(e.text, 1, " +
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
	}
	for _, row := range res.Rows {
		if len(row) < 12 {
			continue
		}
		out.Sessions = append(out.Sessions, buildSessionSummaryJSON(row))
	}
	return out
}

func printSearchResult(w io.Writer, res mcpserver.Result) {
	if res.RowCount == 0 {
		fmt.Fprintln(w, "no matches")
		return
	}
	for _, row := range res.Rows {
		if len(row) != 6 {
			fmt.Fprintf(w, "unexpected result width %d; want 6 columns\n", len(row))
			continue
		}
		fmt.Fprintf(w, "%s  session=%s  agent=%s  repo=%s  time=%s\n",
			recallValue(row[0]), recallValue(row[1]), recallValue(row[2]),
			recallDash(row[3]), recallTime(row[4]))
		fmt.Fprintf(w, "    %s\n", excerptText(recallValue(row[5])))
	}
	if res.Truncated {
		fmt.Fprintf(w, "; result truncated (%s)\n", res.TruncationReason)
	}
}

func printShowResult(w io.Writer, header, events mcpserver.Result) {
	row := header.Rows[0]
	if len(row) < 12 {
		fmt.Fprintf(w, "unexpected session width %d; want 12 columns\n", len(row))
		return
	}
	fmt.Fprintf(w, "session=%s  agent=%s  device=%s  native=%s\n",
		recallValue(row[0]), recallValue(row[1]), recallValue(row[2]), recallValue(row[3]))
	fmt.Fprintf(w, "repo=%s  cwd=%s  branch=%s  model=%s\n",
		recallDash(row[4]), recallDash(row[5]), recallDash(row[6]), recallDash(row[7]))
	fmt.Fprintf(w, "first prompt: %s\n", excerptText(recallValue(row[8])))
	fmt.Fprintf(w, "started=%s  last=%s  events=%s\n",
		recallTime(row[9]), recallTime(row[10]), recallValue(row[11]))
	if events.RowCount == 0 {
		fmt.Fprintln(w, "no events")
		return
	}
	fmt.Fprintf(w, "newest %d events (oldest first):\n", events.RowCount)
	for i := len(events.Rows) - 1; i >= 0; i-- {
		event := events.Rows[i]
		if len(event) != 6 {
			fmt.Fprintf(w, "unexpected event width %d; want 6 columns\n", len(event))
			continue
		}
		line := fmt.Sprintf("%s  %s  %s  %s",
			recallValue(event[0]), recallTime(event[1]),
			eventLabel(recallValue(event[2]), recallValue(event[3]), recallValue(event[4])),
			excerptText(recallValue(event[5])))
		fmt.Fprintln(w, strings.TrimRight(line, " "))
	}
	if events.Truncated {
		fmt.Fprintf(w, "; result truncated (%s)\n", events.TruncationReason)
	}
}

func printSessionsResult(w io.Writer, res mcpserver.Result) {
	if res.RowCount == 0 {
		fmt.Fprintln(w, "no sessions")
		return
	}
	for _, row := range res.Rows {
		if len(row) < 12 {
			fmt.Fprintf(w, "unexpected session width %d; want 13 columns\n", len(row))
			continue
		}
		state := "settled"
		if len(row) > 12 {
			if settled, ok := recallBool(row[12]); ok && !settled {
				state = "active"
			}
		}
		fmt.Fprintf(w, "%s  agent=%s  device=%s  repo=%s  last=%s  events=%s  state=%s\n",
			recallValue(row[0]), recallValue(row[1]), recallValue(row[2]),
			recallDash(row[4]), recallTime(row[10]), recallValue(row[11]), state)
		if prompt := excerptText(recallValue(row[8])); prompt != "" {
			fmt.Fprintf(w, "    %s\n", prompt)
		}
	}
	if res.Truncated {
		fmt.Fprintf(w, "; result truncated (%s)\n", res.TruncationReason)
	}
}

func eventLabel(kind, role, tool string) string {
	label := kind
	if role != "" {
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
