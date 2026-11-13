package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func mapFS(files map[string]string) fstest.MapFS {
	m := fstest.MapFS{}
	for name, content := range files {
		m[name] = &fstest.MapFile{Data: []byte(content)}
	}
	return m
}

// openBareWriter opens a writer-configured database without running
// migrations, so runner tests control the migration set.
func openBareWriter(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", writerDSN(filepath.Join(t.TempDir(), "migrate.db")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func appliedVersions(t *testing.T, db *sql.DB) []int {
	t.Helper()
	rows, err := db.Query(`SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var versions []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return versions
}

func TestMigrateFreshApplyRecordsVersionsInOrder(t *testing.T) {
	db := openBareWriter(t)
	fsys := mapFS(map[string]string{
		"migrations/001_one.sql": "CREATE TABLE a(x);",
		// Inserts into 001's table: applying 002 before 001 would fail, so
		// a clean run proves the ordering.
		"migrations/002_two.sql": "CREATE TABLE b(x); INSERT INTO a(x) VALUES (1);",
	})
	if err := migrate(context.Background(), db, fsys); err != nil {
		t.Fatal(err)
	}
	if got := appliedVersions(t, db); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("applied versions = %v, want [1 2]", got)
	}
	if !tableExists(t, db, "a") || !tableExists(t, db, "b") {
		t.Fatal("migrated tables missing")
	}
	sum := sha256.Sum256([]byte("CREATE TABLE a(x);"))
	var name, checksum string
	if err := db.QueryRow(`SELECT name, checksum FROM schema_migrations WHERE version=1`).Scan(&name, &checksum); err != nil {
		t.Fatal(err)
	}
	if name != "one" || checksum != hex.EncodeToString(sum[:]) {
		t.Fatalf("recorded %q %s, want one %s", name, checksum, hex.EncodeToString(sum[:]))
	}
}

func TestMigrateRepeatedRunAppliesNothing(t *testing.T) {
	db := openBareWriter(t)
	fsys := mapFS(map[string]string{
		"migrations/001_one.sql": "CREATE TABLE a(x); INSERT INTO a VALUES (1);",
	})
	if err := migrate(context.Background(), db, fsys); err != nil {
		t.Fatal(err)
	}
	if err := migrate(context.Background(), db, fsys); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM a`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows after rerun = %d, %v; want 1 (seed must not run twice)", n, err)
	}
	if got := appliedVersions(t, db); len(got) != 1 || got[0] != 1 {
		t.Fatalf("applied versions after rerun = %v, want [1]", got)
	}
}

