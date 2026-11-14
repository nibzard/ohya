// Read-only inspection queries for ohya doctor: checks that compare the hub
// database against this binary — the applied schema against the embedded
// migrations, stored parser output versions against the parsers in this
// build, and the FTS index against the content rows. They run on the
// read-only pool of a DB opened with OpenReadOnly and never write.

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/mattn/go-sqlite3"
)

// ErrNoSchemaMigrations reports a database that holds no schema_migrations
// table: not a database this store's writer created or migrated.
var ErrNoSchemaMigrations = errors.New("store: no schema_migrations table; the database is not an ohya hub database")

// AppliedSchemaVersion returns the highest migration version the database
// records as applied. found is false when the table holds no row.
func (d *DB) AppliedSchemaVersion(ctx context.Context) (version int, found bool, err error) {
	var name string
	err = d.readers.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'`).
		Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, ErrNoSchemaMigrations
	}
	if err != nil {
		return 0, false, fmt.Errorf("store: read schema_migrations presence: %w", err)
	}
	var max sql.NullInt64
	if err := d.readers.QueryRowContext(ctx,
		`SELECT max(version) FROM schema_migrations`).Scan(&max); err != nil {
		return 0, false, fmt.Errorf("store: read schema_migrations: %w", err)
	}
	if !max.Valid {
		return 0, false, nil
	}
	return int(max.Int64), true, nil
}

// MaxParserVersions returns, for every agent with stored events, the highest
// parser output version stamped on its rows — the numbers store.ReDerive
// compares a build's parsers against.
func (d *DB) MaxParserVersions(ctx context.Context) (map[string]int, error) {
	rows, err := d.readers.QueryContext(ctx,
		`SELECT s.agent, max(e.parser_version)
		 FROM events e JOIN sources s ON s.id = e.source_id
		 GROUP BY s.agent`)
	if err != nil {
		return nil, fmt.Errorf("store: read parser versions: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var agent string
		var version int
		if err := rows.Scan(&agent, &version); err != nil {
			return nil, fmt.Errorf("store: read parser versions: %w", err)
		}
		out[agent] = version
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read parser versions: %w", err)
	}
	return out, nil
}

// ReadOnlySourceState is SourceState over the read-only pool, for inspection
// of sources without a writer. Valid only on a DB opened with OpenReadOnly.
func (d *DB) ReadOnlySourceState(ctx context.Context, id string) (SourceState, error) {
	return sourceState(ctx, d.readers, id)
}

// FTSIndexConsistent reports whether the events_fts index agrees with the
// stored events.text rows, by running the FTS5 integrity-check command.
// Two properties of SQLite force the indirection through a private
// in-memory snapshot, taken with the backup API (which reads the source and
// writes nothing; the hub database stays untouched):
//
//   - The command is an SQLite write statement, and SQLite refuses it on a
//     mode=ro connection.
//   - With the rank argument that turns on the external-content comparison,
//     the command demands a docsize entry for every content row. This schema
//     deliberately leaves text-less events unindexed, so on the snapshot the
//     maintenance trigger is dropped and those rows removed first: the
//     comparison then covers exactly the rows the index is supposed to hold.
//     Without the rank argument the command checks the index structure alone
//     and passes any content drift.
func (d *DB) FTSIndexConsistent(ctx context.Context) error {
	mem, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		return fmt.Errorf("store: open integrity snapshot: %w", err)
	}
	defer mem.Close()
	// One connection keeps the snapshot and the check on the same in-memory
	// database; a pool would give every connection its own.
	mem.SetMaxOpenConns(1)
	mem.SetMaxIdleConns(1)
	dst, err := mem.Conn(ctx)
	if err != nil {
		return fmt.Errorf("store: open integrity snapshot connection: %w", err)
	}
	defer dst.Close()
	src, err := d.readers.Conn(ctx)
	if err != nil {
		return fmt.Errorf("store: open reader connection: %w", err)
	}
	defer src.Close()
	if err := dst.Raw(func(dstRaw any) error {
		return src.Raw(func(srcRaw any) error {
			dstConn, ok := dstRaw.(*sqlite3.SQLiteConn)
			if !ok {
				return fmt.Errorf("store: integrity snapshot destination is %T, want the bundled SQLite driver", dstRaw)
			}
			srcConn, ok := srcRaw.(*sqlite3.SQLiteConn)
			if !ok {
				return fmt.Errorf("store: integrity snapshot source is %T, want the bundled SQLite driver", srcRaw)
			}
			backup, err := dstConn.Backup("main", srcConn, "main")
			if err != nil {
				return fmt.Errorf("store: snapshot database: %w", err)
			}
			done, stepErr := backup.Step(-1)
			finishErr := backup.Close()
			switch {
			case stepErr != nil:
				return fmt.Errorf("store: snapshot database: %w", stepErr)
			case finishErr != nil:
				return fmt.Errorf("store: snapshot database: %w", finishErr)
			case !done:
				return fmt.Errorf("store: snapshot database: incomplete")
			}
			return nil
		})
	}); err != nil {
		return err
	}
	// Prepare the snapshot: without the delete trigger, removing text-less
	// events leaves the index untouched, so the comparison sees exactly the
	// rows the write path indexes.
	for _, stmt := range []string{
		`DROP TRIGGER events_fts_delete`,
		`DELETE FROM events WHERE text IS NULL OR length(text) = 0`,
	} {
		if _, err := dst.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("store: prepare integrity snapshot: %q: %w", stmt, err)
		}
	}
	// The check runs on the same checked-out connection: the single-connection
	// pool holds it, so the snapshot is the database this statement sees.
	if _, err := dst.ExecContext(ctx,
		`INSERT INTO events_fts(events_fts, rank) VALUES ('integrity-check', 1)`); err != nil {
		return fmt.Errorf("store: events_fts integrity-check: %w", err)
	}
	return nil
}
