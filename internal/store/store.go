// Package store owns the hub's SQLite database: versioned migrations, the
// single durable writer connection, and separate restricted reader
// connections.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"

	_ "github.com/mattn/go-sqlite3"
)

// synchronousFull is the PRAGMA synchronous value for FULL durability.
const synchronousFull = 2

// busyTimeoutMs bounds how long a connection waits on a lock held by another
// process (for example a backup tool) before failing.
const busyTimeoutMs = 5000

// DB is the hub-owned handle to the transcript database. All writes go
// through its single writer connection; readers get separate read-only
// connections.
type DB struct {
	path   string
	writer *sql.DB
}

// Open opens (creating if needed) the database at path, configures the
// single writer connection, and applies pending migrations. Only one ohya
// process may hold the writer.
func Open(ctx context.Context, path string) (*DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("store: resolve %s: %w", path, err)
	}
	writer, err := sql.Open("sqlite3", writerDSN(abs))
	if err != nil {
		return nil, fmt.Errorf("store: open writer %s: %w", abs, err)
	}
	// Exactly one writer connection: writes serialize on it and cannot
	// interleave.
	writer.SetMaxOpenConns(1)
	writer.SetMaxIdleConns(1)
	if err := verifyWriter(ctx, writer); err != nil {
		_ = writer.Close()
		return nil, err
	}
	if err := migrate(ctx, writer, migrationsFS); err != nil {
		_ = writer.Close()
		return nil, err
	}
	return &DB{path: abs, writer: writer}, nil
}

// Writer returns the single connection that may write. Every write —
// ingestion, annotations, migrations — must go through it.
func (d *DB) Writer() *sql.DB { return d.writer }

// Reader opens a pool of separate read-only connections. The database must
// already exist and be migrated, so open the writer first; read-only
// connections cannot create a database or recover a write-ahead log. Keep
// the writer running while readers are open.
func (d *DB) Reader() (*sql.DB, error) {
	readers, err := sql.Open("sqlite3", readerDSN(d.path))
	if err != nil {
		return nil, fmt.Errorf("store: open reader %s: %w", d.path, err)
	}
	if err := verifyReader(readers); err != nil {
		_ = readers.Close()
		return nil, err
	}
	return readers, nil
}

// Close closes the writer connection.
func (d *DB) Close() error { return d.writer.Close() }

// dsn builds a SQLite URI for path. Parameters with a leading underscore
// configure the driver on every pooled connection; others (mode=ro) reach
// SQLite itself when it opens the file.
func dsn(path string, params url.Values) string {
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	u.RawQuery = params.Encode()
	return u.String()
}

// writerDSN configures the writer connection: WAL journaling, FULL
// synchronous durability, foreign keys, and a busy timeout. The driver
// applies these to every connection it opens for the pool.
func writerDSN(path string) string {
	return dsn(path, url.Values{
		"_journal_mode": []string{"WAL"},
		"_synchronous":  []string{"FULL"},
		"_foreign_keys": []string{"on"},
		"_busy_timeout": []string{strconv.Itoa(busyTimeoutMs)},
	})
}

// readerDSN configures reader connections: opened read-only at the SQLite
// level (mode=ro) with foreign keys and the same busy timeout.
func readerDSN(path string) string {
	return dsn(path, url.Values{
		"mode":          []string{"ro"},
		"_foreign_keys": []string{"on"},
		"_busy_timeout": []string{strconv.Itoa(busyTimeoutMs)},
	})
}

// verifyWriter fails fast when the durability settings are not active, so a
// misconfigured connection surfaces at open instead of at first commit.
func verifyWriter(ctx context.Context, db *sql.DB) error {
	var journal string
	if err := db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journal); err != nil {
		return fmt.Errorf("store: read writer journal_mode: %w", err)
	}
	if journal != "wal" {
		return fmt.Errorf("store: writer journal_mode is %q, want WAL", journal)
	}
	var synchronous int
	if err := db.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&synchronous); err != nil {
		return fmt.Errorf("store: read writer synchronous: %w", err)
	}
	if synchronous != synchronousFull {
		return fmt.Errorf("store: writer synchronous is %d, want FULL (%d)", synchronous, synchronousFull)
	}
	if err := verifyForeignKeys(ctx, db); err != nil {
		return err
	}
	return nil
}

// verifyReader checks the settings on one reader connection, which also
// proves the database exists and is readable in WAL mode.
func verifyReader(db *sql.DB) error {
	var journal string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&journal); err != nil {
		return fmt.Errorf("store: open reader: %w", err)
	}
	if journal != "wal" {
		return fmt.Errorf("store: reader journal_mode is %q, want WAL", journal)
	}
	if err := verifyForeignKeys(context.Background(), db); err != nil {
		return err
	}
	return nil
}

func verifyForeignKeys(ctx context.Context, db *sql.DB) error {
	var foreignKeys int
	if err := db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		return fmt.Errorf("store: read foreign_keys: %w", err)
	}
	if foreignKeys != 1 {
		return fmt.Errorf("store: foreign_keys is %d, want 1", foreignKeys)
	}
	return nil
}
