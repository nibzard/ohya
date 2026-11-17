package hubsync

import (
	"fmt"
	"testing"
	"time"

	"github.com/nibzard/ohya/internal/observe/observertest"
)

func TestObserverCountsHubBatchStages(t *testing.T) {
	db := newTestDB(t)
	obs := observertest.New()
	s := startEndpoint(t, db, nil, Options{Retry: RetryPolicy{Base: time.Millisecond}, Observer: obs})

	recs := fixtureRecords(t)
	d := dial(t, s.addr)
	mustRegisterVia(t, d, deviceA, "src-1", "/a.jsonl")
	d.send(fixtureBatch(deviceA, "src-1", recs))
	d.mustAck(Ack{Cursor: 6528, Inserted: 16})

	if got := obs.Counter("hub_connections", "admitted"); got < 1 {
		t.Errorf("admitted connections = %d, want at least 1", got)
	}
	if got := obs.Counter("hub_connections", "active"); got != 1 {
		t.Errorf("active connections = %d, want 1 authenticated", got)
	}
	if got := obs.Gauge("hub_active_connections"); got != 1 {
		t.Errorf("open connections gauge = %v, want 1", got)
	}
	if got := obs.DurationCount("hub_store_duration"); got != 2 {
		t.Errorf("store durations = %d, want 2 (registration, batch)", got)
	}
	if got := obs.DurationCount("hub_normalize_duration"); got != 1 {
		t.Errorf("normalize durations = %d, want 1", got)
	}
	if got := obs.DurationCount("hub_gate_wait_duration"); got != 1 {
		t.Errorf("gate waits = %d, want 1", got)
	}
	if !obs.AwaitDurationCount("hub_ack_write_duration", 1, 10*time.Second) {
		t.Errorf("ack writes = %d, want 1", obs.DurationCount("hub_ack_write_duration"))
	}
	if got := obs.Counter("parse_records", "attempted"); got != 16 {
		t.Errorf("records attempted = %d, want 16", got)
	}
	if got := obs.Counter("parse_records", "normalized"); got != 16 {
		t.Errorf("records normalized = %d, want 16", got)
	}
	if got := obs.Counter("parse_bytes"); got != 6528 {
		t.Errorf("parse bytes = %d, want 6528 (record bytes plus terminators)", got)
	}
	if got := obs.DurationCount("parse_duration", "claude-code"); got != 1 {
		t.Errorf("parse durations = %d, want 1", got)
	}
	if got := obs.UnknownAddCalls(); got != 0 {
		t.Errorf("%d Add calls used unknown metric names or label arity", got)
	}
}

func TestObserverCountsParseStop(t *testing.T) {
	db := newTestDB(t)
	obs := observertest.New()
	s := startEndpoint(t, db, nil, Options{Retry: RetryPolicy{Base: time.Millisecond}, Observer: obs})

	good := rawLine("sess-1", "commits")
	bad := []byte(`{"type":"user","sessionId":`)
	off1 := int64(len(good) + 1)
	batch := Batch{DeviceID: deviceA, SourceID: "src-1",
		Start: 0, End: off1 + int64(len(bad)) + 1,
		Records: []Record{{Offset: 0, JSON: good}, {Offset: off1, JSON: bad}}}

	d := dial(t, s.addr)
	mustRegisterVia(t, d, deviceA, "src-1", "/p/s.jsonl")
	d.send(batch)
	ack, perr := d.reply()
	if perr != nil || !ack.Stopped {
		t.Fatalf("stopped batch = %+v, %+v", ack, perr)
	}
	if got := obs.Counter("parse_records", "attempted"); got != 2 {
		t.Errorf("records attempted = %d, want 2", got)
	}
	if got := obs.Counter("parse_records", "normalized"); got != 1 {
		t.Errorf("records normalized = %d, want 1 (the prefix before the stop)", got)
	}
	if got := obs.Counter("parse_records", "stopped"); got != 1 {
		t.Errorf("stopped records = %d, want 1", got)
	}
	if got := obs.Counter("parse_stops", "invalid"); got != 1 {
		t.Errorf("invalid stops = %d, want 1", got)
	}
}

func TestObserverCountsCommitFailure(t *testing.T) {
	db := newTestDB(t)
	obs := observertest.New()
	s := startEndpoint(t, db, nil, Options{Retry: RetryPolicy{Base: time.Millisecond, Attempts: 2}, Observer: obs})

	l0 := rawLine("sess-1", "record zero")
	l1 := rawLine("sess-1", "record one")
	off1 := int64(len(l0) + 1)
	if _, err := db.Writer().Exec(fmt.Sprintf(
		`CREATE TRIGGER rollback_probe BEFORE INSERT ON events
		 WHEN NEW.source_offset = %d
		 BEGIN SELECT RAISE(ABORT, 'probe failure'); END`, off1)); err != nil {
		t.Fatal(err)
	}
	batch := Batch{DeviceID: deviceA, SourceID: "src-1",
		Start: 0, End: int64(len(l0) + 1 + len(l1) + 1),
		Records: []Record{{Offset: 0, JSON: l0}, {Offset: off1, JSON: l1}}}

	d := dial(t, s.addr)
	mustRegisterVia(t, d, deviceA, "src-1", "/p/s.jsonl")
	d.send(batch)
	d.wantError(CodeInternal, "probe failure")
	if got := obs.Counter("hub_failures", "commit"); got == 0 {
		t.Errorf("commit failures = %d, want at least 1 after the retry budget", got)
	}
	if got := obs.DurationCount("hub_store_duration"); got < 2 {
		t.Errorf("store durations = %d, want the registration and the failed batch attempt", got)
	}
	if got := queryInt(t, db, `SELECT cursor_bytes FROM sources WHERE id='src-1'`); got != 0 {
		t.Fatalf("cursor after rolled-back commit = %d, want 0", got)
	}
}

func TestObserverCountsBusyRejectionAndReadFailure(t *testing.T) {
	db := newTestDB(t)
	obs := observertest.New()
	s := startEndpoint(t, db, nil, Options{MaxConns: 1, Retry: RetryPolicy{Base: time.Millisecond}, Observer: obs})

	holder := dial(t, s.addr)
	mustRegisterVia(t, holder, deviceA, "src-1", "/a.jsonl")

	extra := dial(t, s.addr)
	extra.register(Registration{DeviceID: deviceA, SourceID: "src-2",
		Agent: "claude-code", Path: "/b.jsonl"})
	extra.wantError(CodeBusy, "connections")
	if got := obs.Counter("hub_connections", "rejected"); got != 1 {
		t.Errorf("rejected connections = %d, want 1 at the connection bound", got)
	}

	if _, err := holder.conn.Write([]byte{'X', ProtocolVersion}); err != nil {
		t.Fatal(err)
	}
	holder.wantError(CodeMalformed, "marker")
	holder.expectClosed()
	if got := obs.Counter("hub_failures", "read"); got != 1 {
		t.Errorf("read failures = %d, want 1 from the broken frame", got)
	}
}
