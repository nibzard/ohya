package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/nibzard/ohya/internal/secrets"
	"github.com/nibzard/ohya/internal/store"
)

const secretsScanPageSize = 500

var secretsSpec = commandSpec{
	name:      "ohya secrets",
	synopsis:  "report secret-shaped values found in stored records",
	usageLine: "ohya secrets <command> [flags]",
	description: "The operator's visibility surface for secret-shaped values in the hub database. " +
		"The sql tool replaces such values with kind-labeled markers before results leave the hub; " +
		"these commands report what the same fixed pattern table finds in the stored records without ever printing matched text.",
	argsTitle: "Commands",
	args:      `scan  count pattern matches per event, session, and pattern`,
	examples:  []string{"ohya secrets scan", "ohya secrets scan --json"},
}

func runSecretsCommand(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || isHelpArg(args[0]) {
		writeUsage(stdout, secretsSpec, nil)
		return nil
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "scan":
		return runSecretsScanCommand(rest, stdout, stderr)
	default:
		return usageError{cmd: "ohya secrets", msg: fmt.Sprintf("unknown secrets subcommand %q", cmd)}
	}
}

var secretsScanSpec = commandSpec{
	name:      "ohya secrets scan",
	synopsis:  "count secret-shaped values in stored records",
	usageLine: "ohya secrets scan [flags]",
	description: "Scans the original record of every stored event (raw_json, including replay duplicates) with the fixed pattern table the sql tool redacts with, and prints match counts per pattern, session, and event. " +
		"It never prints matched text, field values, or excerpts; output holds counts, pattern ids, event ids, and session ids only. " +
		"The scan is read-only and best effort: a fixed table without a classifier finds only the shapes it knows, so a clean report is not proof that no secret is stored. " +
		"Removing stored secrets stays a separate owner action, for example ohya purge.",
	examples: []string{"ohya secrets scan", "ohya secrets scan --db /path/to/hub.db --json"},
}

func runSecretsScanCommand(args []string, stdout, stderr io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	c := newCommandFlags(secretsScanSpec)
	db := c.fs.String("db", filepath.Join(home, ".ohya", "hub.db"),
		"hub database `path`; run the scan on the hub that holds the records")
	asJSON := c.fs.Bool("json", false, "print one JSON object instead of text (docs/adr/0005)")
	if err := c.parse(args, stdout); err != nil {
		return err
	}
	if c.fs.NArg() > 0 {
		return c.usageErrorf("unexpected argument %q", c.fs.Arg(0))
	}
	if _, err := os.Stat(*db); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("hub database %s does not exist; run the scan on the hub that holds the records, or pass --db", *db)
		}
		return fmt.Errorf("stat %s: %w", *db, err)
	}
	hub, err := store.OpenReadOnly(*db)
	if err != nil {
		return jsonFailurePrint(*asJSON, stdout, secretsScanSpec.name,
			fmt.Errorf("open %s read-only: %w; check the --db path and its permissions; another ohya process may hold the database", *db, err))
	}
	defer hub.Close()
	reader, err := hub.Reader()
	if err != nil {
		return err
	}
	defer reader.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	report, err := scanSecrets(ctx, reader)
	if err != nil {
		return jsonFailurePrint(*asJSON, stdout, secretsScanSpec.name, err)
	}
	if *asJSON {
		return printJSON(stdout, secretsScanSpec.name, newSecretsScanJSON(report))
	}
	printSecretsScan(stdout, report)
	return nil
}

type secretsEvent struct {
	id      int64
	matches int
	ids     []string
}

type secretsSession struct {
	id     string
	events []secretsEvent
}

type secretsReport struct {
	sessions []*secretsSession
	byID     map[string]*secretsSession
	patterns map[string]int
	matches  int
	events   int
}

func scanSecrets(ctx context.Context, reader *sql.DB) (*secretsReport, error) {
	report := &secretsReport{byID: map[string]*secretsSession{}, patterns: map[string]int{}}
	order := secrets.IDs()
	lastID := int64(0)
	for {
		rows, err := reader.QueryContext(ctx,
			`SELECT id, session_id, raw_json FROM events WHERE id > ? ORDER BY id LIMIT ?`,
			lastID, secretsScanPageSize)
		if err != nil {
			return nil, fmt.Errorf("scan events: %w", err)
		}
		read := 0
		for rows.Next() {
			var id int64
			var sessionID, raw string
			if err := rows.Scan(&id, &sessionID, &raw); err != nil {
				rows.Close()
				return nil, fmt.Errorf("read event row: %w", err)
			}
			read++
			lastID = id
			counts := secrets.Count(raw)
			if len(counts) == 0 {
				continue
			}
			event := secretsEvent{id: id, ids: patternIDList(order, counts)}
			for _, pid := range event.ids {
				event.matches += counts[pid]
				report.patterns[pid] += counts[pid]
			}
			session := report.byID[sessionID]
			if session == nil {
				session = &secretsSession{id: sessionID}
				report.byID[sessionID] = session
				report.sessions = append(report.sessions, session)
			}
			session.events = append(session.events, event)
			report.matches += event.matches
			report.events++
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan events: %w", err)
		}
		rows.Close()
		if read < secretsScanPageSize {
			return report, nil
		}
	}
}