func TestMigrateRejectsEditedAppliedMigration(t *testing.T) {
	db := openBareWriter(t)
	if err := migrate(context.Background(), db, mapFS(map[string]string{
		"migrations/001_one.sql": "CREATE TABLE a(x);",
	})); err != nil {
		t.Fatal(err)
	}
	err := migrate(context.Background(), db, mapFS(map[string]string{
		"migrations/001_one.sql": "CREATE TABLE a(x); INSERT INTO a VALUES (1);",
	}))
	if err == nil || !strings.Contains(err.Error(), "changed after being applied") {
		t.Fatalf("err = %v, want changed-after-applied rejection", err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM a`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("edited migration ran: rows = %d, %v", n, err)
	}
}

func TestMigrateRejectsDeletedAppliedMigration(t *testing.T) {
	db := openBareWriter(t)
	if err := migrate(context.Background(), db, mapFS(map[string]string{
		"migrations/001_one.sql": "CREATE TABLE a(x);",
		"migrations/002_two.sql": "CREATE TABLE b(x);",
	})); err != nil {
		t.Fatal(err)
	}
	err := migrate(context.Background(), db, mapFS(map[string]string{
		"migrations/002_two.sql": "CREATE TABLE b(x);",
	}))
	if err == nil || !strings.Contains(err.Error(), "missing from the binary") {
		t.Fatalf("err = %v, want missing-from-binary rejection", err)
	}
}

func TestMigrateRejectsNewMigrationBelowAppliedVersion(t *testing.T) {
	db := openBareWriter(t)
	if err := migrate(context.Background(), db, mapFS(map[string]string{
		"migrations/001_one.sql":   "CREATE TABLE a(x);",
		"migrations/003_three.sql": "CREATE TABLE c(x);",
	})); err != nil {
		t.Fatal(err)
	}
	err := migrate(context.Background(), db, mapFS(map[string]string{
		"migrations/001_one.sql":   "CREATE TABLE a(x);",
		"migrations/002_two.sql":   "CREATE TABLE b(x);",
		"migrations/003_three.sql": "CREATE TABLE c(x);",
	}))
	if err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("err = %v, want append-only rejection", err)
	}
	if tableExists(t, db, "b") {
		t.Fatal("out-of-order migration was applied")
	}
	if got := appliedVersions(t, db); len(got) != 2 {
		t.Fatalf("applied versions = %v, want [1 3]", got)
	}
}

func TestMigrateFailedMigrationRollsBackCompletely(t *testing.T) {
	db := openBareWriter(t)
	err := migrate(context.Background(), db, mapFS(map[string]string{
		"migrations/001_one.sql": "CREATE TABLE a(x);",
		"migrations/002_bad.sql": "CREATE TABLE b(x); INSERT INTO missing VALUES (1);",
	}))
	if err == nil || !strings.Contains(err.Error(), "002_bad") {
		t.Fatalf("err = %v, want failure naming 002_bad", err)
	}
	if tableExists(t, db, "b") {
		t.Fatal("failed migration left its table behind")
	}
	if got := appliedVersions(t, db); len(got) != 1 || got[0] != 1 {
		t.Fatalf("applied versions = %v, want [1]", got)
	}
	// 002 was never recorded, so fixing it is legal; rerunning resumes there.
	err = migrate(context.Background(), db, mapFS(map[string]string{
		"migrations/001_one.sql":   "CREATE TABLE a(x);",
		"migrations/002_bad.sql":   "CREATE TABLE b(x); INSERT INTO missing VALUES (1);",
		"migrations/003_three.sql": "CREATE TABLE c(x);",
	}))
	if err == nil || !strings.Contains(err.Error(), "002_bad") {
		t.Fatalf("err = %v, want the still-broken file to fail again", err)
	}
	if tableExists(t, db, "c") {
		t.Fatal("migration after a failing one was applied")
	}
	if err := migrate(context.Background(), db, mapFS(map[string]string{
		"migrations/001_one.sql":   "CREATE TABLE a(x);",
		"migrations/002_two.sql":   "CREATE TABLE b(x);",
		"migrations/003_three.sql": "CREATE TABLE c(x);",
	})); err != nil {
		t.Fatal(err)
	}
	if !tableExists(t, db, "b") || !tableExists(t, db, "c") {
		t.Fatal("rerun did not apply pending migrations")
	}
	if got := appliedVersions(t, db); len(got) != 3 {
		t.Fatalf("applied versions = %v, want [1 2 3]", got)
	}
}

func TestMigrateRejectsInvalidMigrationSets(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"wrong name pattern": {"migrations/foo.sql": "CREATE TABLE a(x);"},
		"short version":      {"migrations/1_a.sql": "CREATE TABLE a(x);"},
		"uppercase name":     {"migrations/001_A.sql": "CREATE TABLE a(x);"},
		"wrong extension":    {"migrations/001_a.txt": "CREATE TABLE a(x);"},
		"duplicate version": {
			"migrations/001_a.sql": "CREATE TABLE a(x);",
			"migrations/001_b.sql": "CREATE TABLE b(x);",
		},
		"empty set": {},
	} {
		t.Run(name, func(t *testing.T) {
			db := openBareWriter(t)
			if err := migrate(context.Background(), db, mapFS(files)); err == nil {
				t.Fatal("migrate accepted an invalid migration set")
			}
		})
	}
}
