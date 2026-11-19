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
		"Freed pages stay in the database file (no VACUUM), and a recovery copy taken before a purge still holds the purged data.",
	argsTitle: "Arguments",
	args:      "(none)",
	examples: []string{
		"ohya purge --repo github.com/owner/name --before 2026-11-01T00:00:00Z",
		"ohya purge --repo github.com/owner/name --before 2026-11-01T00:00:00Z --apply",
		"ohya purge --session 06055d1b-75a0-42a9-88fc-d3cbcb222b52 --apply",
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
	sel := store.PurgeSelector{
		Session:   *session,
		Repo:      *repo,
		Agent:     *agent,
		HasBefore: *before != "",
		BeforeMS:  beforeMS,
	}
	if sel.Empty() {
		return c.usageErrorf("purge needs at least one selector: --session, --repo, --agent, or --before")
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
		fmt.Fprintln(stdout, "dry run only; nothing was deleted; rerun with --apply to purge")
		return nil
	}

	plan, res, err := hub.ApplyPurge(ctx, sel)
	if err != nil {
		return jsonFailurePrint(*asJSON, stdout, purgeSpec.name, err)
	}
	if *asJSON {
		return printJSON(stdout, purgeSpec.name, newPurgeJSON(plan, sel, true, &res))
	}
	for _, src := range plan.Sources {
		fmt.Fprintf(stdout, "purged source %s (%d events, cursor %d)\n", src.ID, src.Events, src.Cursor)
	}
	printPurgeTotals(stdout, plan, "purged")
	return nil
}

func printPurgePlan(w io.Writer, plan store.PurgePlan) {
	if len(plan.Sessions) == 0 {
		fmt.Fprintln(w, "0 sessions match; nothing to purge")
		return
	}
	printPurgeTotals(w, plan, "would purge")
	fmt.Fprintf(w, "sessions (%d):\n", len(plan.Sessions))
	for _, s := range plan.Sessions {
		fmt.Fprintf(w, "  %s  agent=%s  device=%s  events=%d\n", s.ID, s.Agent, s.DeviceID, s.Events)
	}
	fmt.Fprintf(w, "sources (%d):\n", len(plan.Sources))
	for _, src := range plan.Sources {
		fmt.Fprintf(w, "  %s  agent=%s  device=%s  events=%d  cursor=%d  path=%s\n",
			src.ID, src.Agent, src.DeviceID, src.Events, src.Cursor, src.Path)
	}
}

func printPurgeTotals(w io.Writer, plan store.PurgePlan, verb string) {
	fmt.Fprintf(w, "%s %d sessions, %d sources, %d events, %d bytes\n",
		verb, len(plan.Sessions), len(plan.Sources), plan.Events, plan.Bytes)
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
