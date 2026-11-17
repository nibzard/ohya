package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nibzard/ohya/internal/observe/observertest"
)

func observedHub(t *testing.T) (*DB, *observertest.Recorder) {
	t.Helper()
	obs := observertest.New()
	hub, err := Open(context.Background(), filepath.Join(t.TempDir(), "hub.db"), WithObserver(obs))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hub.Close() })
	return hub, obs
}

func TestObserverCountsTransactionOutcomes(t *testing.T) {
	hub, obs := observedHub(t)
	mustRegister(t, hub, "s1", "/a.jsonl")
	mustIngest(t, hub, Batch{SourceID: "s1", Events: []Event{mkEvent(0, rawUser("one"))}})
	if got := obs.Counter("sqlite_outcomes", "ok"); got < 2 {
		t.Errorf("sqlite ok outcomes = %d, want at least 2 (register, ingest)", got)
	}
	if got := obs.DurationCount("sqlite_transaction_duration"); got < 2 {
		t.Errorf("transaction durations = %d, want at least 2", got)
	}
	if got := obs.UnknownAddCalls(); got != 0 {
		t.Errorf("%d Add calls used unknown metric names or label arity", got)
	}
}

func TestObserverCountsCommitFailure(t *testing.T) {
	hub, obs := observedHub(t)
	mustRegister(t, hub, "s1", "/a.jsonl")
	if _, err := hub.Writer().Exec(
		`CREATE TRIGGER inject_failure BEFORE INSERT ON events
		 BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
		t.Fatal(err)
	}
	b := Batch{SourceID: "s1", Events: []Event{mkEvent(0, rawUser("one"))}}
	b.DeviceID = deviceHex
	if _, err := hub.Ingest(context.Background(), b); err == nil {
		t.Fatal("ingest must fail under the injected trigger")
	}
	if got := obs.Counter("sqlite_outcomes", "error"); got != 1 {
		t.Errorf("sqlite error outcomes = %d, want 1", got)
	}
	if got := obs.DurationCount("sqlite_transaction_duration"); got != 2 {
		t.Errorf("transaction durations = %d, want 2 (register, rolled-back ingest)", got)
	}
}

func TestObserverSeparatesDomainRejectionsFromSqliteOutcomes(t *testing.T) {
	hub, obs := observedHub(t)
	b := Batch{SourceID: "missing", Events: []Event{mkEvent(0, rawUser("one"))}}
	b.DeviceID = deviceHex
	if _, err := hub.Ingest(context.Background(), b); err == nil {
		t.Fatal("ingest of an unregistered source must fail")
	}
	if got := obs.Counter("sqlite_outcomes", "ok"); got != 0 {
		t.Errorf("sqlite ok outcomes = %d, want 0: the rejection is not a SQLite outcome", got)
	}
	if got := obs.Counter("sqlite_outcomes", "error"); got != 0 {
		t.Errorf("sqlite error outcomes = %d, want 0: the rejection is a domain outcome", got)
	}
}

func TestIngestResultCountsRecognizedDuplicates(t *testing.T) {
	hub, _ := observedHub(t)
	mustRegister(t, hub, "orig", "/orig.jsonl")
	mustRegister(t, hub, "page", "/page.jsonl")

	o0 := ordinalEvent(0, 0, rawUser("zero"))
	o1 := ordinalEvent(o0.End, 1, rawUser("one"))
	if res := mustIngest(t, hub, Batch{SourceID: "orig", Events: []Event{o0, o1}}); res.Duplicates != 0 {
		t.Fatalf("original duplicates = %d, want 0", res.Duplicates)
	}

	p0 := mkEvent(0, rawUser("page meta"))
	p1 := ordinalEvent(p0.End, 1, o1.RawJSON)
	p1.Text = o1.Text
	p2 := ordinalEvent(p1.End, 2, rawUser("two"))
	res := mustIngest(t, hub, Batch{SourceID: "page", Events: []Event{p0, p1, p2}})
	if res.Inserted != 3 {
		t.Fatalf("page inserted %d rows, want 3 (duplicates are retained)", res.Inserted)
	}
	if res.Duplicates != 1 {
		t.Fatalf("page duplicates = %d, want 1 (the replayed ordinal)", res.Duplicates)
	}
}
