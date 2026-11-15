package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestBackupIncludesWALAndPreservesDestination(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := filepath.Join(dir, "hub.db")
	db, err := Open(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.writer.Exec("PRAGMA wal_autocheckpoint=0"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.writer.Exec("CREATE TABLE backup_probe (value TEXT); INSERT INTO backup_probe VALUES ('durable')"); err != nil {
		t.Fatal(err)
	}
	wal, err := os.Stat(source + "-wal")
	if err != nil || wal.Size() == 0 {
		t.Fatalf("no WAL: %v", err)
	}
	destination := filepath.Join(dir, "backup.db")
	if err := Backup(ctx, source, destination); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(ctx, destination)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	var value string
	if err := restored.writer.QueryRow("SELECT value FROM backup_probe").Scan(&value); err != nil || value != "durable" {
		t.Fatalf("restored %q: %v", value, err)
	}
	if err := Backup(ctx, source, destination); err == nil {
		t.Fatal("overwrote recovery point")
	}
	info, err := os.Stat(destination)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("mode %v: %v", info, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := Backup(cancelled, source, filepath.Join(dir, "cancelled.db")); err == nil {
		t.Fatal("ignored cancellation")
	}
}
