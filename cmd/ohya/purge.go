package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/nibzard/ohya/internal/store"
)

var purgeSpec = commandSpec{
	name:      "ohya purge",
	synopsis:  "plan or apply an owner-decided purge of stored sessions",
	usageLine: "ohya purge [flags]",
	description: "Deletes whole sessions and every source that holds their events, then tombstones each evicted source so its device is refused until the owner re-admits it. " +
		"Retention is owner-decided: nothing is purged automatically, and purged bytes never return on their own. " +
		"The command is a dry run by default and prints the affected sessions, sources, event counts, and bytes; pass --apply to delete. " +
		"Each source is purged in its own transaction, so an interrupted run resumes when rerun with the same selectors. " +
		"Agents hold no destructive tool: they audit with the read-only sql tool and mark garbage with annotate (namespace tags, key garbage), and the owner executes the purge here. " +
		"--marked namespace.key selects the sessions whose events carry that annotation mark; see the garbage audit in DB.md. " +
		"Freed pages stay in the database file (no VACUUM), and a recovery copy taken before a purge still holds the purged data.",
	argsTitle: "Arguments",
	args:      "(none)",
	examples: []string{
		"ohya purge --repo github.com/owner/name --before 2026-11-01T00:00:00Z",
		"ohya purge --repo github.com/owner/name --before 2026-11-01T00:00:00Z --apply",
		"ohya purge --session 06055d1b-75a0-42a9-88fc-d3cbcb222b52 --apply",
		"ohya purge --marked tags.garbage",
		"ohya purge --marked tags.garbage --apply",
	},
}

