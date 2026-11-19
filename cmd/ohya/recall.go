package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
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
	searchHitLimit     = 20
	recallExcerptChars = 200
	showDefaultLast    = 20
)

var searchSpec = commandSpec{
	name:      "ohya search",
	synopsis:  "find sessions by keyword in the stored events",
	usageLine: "ohya search <query> [flags]",
	description: "Runs one bounded read-only FTS5 query with BM25 ranking over stored events and prints the best " +
		strconv.Itoa(searchHitLimit) + " matches, each with its event id, session id, agent, repo, time, and a text excerpt. " +
		"The query is SQLite FTS5 syntax, for example 'auth AND refresh' or '\"exact phrase\"'; an apostrophe must sit inside a double-quoted phrase. " +
		"Without --remote the command reads the local hub database and never writes or migrates it; with --remote it queries the joined hub through the saved pairing credential. " +
		"Bounds and the read-only policy are the sql tool's engine: at most 500 rows and 256 KiB per call, 5 seconds per query.",
	argsTitle: "Arguments",
	args:      "<query>  FTS5 query text, for example 'auth AND refresh'",
	examples: []string{
		`ohya search "auth AND refresh"`,
		`ohya search panic --repo github.com/owner/name --agent claude-code`,
		`ohya search --remote "cursor regression"`,
	},
}

var showSpec = commandSpec{
	name:      "ohya show",
	synopsis:  "print one session's newest events",
	usageLine: "ohya show <session-id> [flags]",
	description: "Prints one session's header — agent, device, native id, repo, working directory, branch, model, first prompt, timing, event count — followed by its newest events, excerpted, oldest first. " +
		"Find the session id with ohya search. Byte-identical replay duplicates stay hidden. " +
		"Without --remote the command reads the local hub database and never writes or migrates it; with --remote it reads the joined hub through the saved pairing credential.",
	argsTitle: "Arguments",
	args:      "<session-id>  session id as printed by ohya search",
	examples: []string{
		"ohya show claude-code:dev1:sess-1",
		"ohya show --last 50 claude-code:dev1:sess-1",
		"ohya show --remote claude-code:dev1:sess-1",
	},
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
	if err := c.parse(args, stdout); err != nil {
		return err
	}
	if c.fs.NArg() != 1 {
		return c.usageErrorf("search needs exactly one FTS query argument")
	}
	target, err := resolveRecallTarget(home, *state, *db, *remote, dbFlagExplicit(c))
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	backend, closeBackend, err := openRecallBackend(target)
	if err != nil {
		return err
	}
	defer closeBackend()
	res, err := recallQuery(ctx, backend, searchQuery(c.fs.Arg(0), *repo, *agent))
	if err != nil {
		return err
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
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	backend, closeBackend, err := openRecallBackend(target)
	if err != nil {
		return err
	}
	defer closeBackend()
	sessionID := c.fs.Arg(0)
	header, err := recallQuery(ctx, backend, showSessionQuery(sessionID))
	if err != nil {
		return err
	}
	if header.RowCount != 1 {
		return fmt.Errorf("no session with id %q; find session ids with ohya search", sessionID)
	}
	events, err := recallQuery(ctx, backend, showEventsQuery(sessionID, *last))
	if err != nil {
		return err
	}
	printShowResult(stdout, header, events)
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
			return nil, nil, fmt.Errorf("hub database %s does not exist; run ohya ingest --once to create it, or pass --remote to query the joined hub", target.dbPath)
		}
		return nil, nil, fmt.Errorf("stat %s: %w", target.dbPath, err)
	}
	hub, err := store.OpenReadOnly(target.dbPath)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s read-only: %w; check the --db path and its permissions; another ohya process may hold the database", target.dbPath, err)
	}
	reader, err := hub.Reader()
	if err != nil {
		hub.Close()
		return nil, nil, err
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
		return nil, nil, fmt.Errorf("this state directory holds no pairing credential; run ohya join first")
	}
	if err != nil {
		return nil, nil, err
	}
	hubAddr, err := device.LoadHubAddress(stateDir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, fmt.Errorf("this state directory holds no hub address; run ohya join with an ohya1 code once to save it")
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
		return res, fmt.Errorf("the hub refused this device's credential: %w; the device identity was revoked, or the state directory belongs to a different hub", err)
	case errors.Is(err, mcpserver.ErrDenied):
		return res, fmt.Errorf("the hub rejected the read-only query: %w", err)
	}
	return res, fmt.Errorf("query failed: %w", err)
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
	return "SELECT id, agent, device_id, native_id, repo, cwd, branch, model, first_prompt, " +
		"started_at_ms, last_event_at_ms, event_count FROM sessions WHERE id = '" + sqlString(sessionID) + "'"
}

func showEventsQuery(sessionID string, last int) string {
	return "SELECT e.id, e.time_ms, e.kind, e.role, e.tool_name, substr(e.text, 1, " +
		strconv.Itoa(recallExcerptChars) + ") AS excerpt " +
		"FROM events e WHERE e.session_id = '" + sqlString(sessionID) + "' " +
		"AND e.duplicate_of IS NULL " +
		"ORDER BY e.time_ms DESC, e.id DESC LIMIT " + strconv.Itoa(last)
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
	if len(row) != 12 {
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
	default:
		return fmt.Sprint(t)
	}
}

func recallDash(v any) string {
	s := recallValue(v)
	if s == "" {
		return "-"
	}
	return s
}

func recallTime(v any) string {
	ms, ok := v.(int64)
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
