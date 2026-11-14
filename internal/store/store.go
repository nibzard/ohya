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

const synchronousFull = 2

const busyTimeoutMs = 5000

type DB struct {
	path    string
	writer  *sql.DB
	readers *sql.DB
}

func Open(ctx context.Context, path string) (*DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("store: resolve %s: %w", path, err)
	}
	writer, err := sql.Open("sqlite3", writerDSN(abs))
	if err != nil {
		return nil, fmt.Errorf("store: open writer %s: %w", abs, err)
	}
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

func OpenReadOnly(path string) (*DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("store: resolve %s: %w", path, err)
	}
	readers, err := sql.Open("sqlite3", readerDSN(abs))
	if err != nil {
		return nil, fmt.Errorf("store: open reader %s: %w", abs, err)
	}
	if err := verifyReader(readers); err != nil {
		_ = readers.Close()
		return nil, err
	}
	return &DB{path: abs, readers: readers}, nil
}

func (d *DB) Writer() *sql.DB { return d.writer }

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

func (d *DB) Close() error {
	if d.writer != nil {
		return d.writer.Close()
	}
	return d.readers.Close()
}

func dsn(path string, params url.Values) string {
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	u.RawQuery = params.Encode()
	return u.String()
}

func writerDSN(path string) string {
	return dsn(path, url.Values{
		"_journal_mode": []string{"WAL"},
		"_synchronous":  []string{"FULL"},
		"_foreign_keys": []string{"on"},
		"_busy_timeout": []string{strconv.Itoa(busyTimeoutMs)},
	})
}

func readerDSN(path string) string {
	return dsn(path, url.Values{
		"mode":          []string{"ro"},
		"_foreign_keys": []string{"on"},
		"_busy_timeout": []string{strconv.Itoa(busyTimeoutMs)},
	})
}

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
