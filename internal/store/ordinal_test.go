package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
)

// ordinalEvent builds one event that carries a per-session ordinal.
func ordinalEvent(offset, ordinal int64, raw string) Event {
	e := mkEvent(offset, raw)
	e.Ordinal, e.HasOrdinal = ordinal, true
	return e
}

// eventRow reads the dedupe columns of one stored record.
type eventRow struct {
	ID          int64
	Ordinal     sql.NullInt64
	DuplicateOf sql.NullInt64
	Text        sql.NullString
}

func eventAt(t *testing.T, hub *DB, source string, offset int64) eventRow {
	t.Helper()
	var r eventRow
	if err := hub.Writer().QueryRow(
		`SELECT id, ordinal, duplicate_of, text FROM events WHERE source_id = ? AND source_offset = ?`,
		source, offset).Scan(&r.ID, &r.Ordinal, &r.DuplicateOf, &r.Text); err != nil {
		t.Fatalf("event %s@%d: %v", source, offset, err)
	}
	return r
}

// TestIngestDeduplicatesOrdinalsAcrossSources covers a resumed page that
// replays the tail of an earlier file of the same session: the replayed
// records are retained, marked as duplicates of the first copies, kept out of
// search, and add nothing to the aggregates or token totals.
func TestIngestDeduplicatesOrdinalsAcrossSources(t *testing.T) {
	hub := ingestHub(t)
	mustRegister(t, hub, "orig", "/orig.jsonl")
	mustRegister(t, hub, "page", "/page.jsonl")

	o0 := ordinalEvent(0, 0, rawUser("zero"))
	o1 := ordinalEvent(o0.End, 1, rawUser("one"))
	o1.HasUsage, o1.MessageID = true, "resp-1"
	o1.Usage = Usage{InputTokens: 10, OutputTokens: 2}
	orig := mustIngest(t, hub, Batch{SourceID: "orig", Events: []Event{o0, o1}})
	if orig.Inserted != 2 {
		t.Fatalf("original inserted %d, want 2", orig.Inserted)
	}

	// The page opens with a record without an ordinal, then replays
	// ordinal 1 (identical bytes, later time) and adds ordinal 2.
	p0 := mkEvent(0, rawUser("page meta"))
	p1 := ordinalEvent(p0.End, 1, o1.RawJSON)
	p1.Text = o1.Text
	p1.HasUsage, p1.MessageID, p1.Usage = true, "resp-1", o1.Usage
	p1.HasTime, p1.TimeMS = true, 9_000_000
	p2 := ordinalEvent(p1.End, 2, rawUser("two"))
	page := mustIngest(t, hub, Batch{SourceID: "page", Events: []Event{p0, p1, p2}})
	if page.Inserted != 3 {
		t.Fatalf("page inserted %d rows, want 3 (duplicates are retained)", page.Inserted)
	}

	first := eventAt(t, hub, "orig", o1.Offset)
	dup := eventAt(t, hub, "page", p1.Offset)
	if !dup.DuplicateOf.Valid || dup.DuplicateOf.Int64 != first.ID || dup.Text.Valid {
		t.Fatalf("replayed record = %+v; want a text-less duplicate of event %d", dup, first.ID)
	}
	if !dup.Ordinal.Valid || dup.Ordinal.Int64 != 1 {
		t.Fatalf("replayed record ordinal = %+v, want 1", dup.Ordinal)
	}
	if r := eventAt(t, hub, "page", p0.Offset); r.Ordinal.Valid || r.DuplicateOf.Valid {
		t.Fatalf("record without ordinal = %+v; want neither ordinal nor duplicate", r)
	}
	if r := eventAt(t, hub, "page", p2.Offset); r.DuplicateOf.Valid || !r.Text.Valid {
		t.Fatalf("new record = %+v; want an original with text", r)
	}

	// Search returns the replayed record once.
	var hits int
	if err := hub.Writer().QueryRow(
		`SELECT count(*) FROM events_fts WHERE events_fts MATCH ?`,
		fmt.Sprintf("%q", o1.Text)).Scan(&hits); err != nil {
		t.Fatal(err)
	}
	if hits != 1 {
		t.Fatalf("search hits for the replayed record = %d, want 1", hits)
	}
	if _, err := hub.Writer().Exec(`INSERT INTO events_fts(events_fts) VALUES ('integrity-check')`); err != nil {
		t.Fatalf("FTS integrity: %v", err)
	}

	// The duplicate counts nowhere: 2 + 2 originals, its later timestamp
	// does not widen the bounds, and the usage counts once.
	row := sessionRowFor(t, hub, SessionID(deviceHex, "claude-code", "sess-1"))
	if row.EventCount != 4 || row.Last.Valid {
		t.Fatalf("session = %+v; want 4 events and no timestamp", row)
	}
	var input int64
	if err := hub.Writer().QueryRow(`SELECT sum(input_tokens) FROM session_token_usage`).Scan(&input); err != nil || input != 10 {
		t.Fatalf("input tokens = %d, %v; want 10", input, err)
	}

	// A retry of the page replays its bytes; validation passes and nothing
	// new is stored.
	again := mustIngest(t, hub, Batch{SourceID: "page", Events: []Event{p0, p1, p2}})
	if again.Inserted != 0 || again.Cursor != p2.End {
		t.Fatalf("retry = %+v; want nothing new at cursor %d", again, p2.End)
	}

	// Arrival order decides which copy is the original: a fresh hub that
	// sees the page first marks the original file's copy instead.
	hub2 := ingestHub(t)
	mustRegister(t, hub2, "orig", "/orig.jsonl")
	mustRegister(t, hub2, "page", "/page.jsonl")
	mustIngest(t, hub2, Batch{SourceID: "page", Events: []Event{p0, p1, p2}})
	mustIngest(t, hub2, Batch{SourceID: "orig", Events: []Event{o0, o1}})
	if r := eventAt(t, hub2, "orig", o1.Offset); !r.DuplicateOf.Valid {
		t.Fatalf("late original = %+v; want a duplicate of the page's copy", r)
	}
}

