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

	"github.com/nibzard/ohya/internal/store"
)

var sourcesSpec = commandSpec{
	name:      "ohya sources",
	synopsis:  "manage stored sources on the hub",
	usageLine: "ohya sources <command> [flags]",
	description: "The hub operator's surface for source lifecycle actions that live in the transcript database. " +
		"A purged source is tombstoned and refused at the write path until the owner re-admits it here.",
	argsTitle: "Commands",
	args:      `reallow <source-id>  remove a purge tombstone so its device syncs again`,
	examples:  []string{"ohya sources reallow <source-id>"},
}

func runSourcesCommand(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || isHelpArg(args[0]) {
		writeUsage(stdout, sourcesSpec, nil)
		return nil
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "reallow":
		return runSourcesReallow(rest, stdout, stderr)
	default:
		return usageError{cmd: "ohya sources", msg: fmt.Sprintf("unknown sources subcommand %q", cmd)}
	}
}

var sourcesReallowSpec = commandSpec{
	name:      "ohya sources reallow",
	synopsis:  "re-admit a purged source",
	usageLine: "ohya sources reallow <source-id> [flags]",
	description: "Removes one purge tombstone and restores the source row at its purged cursor, so history resumes from the purge point when the source is checked again. " +
		"The purged bytes themselves never return: a replay below the restored cursor is rejected. " +
		"Re-admission does not reauthorize anything else; a revoked device stays revoked.",
	args:     "<source-id>  the purged source ID printed by ohya purge and named in the device's refusal",
	examples: []string{"ohya sources reallow <source-id>"},
}

func runSourcesReallow(args []string, stdout, stderr io.Writer) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve home directory: %w", err)
	}
	c := newCommandFlags(sourcesReallowSpec)
	db := c.fs.String("db", filepath.Join(home, ".ohya", "hub.db"),
		"hub database `path`")
	asJSON := c.fs.Bool("json", false, "print one JSON object instead of text (docs/adr/0005)")
	if err := c.parse(args, stdout); err != nil {
		return err
	}
	if c.fs.NArg() != 1 {
		return c.usageErrorf("reallow needs exactly one source ID; the purge report and the device's refusal name it")
	}
	sourceID := c.fs.Arg(0)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	hub, err := store.Open(ctx, *db)
	if err != nil {
		return jsonFailurePrint(*asJSON, stdout, sourcesReallowSpec.name,
			classify(err, "database_open_failed", "check the --db path and its permissions; another ohya process may hold the database"))
	}
	defer hub.Close()

	info, err := hub.ReallowSource(ctx, sourceID)
	if err != nil {
		if errors.Is(err, store.ErrNotPurged) {
			return jsonFailurePrint(*asJSON, stdout, sourcesReallowSpec.name,
				classify(err, "source_not_purged", "run ohya purge with the selectors of the purge; its report prints the purged source IDs"))
		}
		return jsonFailurePrint(*asJSON, stdout, sourcesReallowSpec.name,
			classify(err, "reallow_failed", "check the hub database state and retry"))
	}
	if *asJSON {
		return printJSON(stdout, sourcesReallowSpec.name, reallowJSONData{
			SourceID: sourceID, CursorBytes: info.Cursor,
		})
	}
	fmt.Fprintf(stdout, "%s re-admitted at cursor %d; capture resumes from the purge point when the source is checked again\n",
		sourceID, info.Cursor)
	return nil
}

type reallowJSONData struct {
	SourceID    string `json:"source_id"`
	CursorBytes int64  `json:"cursor_bytes"`
}
