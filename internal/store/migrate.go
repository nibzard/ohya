package store

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strconv"
	"time"
)

// migrationsFS embeds the versioned SQL migrations shipped with the binary.
// Files are named NNN_description.sql with a three-digit version.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

var migrationNameRE = regexp.MustCompile(`^([0-9]{3})_([a-z0-9]+(?:_[a-z0-9]+)*)\.sql$`)

// schemaMigrationsDDL is the runner's own bookkeeping table. Migrations hold
// schema and data changes only; connection configuration is code in this
// package (SQLite cannot switch journal mode inside a transaction, and
// PRAGMA foreign_keys is a silent no-op there).
const schemaMigrationsDDL = `CREATE TABLE IF NOT EXISTS schema_migrations (
  version       INTEGER PRIMARY KEY,
  name          TEXT NOT NULL,
  checksum      TEXT NOT NULL,
  applied_at_ms INTEGER NOT NULL
) STRICT`

type migration struct {
	version  int
	name     string
	checksum string
	script   string
}

// loadMigrations reads and validates the migration set and returns it in
// version order. Names must match NNN_name.sql and versions must be unique.
func loadMigrations(fsys fs.FS) ([]migration, error) {
	files, err := fs.Glob(fsys, "migrations/*.sql")
	if err != nil {
		return nil, fmt.Errorf("store: list migrations: %w", err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("store: no migrations found")
	}
	var ms []migration
	for _, file := range files {
		base := path.Base(file)
		parts := migrationNameRE.FindStringSubmatch(base)
		if parts == nil {
			return nil, fmt.Errorf("store: migration name %q does not match NNN_name.sql", base)
		}
		version, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil, fmt.Errorf("store: migration version in %q: %w", base, err)
		}
		data, err := fs.ReadFile(fsys, file)
		if err != nil {
			return nil, fmt.Errorf("store: read migration %q: %w", base, err)
		}
		sum := sha256.Sum256(data)
		ms = append(ms, migration{
			version:  version,
			name:     parts[2],
			checksum: hex.EncodeToString(sum[:]),
			script:   string(data),
		})
	}
	slices.SortFunc(ms, func(a, b migration) int { return cmp.Compare(a.version, b.version) })
	for i := 1; i < len(ms); i++ {
		if ms[i].version == ms[i-1].version {
			return nil, fmt.Errorf("store: duplicate migration version %03d (%s, %s)",
				ms[i].version, ms[i-1].name, ms[i].name)
		}
	}
	return ms, nil
}

// migrate brings the database up to the newest migration in fsys. Applied
// migrations never run again: their recorded checksums must match the
// embedded files, and only files numbered above every applied version may be
// new. Each pending migration and its schema_migrations row commit in one
// transaction, so a failed migration rolls back completely and a rerun
// resumes at the first pending file.
func migrate(ctx context.Context, db *sql.DB, fsys fs.FS) error {
	ms, err := loadMigrations(fsys)
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, schemaMigrationsDDL); err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}
	applied, err := appliedMigrations(ctx, db)
	if err != nil {
		return err
	}
	byVersion := make(map[int]migration, len(ms))
	for _, m := range ms {
		byVersion[m.version] = m
	}
	newest := 0
	for version, checksum := range applied {
		m, ok := byVersion[version]
		if !ok {
			return fmt.Errorf("store: migration %03d is applied but missing from the binary; applied migrations must not be deleted", version)
		}
		if checksum != m.checksum {
			return fmt.Errorf("store: migration %03d_%s changed after being applied; applied migrations are immutable", version, m.name)
		}
		if version > newest {
			newest = version
		}
	}
	for _, m := range ms {
		if _, ok := applied[m.version]; ok {
			continue
		}
		if m.version < newest {
			return fmt.Errorf("store: migration %03d_%s is new but numbered below applied migration %03d; migrations are append-only", m.version, m.name, newest)
		}
		if err := applyMigration(ctx, db, m); err != nil {
			return err
		}
	}
	return nil
}

func appliedMigrations(ctx context.Context, db *sql.DB) (map[int]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("store: read schema_migrations: %w", err)
	}
	defer rows.Close()
	applied := make(map[int]string)
	for rows.Next() {
		var version int
		var checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			return nil, fmt.Errorf("store: read schema_migrations row: %w", err)
		}
		applied[version] = checksum
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read schema_migrations: %w", err)
	}
	return applied, nil
}

func applyMigration(ctx context.Context, db *sql.DB, m migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: migration %03d_%s: begin: %w", m.version, m.name, err)
	}
	if _, err := tx.ExecContext(ctx, m.script); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("store: migration %03d_%s: %w", m.version, m.name, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, checksum, applied_at_ms) VALUES (?, ?, ?, ?)`,
		m.version, m.name, m.checksum, time.Now().UnixMilli()); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("store: migration %03d_%s: record: %w", m.version, m.name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: migration %03d_%s: commit: %w", m.version, m.name, err)
	}
	return nil
}