func patternIDList(order []string, counts map[string]int) []string {
	var ids []string
	for _, id := range order {
		if counts[id] > 0 {
			ids = append(ids, id)
		}
	}
	return ids
}

func sessionMatches(session *secretsSession) int {
	total := 0
	for _, event := range session.events {
		total += event.matches
	}
	return total
}

func sessionIDs(session *secretsSession) []string {
	seen := map[string]bool{}
	var ids []string
	for _, event := range session.events {
		for _, id := range event.ids {
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	return ids
}

func printSecretsScan(w io.Writer, report *secretsReport) {
	s := styleFor(w)
	summary := fmt.Sprintf("%d matches in %d events across %d sessions",
		report.matches, report.events, len(report.sessions))
	if report.matches == 0 {
		fmt.Fprintln(w, s.boldGreen(summary))
		fmt.Fprintln(w, s.dim("the fixed pattern table found nothing; best effort, not proof that no secret is stored"))
		return
	}
	fmt.Fprintln(w, s.boldYellow(summary))
	fmt.Fprintf(w, "\n%s\n", s.section("patterns:"))
	tbl := cliTable{indent: "  ", rightAlign: []bool{false, true}}
	for _, id := range secrets.IDs() {
		if report.patterns[id] > 0 {
			tbl.addRow(s.cyan(id), s.bold(fmt.Sprintf("%d", report.patterns[id])))
		}
	}
	tbl.render(w, s)
	fmt.Fprintln(w)
	for _, session := range report.sessions {
		fmt.Fprintf(w, "%s %s  %s\n",
			s.dim("session"), s.bold(session.id),
			s.dim(fmt.Sprintf("matches=%d  events=%d  patterns=%s",
				sessionMatches(session), len(session.events), joinIDs(sessionIDs(session)))))
		for _, event := range session.events {
			fmt.Fprintf(w, "  event %d  matches=%d  patterns=%s\n",
				event.id, event.matches, s.yellow(joinIDs(event.ids)))
		}
	}
	fmt.Fprintf(w, "\n%s\n", s.dim("best effort: a fixed pattern table, no classifier; matched text is never printed; removing stored secrets is a separate owner action"))
}

func joinIDs(ids []string) string {
	out := ""
	for i, id := range ids {
		if i > 0 {
			out += ","
		}
		out += id
	}
	return out
}

type secretsScanJSONPattern struct {
	ID      string `json:"id"`
	Matches int    `json:"matches"`
}

type secretsScanJSONEvent struct {
	ID       int64    `json:"id"`
	Matches  int      `json:"matches"`
	Patterns []string `json:"patterns"`
}

type secretsScanJSONSession struct {
	ID       string                 `json:"id"`
	Matches  int                    `json:"matches"`
	Patterns []string               `json:"patterns"`
	Events   []secretsScanJSONEvent `json:"events"`
}

type secretsScanJSONData struct {
	Patterns []secretsScanJSONPattern `json:"patterns"`
	Sessions []secretsScanJSONSession `json:"sessions"`
	Events   int                      `json:"events"`
	Matches  int                      `json:"matches"`
}

func newSecretsScanJSON(report *secretsReport) secretsScanJSONData {
	data := secretsScanJSONData{
		Patterns: []secretsScanJSONPattern{},
		Sessions: []secretsScanJSONSession{},
		Events:   report.events,
		Matches:  report.matches,
	}
	for _, id := range secrets.IDs() {
		if report.patterns[id] > 0 {
			data.Patterns = append(data.Patterns, secretsScanJSONPattern{ID: id, Matches: report.patterns[id]})
		}
	}
	for _, session := range report.sessions {
		js := secretsScanJSONSession{
			ID: session.id, Matches: sessionMatches(session), Patterns: sessionIDs(session),
			Events: make([]secretsScanJSONEvent, 0, len(session.events)),
		}
		for _, event := range session.events {
			js.Events = append(js.Events, secretsScanJSONEvent{
				ID: event.id, Matches: event.matches, Patterns: event.ids,
			})
		}
		data.Sessions = append(data.Sessions, js)
	}
	return data
}
