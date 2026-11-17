package watch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nibzard/ohya/internal/observe/observertest"
)

func TestObserverCountsSourceGaugesAndObservedBytes(t *testing.T) {
	dir := t.TempDir()
	obs := observertest.New()
	w, err := New(dir, agent, Options{Observer: obs})
	if err != nil {
		t.Fatal(err)
	}
	a := filepath.Join(dir, "a.jsonl")
	b := filepath.Join(dir, "b.jsonl")
	writeFile(t, a, strings.Repeat("x", 100))
	writeFile(t, b, strings.Repeat("y", 50))
	poll(t, w, a, b)
	if got := obs.Gauge("watch_sources", "active"); got != 2 {
		t.Errorf("active sources = %v, want 2", got)
	}
	if got := obs.Counter("watch_bytes_observed"); got != 150 {
		t.Errorf("watch_bytes_observed = %d, want 150", got)
	}
	if got := obs.UnknownAddCalls(); got != 0 {
		t.Errorf("%d Add calls used unknown metric names or label arity", got)
	}
}

func TestObserverCountsIdleReadsAndCheckpointSaves(t *testing.T) {
	dir := t.TempDir()
	obs := observertest.New()
	w, err := New(dir, agent, Options{Observer: obs})
	if err != nil {
		t.Fatal(err)
	}
	a := filepath.Join(dir, "a.jsonl")
	content := strings.Repeat("x", 4096)
	writeFile(t, a, content)
	outcomes := poll(t, w, a)
	if saves := obs.Counter("watch_checkpoint_saves"); saves != 1 {
		t.Fatalf("checkpoint saves after first poll = %d, want 1", saves)
	}
	if written := obs.Counter("watch_checkpoint_bytes_written"); written == 0 {
		t.Error("checkpoint bytes written must be counted")
	}
	if err := w.Acknowledge(a, outcomes[0].SourceID, int64(len(content))); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if saves := obs.Counter("watch_checkpoint_saves"); saves != 2 {
		t.Fatalf("checkpoint saves = %d, want 2 (registration, acknowledgment)", saves)
	}
	readsAfterAck := obs.Counter("watch_read_bytes")
	if readsAfterAck != 4096 {
		t.Fatalf("watch_read_bytes after acknowledgment window = %d, want 4096", readsAfterAck)
	}

	poll(t, w, a)
	if saves := obs.Counter("watch_checkpoint_saves"); saves != 2 {
		t.Errorf("idle poll must not save checkpoints again; saves = %d, want 2", saves)
	}
	if got := obs.Counter("watch_read_bytes"); got != readsAfterAck {
		t.Errorf("idle poll read %d bytes, want %d: unchanged identity, size, and modification time skip the fingerprint read",
			got-readsAfterAck, readsAfterAck)
	}

	if err := os.Chtimes(a, time.Now().Add(time.Second), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	poll(t, w, a)
	if got := obs.Counter("watch_read_bytes"); got != readsAfterAck+4096 {
		t.Errorf("poll after a modification-time change read %d bytes, want %d (one more window)",
			got-readsAfterAck, readsAfterAck+4096)
	}

	restarted, err := New(dir, agent, Options{Observer: obs})
	if err != nil {
		t.Fatal(err)
	}
	poll(t, restarted, a)
	if got := obs.Counter("watch_read_bytes"); got != readsAfterAck+8192 {
		t.Errorf("first poll of a restarted watcher read %d bytes, want %d (re-verify once)",
			got-readsAfterAck, readsAfterAck+8192)
	}
}

func TestObserverCountsPendingReadBytes(t *testing.T) {
	dir := t.TempDir()
	obs := observertest.New()
	w, err := New(dir, agent, Options{Observer: obs})
	if err != nil {
		t.Fatal(err)
	}
	a := filepath.Join(dir, "a.jsonl")
	writeFile(t, a, strings.Repeat("x", 300))
	poll(t, w, a)
	appendFile(t, a, strings.Repeat("y", 200))
	var pending Outcome
	for _, o := range poll(t, w, a) {
		if o.Path == a {
			pending = o
		}
	}
	before := obs.Counter("watch_read_bytes")
	data, err := w.ReadPending(pending)
	if err != nil {
		t.Fatal(err)
	}
	if got := obs.Counter("watch_read_bytes") - before; got != uint64(len(data)) {
		t.Errorf("watch_read_bytes delta = %d, want the %d pending bytes read", got, len(data))
	}
}

func TestObserverClassifiesWatchEvents(t *testing.T) {
	dir := t.TempDir()
	obs := observertest.New()
	w, err := New(dir, agent, Options{Observer: obs})
	if err != nil {
		t.Fatal(err)
	}
	a := filepath.Join(dir, "a.jsonl")
	writeFile(t, a, strings.Repeat("x", 100))
	outcomes := poll(t, w, a)
	if err := w.Acknowledge(a, outcomes[0].SourceID, 100); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}

	writeFile(t, a, strings.Repeat("z", 40))
	poll(t, w, a)
	if got := obs.Counter("watch_source_events", "truncation"); got != 1 {
		t.Errorf("truncation events = %d, want 1", got)
	}

	outcomes = poll(t, w, a)
	if err := w.Acknowledge(a, outcomes[0].SourceID, 40); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	replaceFile(t, a, strings.Repeat("q", 60))
	poll(t, w, a)
	if got := obs.Counter("watch_source_events", "replacement"); got != 1 {
		t.Errorf("replacement events = %d, want 1", got)
	}

	if err := os.Remove(a); err != nil {
		t.Fatal(err)
	}
	poll(t, w, a)
	if got := obs.Gauge("watch_sources", "missing"); got != 1 {
		t.Errorf("missing sources = %v, want 1", got)
	}
	if got := obs.Counter("watch_source_events", "missing"); got != 1 {
		t.Errorf("missing events = %d, want 1", got)
	}
}
