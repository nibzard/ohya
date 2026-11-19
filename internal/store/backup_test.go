package store

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func createBackupSource(t *testing.T, dir, name string, filler bool) (*DB, string) {
	t.Helper()
	source := filepath.Join(dir, name)
	db, err := Open(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.writer.Exec("PRAGMA wal_autocheckpoint=0"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.writer.Exec("CREATE TABLE backup_probe (value TEXT); INSERT INTO backup_probe VALUES ('durable')"); err != nil {
		t.Fatal(err)
	}
	if filler {
		if _, err := db.writer.Exec("CREATE TABLE backup_progress_probe (padding TEXT); INSERT INTO backup_progress_probe VALUES (hex(zeroblob(720896)))"); err != nil {
			t.Fatal(err)
		}
	}
	return db, source
}

func TestBackupIncludesWALAndPreservesDestination(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, source := createBackupSource(t, dir, "hub.db", false)
	defer db.Close()
	wal, err := os.Stat(source + "-wal")
	if err != nil || wal.Size() == 0 {
		t.Fatalf("no WAL: %v", err)
	}
	destination := filepath.Join(dir, "backup.db")
	if err := Backup(ctx, source, destination, nil); err != nil {
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
	if err := Backup(ctx, source, destination, nil); err == nil {
		t.Fatal("overwrote recovery point")
	}
	info, err := os.Stat(destination)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("mode %v: %v", info, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := Backup(cancelled, source, filepath.Join(dir, "cancelled.db"), nil); err == nil {
		t.Fatal("ignored cancellation")
	}
	if _, err := os.Stat(filepath.Join(dir, "cancelled.db")); !os.IsNotExist(err) {
		t.Fatal("kept an interrupted destination")
	}
}

func TestBackupReportsProgress(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, source := createBackupSource(t, dir, "hub.db", true)
	defer db.Close()
	var done []int64
	var totals []int64
	if err := Backup(ctx, source, filepath.Join(dir, "backup.db"), func(copied, total int64) {
		done = append(done, copied)
		totals = append(totals, total)
	}); err != nil {
		t.Fatal(err)
	}
	if len(done) < 2 || done[0] >= totals[0] {
		t.Fatalf("no copy progress reported: %v of %v", done, totals)
	}
	info, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	if totals[0] < info.Size()/2 {
		t.Fatalf("progress total %d does not track the source size %d", totals[0], info.Size())
	}
	if !slices.Equal(totals, slices.Repeat([]int64{totals[0]}, len(totals))) {
		t.Fatalf("totals changed during the copy: %v", totals)
	}
	if done[len(done)-1] != totals[0] {
		t.Fatalf("final progress %d of %d", done[len(done)-1], totals[0])
	}
	if !slices.IsSorted(done) {
		t.Fatalf("progress not monotonic: %v", done)
	}
}

func TestBackupBudget(t *testing.T) {
	for _, tc := range []struct {
		total int64
		want  time.Duration
	}{
		{0, 2 * time.Minute},
		{30 << 20, 2 * time.Minute},
		{1 << 30, (1 << 30 / (256 << 10)) * time.Second},
	} {
		if got := backupBudget(tc.total); got != tc.want {
			t.Fatalf("backupBudget(%d) = %v, want %v", tc.total, got, tc.want)
		}
	}
}

func TestSourcePageMetricsReflectsDatabaseSize(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, source := createBackupSource(t, dir, "hub.db", true)
	defer db.Close()

	ro, err := OpenReadOnly(source)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	conn, err := ro.readers.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	pageSize, pageCount, err := sourcePageMetrics(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	total := pageSize * pageCount
	if pageSize <= 0 || pageCount <= 0 || total < 1<<20 {
		t.Fatalf("sourcePageMetrics = (%d, %d) (%d bytes), want > 1 MiB", pageSize, pageCount, total)
	}
}