func runPurgeCommand(args []string, stdout, stderr io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	c := newCommandFlags(purgeSpec)
	db := c.fs.String("db", filepath.Join(home, ".ohya", "hub.db"),
		"hub database `path`")
	session := c.fs.String("session", "",
		"purge sessions with this `native-id`")
	repo := c.fs.String("repo", "",
		"purge sessions of this `repo` (host/path, as stored in sessions.repo)")
	agent := c.fs.String("agent", "",
		"purge sessions of this `agent` (for example claude-code)")
	before := c.fs.String("before", "",
		"purge sessions that ended before this RFC3339 `date`")
	marked := c.fs.String("marked", "",
		"purge sessions whose events carry this annotation `namespace.key` (for example tags.garbage)")
	apply := c.fs.Bool("apply", false,
		"delete the planned rows; without it the command only prints the plan")
	asJSON := c.fs.Bool("json", false, "print one JSON object instead of text (docs/adr/0005)")
	if err := c.parse(args, stdout); err != nil {
		return err
	}
	if c.fs.NArg() > 0 {
		return c.usageErrorf("unexpected argument %q", c.fs.Arg(0))
	}
	var beforeMS int64
	if *before != "" {
		t, err := time.Parse(time.RFC3339, *before)
		if err != nil {
			return c.usageErrorf("--before must be an RFC3339 date, for example 2026-11-01T00:00:00Z")
		}
		beforeMS = t.UnixMilli()
	}
	var markedNS, markedKey string
	if *marked != "" {
		ns, key, err := store.ParseMarkSelector(*marked)
		if err != nil {
			return c.usageErrorf("--%s", err.Error())
		}
		markedNS, markedKey = ns, key
	}
	sel := store.PurgeSelector{
		Session:         *session,
		Repo:            *repo,
		Agent:           *agent,
		HasBefore:       *before != "",
		BeforeMS:        beforeMS,
		MarkedNamespace: markedNS,
		MarkedKey:       markedKey,
	}
	if sel.Empty() {
		return c.usageErrorf("purge needs at least one selector: --session, --repo, --agent, --before, or --marked")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	hub, err := store.Open(ctx, *db)
	if err != nil {
		return jsonFailurePrint(*asJSON, stdout, purgeSpec.name,
			classify(err, "database_open_failed", "check the --db path and its permissions; another ohya process may hold the database"))
	}
	defer hub.Close()

	if !*apply {
		plan, err := hub.PlanPurge(ctx, sel)
		if err != nil {
			return jsonFailurePrint(*asJSON, stdout, purgeSpec.name, err)
		}
		if *asJSON {
			return printJSON(stdout, purgeSpec.name, newPurgeJSON(plan, sel, false, nil))
		}
		printPurgePlan(stdout, plan)
		s := styleFor(stdout)
		fmt.Fprintf(stdout, "\n%s\n", s.yellow("dry run only; nothing was deleted; rerun with --apply to purge"))
		return nil
	}

	plan, res, err := hub.ApplyPurge(ctx, sel)
	if err != nil {
		return jsonFailurePrint(*asJSON, stdout, purgeSpec.name, err)
	}
	if *asJSON {
		return printJSON(stdout, purgeSpec.name, newPurgeJSON(plan, sel, true, &res))
	}
	s := styleFor(stdout)
	for _, src := range plan.Sources {
		fmt.Fprintf(stdout, "%s %s %s\n", s.green("purged source"), s.bold(src.ID), s.dim(fmt.Sprintf("(%d events, cursor %d)", src.Events, src.Cursor)))
	}
	printPurgeTotals(stdout, plan, "purged")
	return nil
}

func printPurgePlan(w io.Writer, plan store.PurgePlan) {
	s := styleFor(w)
	if len(plan.Sessions) == 0 {
		fmt.Fprintln(w, s.dim("0 sessions match; nothing to purge"))
		return
	}
	printPurgeTotals(w, plan, "would purge")
	fmt.Fprintf(w, "\n%s\n", s.section(fmt.Sprintf("Sessions (%d)", len(plan.Sessions))))
	sTbl := cliTable{
		indent:     "  ",
		headers:    []string{"SESSION", "AGENT", "DEVICE", "EVENTS"},
		rightAlign: []bool{false, false, false, true},
	}
	for _, ses := range plan.Sessions {
		sTbl.addRow(s.bold(ses.ID), s.cyan(ses.Agent), s.dim(ses.DeviceID), fmt.Sprintf("%d", ses.Events))
	}
	sTbl.render(w, s)

	fmt.Fprintf(w, "\n%s\n", s.section(fmt.Sprintf("Sources (%d)", len(plan.Sources))))
	srcTbl := cliTable{
		indent:     "  ",
		headers:    []string{"SOURCE", "AGENT", "DEVICE", "EVENTS", "CURSOR", "PATH"},
		rightAlign: []bool{false, false, false, true, true, false},
	}
	for _, src := range plan.Sources {
		srcTbl.addRow(
			s.bold(src.ID),
			s.cyan(src.Agent),
			s.dim(src.DeviceID),
			fmt.Sprintf("%d", src.Events),
			s.dim(fmt.Sprintf("%d", src.Cursor)),
			s.dim(src.Path),
		)
	}
	srcTbl.render(w, s)
}

func printPurgeTotals(w io.Writer, plan store.PurgePlan, verb string) {
	s := styleFor(w)
	line := fmt.Sprintf("%s %d sessions, %d sources, %d events, %d bytes",
		verb, len(plan.Sessions), len(plan.Sources), plan.Events, plan.Bytes)
	fmt.Fprintln(w, s.bold(line))
}

type purgeJSONSession struct {
	ID       string `json:"id"`
	Agent    string `json:"agent"`
	DeviceID string `json:"device_id"`
	NativeID string `json:"native_id"`
	Events   int64  `json:"events"`
}

type purgeJSONSource struct {
	ID       string `json:"id"`
	Agent    string `json:"agent"`
	DeviceID string `json:"device_id"`
	Path     string `json:"path"`
	Cursor   int64  `json:"cursor_bytes"`
	Events   int64  `json:"events"`
	NativeID string `json:"native_id,omitempty"`
	PurgedAt string `json:"purged_at,omitempty"`
}

type purgeJSONData struct {
	DryRun   bool               `json:"dry_run"`
	Scope    string             `json:"scope"`
	Sessions []purgeJSONSession `json:"sessions"`
	Sources  []purgeJSONSource  `json:"sources"`
	Events   int64              `json:"events"`
	Bytes    int64              `json:"bytes"`
}

func newPurgeJSON(plan store.PurgePlan, sel store.PurgeSelector, applied bool, res *store.PurgeResult) purgeJSONData {
	data := purgeJSONData{
		DryRun:   !applied,
		Scope:    sel.Scope(),
		Sessions: make([]purgeJSONSession, 0, len(plan.Sessions)),
		Sources:  make([]purgeJSONSource, 0, len(plan.Sources)),
		Events:   plan.Events,
		Bytes:    plan.Bytes,
	}
	for _, s := range plan.Sessions {
		data.Sessions = append(data.Sessions, purgeJSONSession{
			ID: s.ID, Agent: s.Agent, DeviceID: s.DeviceID, NativeID: s.NativeID, Events: s.Events,
		})
	}
	for _, src := range plan.Sources {
		js := purgeJSONSource{
			ID: src.ID, Agent: src.Agent, DeviceID: src.DeviceID, Path: src.Path,
			Cursor: src.Cursor, Events: src.Events, NativeID: src.NativeID,
		}
		if applied {
			js.PurgedAt = msTime(res.PurgedAtMS)
		}
		data.Sources = append(data.Sources, js)
	}
	return data
}
