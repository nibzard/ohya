// The rederive subcommand: rebuild normalized event rows and events_fts from
// the stored raw records. Run it after upgrading to a build whose parser
// carries a higher output version; an up-to-date hub is a no-op.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/nibzard/ohya/internal/events"
	"github.com/nibzard/ohya/internal/store"
)

// rederiveSpec is the rederive usage template.
var rederiveSpec = commandSpec{
	name:      "ohya rederive",
	synopsis:  "rebuild normalized rows and events_fts after a parser upgrade",
	usageLine: "ohya rederive [flags]",
	description: "Re-derives stale normalized event rows from the stored raw records — inside the hub, with no access to the original files — in one transaction, recomputing sessions and token totals and rebuilding the search index. " +
		"Event IDs and annotations survive, and an up-to-date hub is a no-op, so the command is safe to run any time.",
	argsTitle: "Arguments",
	args:      "(none)",
	examples:  []string{"ohya rederive", "ohya rederive --db ~/.ohya/hub.db"},
}

// runRederiveCommand parses the rederive flags and re-derives the hub
// database in one transaction.
func runRederiveCommand(args []string, stdout, stderr io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	c := newCommandFlags(rederiveSpec)
	db := c.fs.String("db", filepath.Join(home, ".ohya", "hub.db"),
		"hub database `path`")
	if err := c.parse(args, stdout); err != nil {
		return err
	}
	if c.fs.NArg() > 0 {
		return c.usageErrorf("unexpected argument %q", c.fs.Arg(0))
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	hub, err := store.Open(ctx, *db)
	if err != nil {
		return err
	}
	defer hub.Close()
	res, err := hub.ReDerive(ctx, events.Derivers())
	if err != nil {
		return err
	}
	if res.Events == 0 {
		fmt.Fprintln(stdout, "all stored rows carry the current parser output version; nothing re-derived")
		return nil
	}
	fmt.Fprintf(stdout, "re-derived %d events across %d sessions; events_fts rebuilt\n", res.Events, res.Sessions)
	return nil
}
