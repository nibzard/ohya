package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nibzard/ohya/internal/claudecode"
	"github.com/nibzard/ohya/internal/device"
	"github.com/nibzard/ohya/internal/observe"
	"github.com/nibzard/ohya/internal/observe/observertest"
	"github.com/nibzard/ohya/internal/store"
	"github.com/nibzard/ohya/internal/watch"
)

func TestRunIngestObserverMeasuresPassesAndIdleIO(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "projects")
	if err := os.MkdirAll(filepath.Join(root, "fixture-project"), 0o700); err != nil {
		t.Fatal(err)
	}
	session := filepath.Join(root, "fixture-project", "main-session.jsonl")
	copyFixture(t, session, "main-session.jsonl")
	info, err := os.Stat(session)
	if err != nil {
		t.Fatal(err)
	}
	size := uint64(info.Size())

	opts := func(obs observe.Observer) ingestOptions {
		return ingestOptions{
			dbPath:         filepath.Join(dir, "hub", "hub.db"),
			stateDir:       filepath.Join(dir, "state"),
			roots:          []agentRoot{{agent: claudecode.AgentName, flag: "claude-root", dirs: []agentDir{{dir: root}}}},
			interval:       time.Second,
			once:           true,
			maxRecordBytes: claudecode.DefaultMaxRecordBytes,
			stdout:         io.Discard,
			stderr:         io.Discard,
			observer:       obs,
		}
	}
	obs := observertest.New()
	if err := runIngest(context.Background(), opts(obs)); err != nil {
		t.Fatal(err)
	}

	if got := obs.Counter("ingest_passes"); got != 1 {
		t.Errorf("ingest passes = %d, want 1", got)
	}
	if got := obs.DurationCount("ingest_pass_duration"); got != 1 {
		t.Errorf("pass durations = %d, want 1", got)
	}
	if got := obs.Counter("discovery_scans"); got != 1 {
		t.Errorf("discovery scans = %d, want 1", got)
	}
	if got := obs.Counter("discovery_candidates"); got != 1 {
		t.Errorf("discovery candidates = %d, want 1", got)
	}
	if got := obs.Counter("parse_records", "attempted"); got != 16 {
		t.Errorf("records attempted = %d, want 16", got)
	}
	if got := obs.Counter("parse_records", "normalized"); got != 16 {
		t.Errorf("records normalized = %d, want 16", got)
	}
	if got := obs.Counter("parse_bytes"); got != size {
		t.Errorf("parse bytes = %d, want the %d fixture bytes", got, size)
	}
	if got := obs.Counter("ingest_events", "inserted"); got != 16 {
		t.Errorf("events inserted = %d, want 16", got)
	}
	if got := obs.Counter("ingest_bytes"); got != size {
		t.Errorf("ingest bytes = %d, want the %d accepted fixture bytes", got, size)
	}
	if got := obs.Counter("ingest_batches", "committed"); got != 1 {
		t.Errorf("committed batches = %d, want 1", got)
	}
	if got := obs.Counter("watch_checkpoint_saves"); got != 2 {
		t.Errorf("checkpoint saves = %d, want 2 (source registration, acknowledgment)", got)
	}
	if got := obs.Counter("sqlite_outcomes", "ok"); got < 2 {
		t.Errorf("sqlite ok outcomes = %d, want at least 2 (register, ingest)", got)
	}
	if got := obs.UnknownAddCalls(); got != 0 {
		t.Errorf("%d Add calls used unknown metric names or label arity", got)
	}

	readsAfterImport := obs.Counter("watch_read_bytes")
	if err := runIngest(context.Background(), opts(obs)); err != nil {
		t.Fatal(err)
	}
	if got := obs.Counter("ingest_passes"); got != 2 {
		t.Errorf("ingest passes = %d, want 2", got)
	}
	if got := obs.Counter("ingest_events", "inserted"); got != 16 {
		t.Errorf("events inserted after the idle pass = %d, want the original 16", got)
	}
	if got := obs.Counter("watch_checkpoint_saves"); got != 2 {
		t.Errorf("checkpoint saves after the idle pass = %d, want 2: an idle pass rewrites no state", got)
	}
	if got := obs.Counter("watch_read_bytes") - readsAfterImport; got != 4096 {
		t.Errorf("restarted ingest read %d bytes, want exactly one 4096-byte fingerprint window: a fresh watcher re-verifies every source once",
			got)
	}
	if got := obs.Counter("parse_bytes"); got != size {
		t.Errorf("parse bytes after the idle pass = %d, want the unchanged %d", got, size)
	}
}

func TestIngestIdlePassSkipsRegistrationAndFingerprintReads(t *testing.T) {
	e := newSliceEnv(t)
	session := e.session("main-session.jsonl")
	copyFixture(t, session, "main-session.jsonl")
	obs := observertest.New()
	if err := os.MkdirAll(filepath.Dir(e.dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(e.stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	hub, err := store.Open(context.Background(), e.dbPath, store.WithObserver(obs))
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	deviceID, err := device.LoadOrCreate(e.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	w, err := watch.New(e.stateDir, claudecode.AgentName, watch.Options{Observer: obs})
	if err != nil {
		t.Fatal(err)
	}
	r := newIngestor(hub, w, claudecode.AgentName, deviceID, e.maxRecordBytes, nil, io.Discard, obs, nil)

	if _, err := r.poll(context.Background(), e.dirs(), false); err != nil {
		t.Fatal(err)
	}
	reads := obs.Counter("watch_read_bytes")
	okTx := obs.Counter("sqlite_outcomes", "ok")
	saves := obs.Counter("watch_checkpoint_saves")

	stats, err := r.poll(context.Background(), e.dirs(), false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.sources != 1 || stats.inserted != 0 || stats.failed != 0 {
		t.Fatalf("idle pass = %+v, want one quiet source", stats)
	}
	if got := obs.Counter("watch_read_bytes") - reads; got != 0 {
		t.Errorf("idle pass read %d bytes, want 0: the fingerprint read is skipped while identity, size, and modification time hold", got)
	}
	if got := obs.Counter("sqlite_outcomes", "ok") - okTx; got != 0 {
		t.Errorf("idle pass ran %d store transactions, want 0: an unchanged source is not re-registered", got)
	}
	if got := obs.Counter("watch_checkpoint_saves") - saves; got != 0 {
		t.Errorf("idle pass wrote the checkpoint %d times, want 0", got)
	}

	moved := e.session("renamed-session.jsonl")
	if err := os.Rename(session, moved); err != nil {
		t.Fatal(err)
	}
	stats, err = r.poll(context.Background(), e.dirs(), false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.sources != 1 || stats.failed != 0 {
		t.Fatalf("pass after the rename = %+v, want the moved source continued", stats)
	}
	if got := obs.Counter("sqlite_outcomes", "ok") - okTx; got != 1 {
		t.Errorf("pass after the rename ran %d store transactions, want 1: a path move re-registers the source", got)
	}
	if got := obs.Counter("watch_read_bytes") - reads; got != 0 {
		t.Errorf("pass after the rename read %d bytes, want 0: a move keeps the file unchanged", got)
	}
	var stored string
	if err := rawOpen(t, e.dbPath).QueryRow(`SELECT path FROM sources`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(stored, "renamed-session.jsonl") {
		t.Fatalf("stored source path = %q, want the renamed path", stored)
	}
}
