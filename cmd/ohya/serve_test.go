package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServeContextCancelDuringStartupStopsCleanly(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "hub", "hub.db")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout bytes.Buffer
	err := runServe(ctx, serveOptions{
		dbPath:   dbPath,
		stateDir: filepath.Join(dir, "state"),
		stdout:   &stdout,
	})
	if err != nil {
		t.Fatalf("a context canceled before the hub listens or opens the database is a clean stop, got error: %v", err)
	}
	if out := stdout.String(); strings.Contains(out, "serving devices") {
		t.Fatalf("a canceled serve announced itself serving: %s", out)
	}
	if _, err := os.Stat(dbPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("database %s exists after the clean stop: %v", dbPath, err)
	}
}

func TestServeContextDeadlineDuringStartupFails(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	err := runServe(ctx, serveOptions{
		dbPath:   filepath.Join(dir, "hub", "hub.db"),
		stateDir: filepath.Join(dir, "state"),
		stdout:   &bytes.Buffer{},
	})
	if err == nil {
		t.Fatal("an expired startup deadline is a failure, not a clean stop; only SIGINT or SIGTERM stops serve cleanly")
	}
}