func TestIngestOrdinalsAreScopedBySession(t *testing.T) {
	hub := ingestHub(t)
	mustRegister(t, hub, "a", "/a.jsonl")
	mustRegister(t, hub, "b", "/b.jsonl")
	a := ordinalEvent(0, 7, rawUser("a"))
	b := ordinalEvent(0, 7, rawUser("b"))
	b.NativeID = "sess-2"
	mustIngest(t, hub, Batch{SourceID: "a", Events: []Event{a}})
	mustIngest(t, hub, Batch{SourceID: "b", Events: []Event{b}})
	if r := eventAt(t, hub, "b", 0); r.DuplicateOf.Valid {
		t.Fatalf("same ordinal in another session = %+v; want an original", r)
	}

	// Within one batch, a repeated ordinal is a duplicate of the earlier
	// record.
	c1 := ordinalEvent(b.End, 8, rawUser("c"))
	c1.NativeID = "sess-2"
	c2 := ordinalEvent(c1.End, 8, rawUser("c"))
	c2.NativeID = "sess-2"
	mustIngest(t, hub, Batch{SourceID: "b", Events: []Event{c1, c2}})
	if r := eventAt(t, hub, "b", c2.Offset); r.DuplicateOf.Int64 != eventAt(t, hub, "b", c1.Offset).ID {
		t.Fatalf("repeat within a batch = %+v", r)
	}

	// A negative ordinal is a malformed batch.
	bad := ordinalEvent(c2.End, -1, rawUser("x"))
	bad.NativeID = "sess-2"
	_, err := hub.Ingest(context.Background(), Batch{SourceID: "b", DeviceID: deviceHex, Events: []Event{bad}})
	if !errors.Is(err, ErrBatchRejected) {
		t.Fatalf("negative ordinal error = %v", err)
	}
}

func TestSourceState(t *testing.T) {
	hub := ingestHub(t)
	ctx := context.Background()
	if _, err := hub.SourceState(ctx, "nope"); !errors.Is(err, ErrSourceNotRegistered) {
		t.Fatalf("unknown source error = %v", err)
	}
	if err := hub.RegisterSource(ctx, "src", deviceHex, "codex", "/r.jsonl"); err != nil {
		t.Fatal(err)
	}
	st, err := hub.SourceState(ctx, "src")
	if err != nil || st != (SourceState{Agent: "codex"}) {
		t.Fatalf("fresh source = %+v, %v", st, err)
	}
	e0 := mkEvent(0, rawUser("meta"))
	e0.NativeID = "thread-1"
	e1 := mkEvent(e0.End, rawUser("next"))
	e1.NativeID = "thread-1"
	mustIngest(t, hub, Batch{SourceID: "src", Events: []Event{e0, e1}})
	st, err = hub.SourceState(ctx, "src")
	if err != nil || st != (SourceState{Agent: "codex", Cursor: e1.End, FirstNativeID: "thread-1"}) {
		t.Fatalf("ingested source = %+v, %v", st, err)
	}
}
