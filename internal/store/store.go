package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"
	"github.com/nibzard/ohya/internal/observe"
)

const synchronousFull = 2

const busyTimeoutMs = 5000

const busyTimeout = time.Duration(busyTimeoutMs) * time.Millisecond

const openBusyPause = 20 * time.Millisecond

type DB struct {
	path        string
	writer      *sql.DB
	readers     *sql.DB
	observer    observe.Observer
	waitForLock bool
	lockReport  func(error)
}

type OpenOption func(*DB)

func WithObserver(o observe.Observer) OpenOption {
	return func(d *DB) {
		if o != nil {
			d.observer = o
		}
	}
}

func WaitForLock(report func(error)) OpenOption {
	return func(d *DB) {
		d.waitForLock = true
		d.lockReport = report
	}
}

func Open(ctx context.Context, path string, opts ...OpenOption) (*DB, error) {
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
	d := &DB{path: abs, writer: writer, observer: observe.Disabled}
	for _, opt := range opts {
		opt(d)
	}
	if err := prepareWriter(ctx, d); err != nil {
		_ = writer.Close()
		return nil, err
	}
	return d, nil
}

func prepareWriter(ctx context.Context, d *DB) error {
	return d.retryWriteLock(ctx, func() error {
		if err := verifyWriter(ctx, d.writer); err != nil {
			return err
		}
		return migrate(ctx, d.writer, migrationsFS)
	})
}

func (d *DB) retryWriteLock(ctx context.Context, attempt func() error) error {
	wait := busyTimeout
	var report func(error)
	if d.waitForLock {
		wait = 0
		if d.lockReport != nil {
			report = atMostEvery(busyTimeout, d.lockReport)
		}
	}
	return retryBusy(ctx, wait, func() error {
		err := attempt()
		if busyDatabase(err) && report != nil {
			report(err)
		}
		return err
	})
}

func atMostEvery(interval time.Duration, report func(error)) func(error) {
	var last time.Time
	return func(err error) {
		if time.Since(last) < interval {
			return
		}
		last = time.Now()
		report(err)
	}
}

func retryBusy(ctx context.Context, wait time.Duration, attempt func() error) error {
	deadline := time.Time{}
	for {
		started := time.Now()
		err := attempt()
		if err == nil || !busyDatabase(err) {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if deadline.IsZero() {
			deadline = started.Add(wait)
		}
		if wait > 0 && !time.Now().Before(deadline) {
			return fmt.Errorf("%w; the database stayed locked for %s, so another process may be migrating the database or may hold a long write", err, wait)
		}
		timer := time.NewTimer(openBusyPause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func busyDatabase(err error) bool {
	outcome, retryable := sqliteOutcome(err)
	return retryable && outcome == "busy"
}

func sqliteOutcome(err error) (string, bool) {
	if err == nil {
		return "ok", true
	}
	var sqliteErr sqlite3.Error
	if errors.As(err, &sqliteErr) {
		switch sqliteErr.Code & 0xff {
		case sqlite3.ErrBusy:
			return "busy", true
		case sqlite3.ErrLocked:
			return "locked", true
		}
		return "error", true
	}
	if errors.Is(err, ErrBatchRejected) || errors.Is(err, ErrSourceOwnership) ||
		errors.Is(err, ErrSourceNotRegistered) || errors.Is(err, ErrSourcePurged) {
		return "", false
	}
	return "error", true
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
		"_txlock":       []string{"immediate"},
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
	if _, err := db.ExecContext(ctx, fmt.Sprintf(`PRAGMA journal_size_limit = %d`, walJournalSizeLimit)); err != nil {
		return fmt.Errorf("store: set journal_size_limit: %w", err)
	}
	return nil
}

const walJournalSizeLimit = 64 << 20

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
