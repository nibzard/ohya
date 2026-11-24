package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/mattn/go-sqlite3"
)

var ErrNoSchemaMigrations = errors.New("store: no schema_migrations table; the database is not an ohya hub database")

var ErrFTSIndexInconsistent = errors.New("store: events_fts differs from the events table")

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

const parserVersionsQuery = `SELECT s.agent, max(m.newest)
 FROM sources s
 JOIN (SELECT source_id, max(parser_version) AS newest FROM events GROUP BY source_id) m
   ON m.source_id = s.id
 GROUP BY s.agent`

func (d *DB) MaxParserVersions(ctx context.Context) (map[string]int, error) {
	rows, err := d.readers.QueryContext(ctx, parserVersionsQuery)
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

func (d *DB) ReadOnlySourceState(ctx context.Context, id string) (SourceState, error) {
	return sourceState(ctx, d.readers, id, "")
}

func (d *DB) FTSIndexConsistent(ctx context.Context) error {
	tmpDir, err := os.MkdirTemp("", "ohya-fts-check-*")
	if err != nil {
		return fmt.Errorf("store: create integrity snapshot dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)
	snapPath := filepath.Join(tmpDir, "check.db")
	snap, err := sql.Open("sqlite3", dsn(snapPath, nil))
	if err != nil {
		return fmt.Errorf("store: open integrity snapshot: %w", err)
	}
	defer snap.Close()
	snap.SetMaxOpenConns(1)
	snap.SetMaxIdleConns(1)
	dst, err := snap.Conn(ctx)
	if err != nil {
		return fmt.Errorf("store: open integrity snapshot connection: %w", err)
	}
	defer dst.Close()
	readers := d.readers
	if readers == nil {
		readers, err = d.Reader()
		if err != nil {
			return err
		}
		defer readers.Close()
	}
	src, err := readers.Conn(ctx)
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
			if err := copySnapshot(ctx, dstConn, srcConn, nil); err != nil {
				return fmt.Errorf("store: snapshot database: %w", err)
			}
			return nil
		})
	}); err != nil {
		return err
	}
	for _, stmt := range []string{
		`PRAGMA journal_mode = OFF`,
		`PRAGMA synchronous = OFF`,
		`DROP TRIGGER events_fts_delete`,
		`DELETE FROM events WHERE text IS NULL OR length(text) = 0`,
	} {
		if _, err := dst.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("store: prepare integrity snapshot: %q: %w", stmt, err)
		}
	}
	if _, err := dst.ExecContext(ctx,
		`INSERT INTO events_fts(events_fts, rank) VALUES ('integrity-check', 1)`); err != nil {
		var sqliteErr sqlite3.Error
		if errors.As(err, &sqliteErr) && sqliteErr.Code == sqlite3.ErrCorrupt {
			return fmt.Errorf("%w: %w", ErrFTSIndexInconsistent, err)
		}
		return fmt.Errorf("store: events_fts integrity-check: %w", err)
	}
	return nil
}
