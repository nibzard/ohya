// The rederive subcommand: rebuild normalized event rows and events_fts from
// the stored raw records. Run it after upgrading to a build whose parser
// carries a higher output version; an up-to-date hub is a no-op.
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

	"github.com/nibzard/ohya/internal/events"
	"github.com/nibzard/ohya/internal/store"
)

// runRederiveCommand parses the rederive flags and re-derives the hub
// database in one transaction.
func runRederiveCommand(args []string, stdout, stderr io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	fs := flag.NewFlagSet("ohya rederive", flag.ContinueOnError)
	fs.SetOutput(stderr)
	db := fs.String("db", filepath.Join(home, ".ohya", "hub.db"),
		"hub database `path`")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q", fs.Arg(0))
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
