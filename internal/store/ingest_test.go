package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nibzard/ohya/internal/claudecode"
)

// deviceHex is a well-formed local Device ID (see internal/device).
const deviceHex = "0123456789abcdef0123456789abcdef"

func ingestHub(t *testing.T) *DB {
	t.Helper()
	hub, err := Open(context.Background(), filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hub.Close() })
	return hub
}

func mustRegister(t *testing.T, hub *DB, id, path string) {
	t.Helper()
	if err := hub.RegisterSource(context.Background(), id, deviceHex, "claude-code", path); err != nil {
		t.Fatal(err)
	}
}

func mustIngest(t *testing.T, hub *DB, b Batch) IngestResult {
	t.Helper()
	b.DeviceID = deviceHex
	res, err := hub.Ingest(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// mustIngestAs submits a batch under a chosen device identity.
func mustIngestAs(t *testing.T, hub *DB, device string, b Batch) IngestResult {
	t.Helper()
	b.DeviceID = device
	res, err := hub.Ingest(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// rawUser builds one minimal user record; its JSON length is all the write
// path needs, and its content never reaches an error message.
func rawUser(content string) string {
	return fmt.Sprintf(`{"type":"user","sessionId":"sess-1","message":{"role":"user","content":%q}}`, content)
}

// mkEvent builds one synthetic event at offset spanning raw plus one
// terminator byte.
func mkEvent(offset int64, raw string) Event {
	return Event{
		Offset:   offset,
		End:      offset + int64(len(raw)) + 1,
		NativeID: "sess-1",
		Kind:     "user",
		Role:     "user",
		Text:     "text of the record at " + fmt.Sprint(offset),
		RawJSON:  raw,
	}
}

// parseFixture parses a Claude Code fixture with the real parser and maps
// the records onto Events, the way the ingestion wiring will. It returns the
// mapped events and the parser's stopping diagnostic, if any.
func parseFixture(t *testing.T, name, scope string) ([]Event, error) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "claude-code", name))
	if err != nil {
		t.Fatal(err)
	}
	res, perr := claudecode.Parse(data, 0, claudecode.Options{SourceScope: scope})
	var events []Event
	for _, r := range res.Records {
		events = append(events, eventFromRecord(r))
	}
	return events, perr
}

// eventFromRecord maps one parsed Claude Code record onto an Event: string
// user content is a prompt, and message.usage carries the four unambiguous
// token counts keyed by message.id.
func eventFromRecord(r claudecode.Record) Event {
	e := Event{
		Offset: r.Offset, End: r.End, NativeID: r.Native,
		Kind: r.Kind, Role: r.Role, ToolName: r.ToolName, Text: r.Text,
		RawJSON: r.Raw, Metadata: r.Metadata,
		HasTime: r.HasTime, TimeMS: r.TimeMS,
		Cwd: r.Cwd, GitBranch: r.GitBranch, Model: r.Model,
		MessageID: r.MessageID,
	}
	var md struct {
		ContentForm string `json:"contentForm"`
	}
	_ = json.Unmarshal([]byte(r.Metadata), &md)
	if r.Kind == "user" && md.ContentForm == "string" {
		e.Prompt = r.Text
	}
	if len(r.Usage) > 0 {
		var u struct {
			InputTokens              int64 `json:"input_tokens"`
			CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
			OutputTokens             int64 `json:"output_tokens"`
		}
		if err := json.Unmarshal(r.Usage, &u); err == nil {
			e.Usage = Usage{
				InputTokens:              u.InputTokens,
				CacheCreationInputTokens: u.CacheCreationInputTokens,
				CacheReadInputTokens:     u.CacheReadInputTokens,
				OutputTokens:             u.OutputTokens,
			}
			e.HasUsage = true
		}
	}
	return e
}

// sessionRow reads one sessions row for aggregate assertions.
type sessionRow struct {
	EventCount                      int
	Started, Last                   sql.NullInt64
	Cwd, Branch, Model, FirstPrompt sql.NullString
	Metadata                        string
}

func sessionRowFor(t *testing.T, hub *DB, id string) sessionRow {
	t.Helper()
	var row sessionRow
	err := hub.Writer().QueryRow(
		`SELECT event_count, started_at_ms, last_event_at_ms, cwd, branch, model, first_prompt, metadata
		 FROM sessions WHERE id = ?`, id).
		Scan(&row.EventCount, &row.Started, &row.Last, &row.Cwd, &row.Branch,
			&row.Model, &row.FirstPrompt, &row.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

// tokenTotals sums one session's counted usage.
func tokenTotals(t *testing.T, hub *DB, sid string) Usage {
	t.Helper()
	var u Usage
	err := hub.Writer().QueryRow(
		`SELECT coalesce(sum(input_tokens), 0), coalesce(sum(cache_creation_input_tokens), 0),
		        coalesce(sum(cache_read_input_tokens), 0), coalesce(sum(output_tokens), 0)
		 FROM session_token_usage WHERE session_id = ?`, sid).
		Scan(&u.InputTokens, &u.CacheCreationInputTokens, &u.CacheReadInputTokens, &u.OutputTokens)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func countEvents(t *testing.T, hub *DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := hub.Writer().QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func wantError(t *testing.T, err error, sub string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), sub) {
		t.Fatalf("error = %v; want one containing %q", err, sub)
	}
}

const fixtureNativeID = "00000000-0000-4000-8000-0000000000a1"

func fixtureSessionID() string {
	return SessionID(deviceHex, "claude-code", fixtureNativeID)
}

func TestRegisterSource(t *testing.T) {
	hub := ingestHub(t)
	ctx := context.Background()
	for _, tc := range []struct {
		id, device, agent, path, want string
	}{
		{"", deviceHex, "claude-code", "/a.jsonl", "empty source ID"},
		{"s1", "", "claude-code", "/a.jsonl", "empty device ID"},
		{"s1", deviceHex, "Claude Code", "/a.jsonl", "lowercase slug"},
		{"s1", deviceHex, "claude-code", "", "empty path"},
	} {
		err := hub.RegisterSource(ctx, tc.id, tc.device, tc.agent, tc.path)
		wantError(t, err, tc.want)
	}
	mustRegister(t, hub, "s1", "/a.jsonl")

	// Re-registration is idempotent and only follows a path move.
	if err := hub.RegisterSource(ctx, "s1", deviceHex, "claude-code", "/moved.jsonl"); err != nil {
		t.Fatal(err)
	}
	var path string
	if err := hub.Writer().QueryRow(`SELECT path FROM sources WHERE id='s1'`).Scan(&path); err != nil || path != "/moved.jsonl" {
		t.Fatalf("path after move = %q, %v; want /moved.jsonl", path, err)
	}

	// Ownership never moves.
	err := hub.RegisterSource(ctx, "s1", "ffffffffffffffffffffffffffffffff", "claude-code", "/a.jsonl")
	wantError(t, err, "ownership cannot change")
	if !errors.Is(err, ErrSourceOwnership) {
		t.Fatalf("ownership error = %v; want ErrSourceOwnership", err)
	}
	err = hub.RegisterSource(ctx, "s1", deviceHex, "codex", "/a.jsonl")
	wantError(t, err, "held for agent")
	if !errors.Is(err, ErrSourceOwnership) {
		t.Fatalf("agent-change error = %v; want ErrSourceOwnership", err)
	}
}

// TestIngestBindsBatchToDevice pins the sync contract's ownership rule at the
// write path: a batch naming another device is rejected with
// ErrSourceOwnership before anything is written, and rejection classes are
// distinguishable from internal failures through their sentinels.
func TestIngestBindsBatchToDevice(t *testing.T) {
	hub := ingestHub(t)
	mustRegister(t, hub, "s1", "/a.jsonl")
	other := "ffffffffffffffffffffffffffffffff"
	_, err := hub.Ingest(context.Background(), Batch{
		SourceID: "s1", DeviceID: other, Events: []Event{mkEvent(0, rawUser("foreign upload"))},
	})
	if !errors.Is(err, ErrSourceOwnership) {
		t.Fatalf("foreign batch error = %v; want ErrSourceOwnership", err)
	}
	wantError(t, err, "batch claims device")
	if n := countEvents(t, hub, `SELECT count(*) FROM events`); n != 0 {
		t.Fatalf("events after foreign batch = %d, want 0", n)
	}
	// An unregistered source is its own rejection class, not an internal
	// failure: the uploader must register the source before its first batch.
	_, err = hub.Ingest(context.Background(), Batch{SourceID: "ghost", DeviceID: other})
	if !errors.Is(err, ErrSourceNotRegistered) {
		t.Fatalf("unregistered error = %v; want ErrSourceNotRegistered", err)
	}
	_, err = hub.Ingest(context.Background(), Batch{SourceID: "s1", DeviceID: other, Events: []Event{
		mkEvent(500, rawUser("skips bytes")),
	}})
	if !errors.Is(err, ErrSourceOwnership) {
		t.Fatalf("gap under foreign device = %v; want ErrSourceOwnership (identity first)", err)
	}
	// The owning device still commits after the rejected foreign attempts.
	if res := mustIngest(t, hub, Batch{SourceID: "s1", Events: []Event{mkEvent(0, rawUser("owner upload"))}}); res.Inserted != 1 {
		t.Fatalf("owner batch = %+v; want 1 insert", res)
	}
}

// TestIngestFixtureMainSession drives the write path with the real fixture:
// raw text is preserved verbatim, aggregates and token totals land in one
// commit, FTS follows the trigger, and readers see the commit.
func TestIngestFixtureMainSession(t *testing.T) {
	hub := ingestHub(t)
	mustRegister(t, hub, "aaa-main", "/main-session.jsonl")
	events, perr := parseFixture(t, "main-session.jsonl", "aaa-main")
	if perr != nil {
		t.Fatalf("fixture did not parse cleanly: %v", perr)
	}
	if len(events) != 16 {
		t.Fatalf("fixture mapped to %d events, want 16", len(events))
	}
	res := mustIngest(t, hub, Batch{SourceID: "aaa-main", Events: events})
	if res.Inserted != 16 || res.Cursor != 6528 {
		t.Fatalf("ingest = {inserted %d, cursor %d}; want {16, 6528}", res.Inserted, res.Cursor)
	}

	var cursor, updated int64
	if err := hub.Writer().QueryRow(
		`SELECT cursor_bytes, updated_at_ms FROM sources WHERE id='aaa-main'`).Scan(&cursor, &updated); err != nil {
		t.Fatal(err)
	}
	if cursor != 6528 {
		t.Fatalf("stored cursor = %d, want 6528", cursor)
	}
	if updated == 0 {
		t.Fatal("cursor advance did not stamp updated_at_ms")
	}
	if n := countEvents(t, hub, `SELECT count(*) FROM events`); n != 16 {
		t.Fatalf("events = %d, want 16", n)
	}

	// The original record text survives byte for byte (line 5, offset 1086).
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "claude-code", "main-session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := hub.Writer().QueryRow(
		`SELECT raw_json FROM events WHERE source_id='aaa-main' AND source_offset=1086`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != strings.Split(string(data), "\n")[4] {
		t.Fatal("raw at offset 1086 differs from the fixture line")
	}

	row := sessionRowFor(t, hub, fixtureSessionID())
	if row.EventCount != 16 {
		t.Fatalf("event_count = %d, want 16", row.EventCount)
	}
	// Bounds are observed min/max over timestamps that are not strictly
	// monotonic (09:00:03.1 and 09:00:03.2 arrive before 09:00:04).
	if !row.Started.Valid || row.Started.Int64 != 1794560400000 {
		t.Fatalf("started_at_ms = %+v, want 1794560400000 (09:00:00.000Z)", row.Started)
	}
	if !row.Last.Valid || row.Last.Int64 != 1794560404000 {
		t.Fatalf("last_event_at_ms = %+v, want 1794560404000 (09:00:04.000Z)", row.Last)
	}
	if !row.Cwd.Valid || row.Cwd.String != "/home/dev/fixture-project" {
		t.Fatalf("cwd = %+v, want /home/dev/fixture-project", row.Cwd)
	}
	if !row.Branch.Valid || row.Branch.String != "main" {
		t.Fatalf("branch = %+v, want main", row.Branch)
	}
	if !row.Model.Valid || row.Model.String != "fixture-model-v1" {
		t.Fatalf("model = %+v, want fixture-model-v1", row.Model)
	}
	if !row.FirstPrompt.Valid || row.FirstPrompt.String != "Find the highest prime below 20." {
		t.Fatalf("first_prompt = %+v, want the fixture prompt", row.FirstPrompt)
	}

	// msg_fixture_0001 repeats one usage over three records and
	// msg_fixture_0002 contributes its own: each response counts once.
	if want := (Usage{InputTokens: 180, CacheReadInputTokens: 428, OutputTokens: 47}); tokenTotals(t, hub, fixtureSessionID()) != want {
		t.Fatalf("token totals = %+v, want %+v", tokenTotals(t, hub, fixtureSessionID()), want)
	}
	if n := countEvents(t, hub,
		`SELECT count(*) FROM session_token_usage WHERE session_id = ?`, fixtureSessionID()); n != 2 {
		t.Fatalf("usage rows = %d, want 2 (one per message id)", n)
	}

	// FTS carries the indexed text and stays consistent with the events
	// table: MATCH finds exactly the events whose text holds the term.
	if _, err := hub.Writer().Exec(`INSERT INTO events_fts(events_fts) VALUES ('integrity-check')`); err != nil {
		t.Fatalf("FTS integrity-check failed: %v", err)
	}
	readers, err := hub.Reader()
	if err != nil {
		t.Fatal(err)
	}
	defer readers.Close()
	var hits, containing int
	if err := readers.QueryRow(
		`SELECT count(*) FROM events_fts WHERE events_fts MATCH 'prime'`).Scan(&hits); err != nil {
		t.Fatal(err)
	}
	if err := readers.QueryRow(
		`SELECT count(*) FROM events e WHERE text LIKE '%prime%'`).Scan(&containing); err != nil {
		t.Fatal(err)
	}
	if hits == 0 || hits != containing {
		t.Fatalf("MATCH prime = %d, events containing prime = %d; want equal and non-zero", hits, containing)
	}
}

// TestIngestCountsUsageOncePerMessageID pins the fixture-verified usage
// semantics: usage repeats identically on every record sharing message.id,
// splits across batches, and a later conflicting usage is ignored.
func TestIngestCountsUsageOncePerMessageID(t *testing.T) {
	hub := ingestHub(t)
	mustRegister(t, hub, "s1", "/a.jsonl")
	events, perr := parseFixture(t, "main-session.jsonl", "s1")
	if perr != nil {
		t.Fatal(perr)
	}
	// Lines 1-4 carry no usage; line 5 is the first record of
	// msg_fixture_0001. Incremental polling splits one response's records
	// across batches while it streams.
	mustIngest(t, hub, Batch{SourceID: "s1", Events: events[:4]})
	if got := tokenTotals(t, hub, fixtureSessionID()); got != (Usage{}) {
		t.Fatalf("totals before any usage = %+v", got)
	}
	mustIngest(t, hub, Batch{SourceID: "s1", Events: events[4:5]})
	if got := tokenTotals(t, hub, fixtureSessionID()); got != (Usage{InputTokens: 120, CacheReadInputTokens: 48, OutputTokens: 35}) {
		t.Fatalf("totals after the first record of the response = %+v", got)
	}
	// Lines 6 and 7 repeat the same usage under the same message id.
	mustIngest(t, hub, Batch{SourceID: "s1", Events: events[5:6]})
	mustIngest(t, hub, Batch{SourceID: "s1", Events: events[6:7]})
	if got := tokenTotals(t, hub, fixtureSessionID()); got != (Usage{InputTokens: 120, CacheReadInputTokens: 48, OutputTokens: 35}) {
		t.Fatalf("repeated usage was counted twice: %+v", got)
	}
	// The rest of the file contributes msg_fixture_0002.
	mustIngest(t, hub, Batch{SourceID: "s1", Events: events[7:]})
	if want := (Usage{InputTokens: 180, CacheReadInputTokens: 428, OutputTokens: 47}); tokenTotals(t, hub, fixtureSessionID()) != want {
		t.Fatalf("totals = %+v, want %+v", tokenTotals(t, hub, fixtureSessionID()), want)
	}

	// A later record claiming the same message id with inflated usage is
	// ignored: the first usage observed for a message id wins.
	conflict := mkEvent(6528, rawUser("repeated response"))
	conflict.NativeID = fixtureNativeID
	conflict.MessageID = "msg_fixture_0001"
	conflict.HasUsage = true
	conflict.Usage = Usage{InputTokens: 9999, OutputTokens: 9999}
	if res := mustIngest(t, hub, Batch{SourceID: "s1", Events: []Event{conflict}}); res.Inserted != 1 {
		t.Fatalf("inserted = %d, want 1", res.Inserted)
	}
	if want := (Usage{InputTokens: 180, CacheReadInputTokens: 428, OutputTokens: 47}); tokenTotals(t, hub, fixtureSessionID()) != want {
		t.Fatalf("conflicting usage changed the totals: %+v", tokenTotals(t, hub, fixtureSessionID()))
	}

	// Usage without a message id is ambiguous and contributes nothing.
	anonymous := mkEvent(conflict.End, rawUser("anonymous usage"))
	anonymous.NativeID = fixtureNativeID
	anonymous.HasUsage = true
	anonymous.Usage = Usage{InputTokens: 7}
	mustIngest(t, hub, Batch{SourceID: "s1", Events: []Event{anonymous}})
	if got := tokenTotals(t, hub, fixtureSessionID()); got.InputTokens != 180 {
		t.Fatalf("usage without a message id was summed: %+v", got)
	}
}

// TestIngestReplayAndOverlap covers lost acknowledgments: a fully repeated
// batch changes nothing, a partially repeated batch inserts only its new
// suffix, and a conflicting retry is rejected before anything is written.
func TestIngestReplayAndOverlap(t *testing.T) {
	hub := ingestHub(t)
	mustRegister(t, hub, "s1", "/a.jsonl")
	e0 := mkEvent(0, rawUser("hello"))
	e1 := mkEvent(e0.End, rawUser("world"))
	e2 := mkEvent(e1.End, rawUser("again"))
	batch := Batch{SourceID: "s1", Events: []Event{e0, e1}}
	if res := mustIngest(t, hub, batch); res.Inserted != 2 || res.Cursor != e1.End {
		t.Fatalf("first ingest = %+v", res)
	}
	// An empty batch reports the cursor and writes nothing.
	if res := mustIngest(t, hub, Batch{SourceID: "s1"}); res.Inserted != 0 || res.Cursor != e1.End {
		t.Fatalf("empty batch = %+v; want {0, %d}", res, e1.End)
	}

	// Full replay after a lost acknowledgment: no new records, no aggregate
	// change, same cursor.
	if res := mustIngest(t, hub, batch); res.Inserted != 0 || res.Cursor != e1.End {
		t.Fatalf("replay = %+v; want {0, %d}", res, e1.End)
	}
	sid := SessionID(deviceHex, "claude-code", "sess-1")
	if row := sessionRowFor(t, hub, sid); row.EventCount != 2 {
		t.Fatalf("event_count after replay = %d, want 2", row.EventCount)
	}

	// Overlapping retry carries the old bytes plus one new record.
	overlap := Batch{SourceID: "s1", Events: []Event{e0, e1, e2}}
	if res := mustIngest(t, hub, overlap); res.Inserted != 1 || res.Cursor != e2.End {
		t.Fatalf("overlapping retry = %+v; want {1, %d}", res, e2.End)
	}
	if row := sessionRowFor(t, hub, sid); row.EventCount != 3 {
		t.Fatalf("event_count after overlap = %d, want 3", row.EventCount)
	}

	// Retries whose replayed bytes differ are rejected and leave everything
	// untouched: same length with different bytes, and a different length.
	conflicts := []Batch{
		{SourceID: "s1", Events: []Event{
			mkEvent(0, rawUser("jello")), mkEvent(e0.End, rawUser("world")),
			mkEvent(e1.End, rawUser("fresh tail")),
		}},
		{SourceID: "s1", Events: []Event{mkEvent(0, `{"type":"user","sessionId":"sess-1","x":1}`)}},
	}
	for _, conflicting := range conflicts {
		conflicting.DeviceID = deviceHex
		_, err := hub.Ingest(context.Background(), conflicting)
		wantError(t, err, "conflicting retry at offset 0")
		if n := countEvents(t, hub, `SELECT count(*) FROM events`); n != 3 {
			t.Fatalf("events after rejected retry = %d, want 3", n)
		}
		var cursor int64
		if err := hub.Writer().QueryRow(`SELECT cursor_bytes FROM sources WHERE id='s1'`).Scan(&cursor); err != nil || cursor != e2.End {
			t.Fatalf("cursor after rejected retry = %d, %v; want %d", cursor, err, e2.End)
		}
		if row := sessionRowFor(t, hub, sid); row.EventCount != 3 {
			t.Fatalf("event_count after rejected retry = %d, want 3", row.EventCount)
		}
	}
}

// TestIngestRejectsGapsAndBadShape proves bytes are never skipped silently:
// after a source stops at a malformed offset, a batch that starts past that
// offset is rejected. malformed-line.jsonl stops at 1094 with a diagnostic.
func TestIngestRejectsGapsAndBadShape(t *testing.T) {
	hub := ingestHub(t)
	mustRegister(t, hub, "s1", "/malformed-line.jsonl")
	events, perr := parseFixture(t, "malformed-line.jsonl", "s1")
	var parseStop *claudecode.ParseError
	if !errors.As(perr, &parseStop) || parseStop.Offset != 1094 {
		t.Fatalf("parse error = %v; want stop at offset 1094", perr)
	}
	if len(events) != 2 {
		t.Fatalf("valid prefix = %d records, want 2", len(events))
	}
	res := mustIngest(t, hub, Batch{SourceID: "s1", Events: events})
	if res.Cursor != 1094 {
		t.Fatalf("cursor after valid prefix = %d, want 1094 (the offending offset)", res.Cursor)
	}

	// The valid record after the malformed line starts at 1507. Ingesting it
	// would skip the malformed bytes; the write path must refuse.
	valid := mkEvent(1507, rawUser("valid after the malformed line"))
	_, err := hub.Ingest(context.Background(), Batch{SourceID: "s1", DeviceID: deviceHex, Events: []Event{valid}})
	wantError(t, err, "bytes would be skipped")
	if n := countEvents(t, hub, `SELECT count(*) FROM events`); n != 2 {
		t.Fatalf("events after gap rejection = %d, want 2", n)
	}

	// Shape validation: non-contiguous delivery, wrong byte boundaries,
	// empty JSON text and a missing native identity all fail before any
	// write.
	good := mkEvent(1094, rawUser("fine"))
	for _, tc := range []struct {
		name  string
		event Event
		want  string
	}{
		{"non-contiguous", mkEvent(good.End+10, rawUser("gap inside batch")), "delivery must be contiguous"},
		{"bad boundary", func() Event {
			e := mkEvent(1094, rawUser("bounds"))
			e.End += 5
			return e
		}(), "plus one terminator"},
		{"empty json", func() Event {
			e := mkEvent(1094, "")
			e.End = 1095
			return e
		}(), "has no JSON text"},
		{"no native id", func() Event {
			e := mkEvent(1094, rawUser("identity"))
			e.NativeID = ""
			return e
		}(), "no native session ID"},
	} {
		_, err := hub.Ingest(context.Background(), Batch{SourceID: "s1", DeviceID: deviceHex, Events: []Event{good, tc.event}})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v; want %q", tc.name, err, tc.want)
		}
		if n := countEvents(t, hub, `SELECT count(*) FROM events`); n != 2 {
			t.Fatalf("%s: events = %d, want 2 (nothing written)", tc.name, n)
		}
	}

	// An unregistered source is an error, not an implicit registration.
	_, err = hub.Ingest(context.Background(), Batch{SourceID: "ghost", DeviceID: deviceHex})
	wantError(t, err, "not registered")
}

// TestIngestOtherSourcesContinue checks that one stopped source's rejection
// never blocks another source's commit.
func TestIngestOtherSourcesContinue(t *testing.T) {
	hub := ingestHub(t)
	mustRegister(t, hub, "a", "/a.jsonl")
	mustRegister(t, hub, "b", "/b.jsonl")
	first := mkEvent(0, rawUser("a first"))
	mustIngest(t, hub, Batch{SourceID: "a", Events: []Event{first}})

	skipping := mkEvent(500, rawUser("a skips bytes"))
	_, err := hub.Ingest(context.Background(), Batch{SourceID: "a", DeviceID: deviceHex, Events: []Event{skipping}})
	wantError(t, err, "bytes would be skipped")

	other := mkEvent(0, rawUser("b first"))
	if res := mustIngest(t, hub, Batch{SourceID: "b", Events: []Event{other}}); res.Inserted != 1 {
		t.Fatalf("source b after source a's rejection: %+v", res)
	}
}

func TestIngestTimestampBounds(t *testing.T) {
	hub := ingestHub(t)
	mustRegister(t, hub, "s1", "/a.jsonl")
	at := func(offset int64, raw string, ms int64, hasTime bool) Event {
		e := mkEvent(offset, raw)
		e.HasTime, e.TimeMS = hasTime, ms
		return e
	}
	// Batch 1: no timestamp, then 200, then 100 — bounds are min/max, not
	// first/last.
	e0 := at(0, rawUser("one"), 0, false)
	e1 := at(e0.End, rawUser("two"), 200, true)
	e2 := at(e1.End, rawUser("three"), 100, true)
	mustIngest(t, hub, Batch{SourceID: "s1", Events: []Event{e0, e1, e2}})
	sid := SessionID(deviceHex, "claude-code", "sess-1")
	row := sessionRowFor(t, hub, sid)
	if !row.Started.Valid || row.Started.Int64 != 100 || !row.Last.Valid || row.Last.Int64 != 200 {
		t.Fatalf("bounds = [%+v, %+v]; want [100, 200]", row.Started, row.Last)
	}

	// Batch 2: records without timestamps never touch known bounds.
	e3 := at(e2.End, rawUser("four"), 0, false)
	mustIngest(t, hub, Batch{SourceID: "s1", Events: []Event{e3}})
	row = sessionRowFor(t, hub, sid)
	if !row.Started.Valid || row.Started.Int64 != 100 || !row.Last.Valid || row.Last.Int64 != 200 {
		t.Fatalf("bounds after missing timestamps = [%+v, %+v]; want unchanged [100, 200]", row.Started, row.Last)
	}

	// Batch 3: wider observations widen the bounds.
	e4 := at(e3.End, rawUser("five"), 50, true)
	e5 := at(e4.End, rawUser("six"), 400, true)
	mustIngest(t, hub, Batch{SourceID: "s1", Events: []Event{e4, e5}})
	row = sessionRowFor(t, hub, sid)
	if !row.Started.Valid || row.Started.Int64 != 50 || !row.Last.Valid || row.Last.Int64 != 400 {
		t.Fatalf("bounds after widening = [%+v, %+v]; want [50, 400]", row.Started, row.Last)
	}

	// A fresh session keeps NULL bounds until an observation arrives.
	other := mkEvent(e5.End, rawUser("new session"))
	other.NativeID = "sess-2"
	mustIngest(t, hub, Batch{SourceID: "s1", Events: []Event{other}})
	row = sessionRowFor(t, hub, SessionID(deviceHex, "claude-code", "sess-2"))
	if row.Started.Valid || row.Last.Valid {
		t.Fatalf("new session bounds = [%+v, %+v]; want NULL", row.Started, row.Last)
	}
}

// rawOfLen builds one valid JSON record of exactly n bytes, for placing a
// record at a chosen offset.
func rawOfLen(n int) string {
	pad := n - len(`{"pad":""}`)
	if pad < 0 {
		pad = 0
	}
	return `{"pad":"` + strings.Repeat("x", pad) + `"}`
}

// TestIngestFirstPromptTieBreak pins the deterministic selection: the prompt
// with the smallest (source_id, source_offset) wins, whatever the arrival
// order, and only marked prompts compete.
func TestIngestFirstPromptTieBreak(t *testing.T) {
	// Source aaa's prompt sits at a larger offset than source bbb's, but aaa
	// orders first by source ID — a subagent file's earlier prompt must not
	// steal the main session's first prompt by arriving first.
	aaaFiller := mkEvent(0, rawOfLen(300))
	mainPrompt := mkEvent(aaaFiller.End, rawUser("prompt from source aaa"))
	mainPrompt.Prompt = mainPrompt.Text
	bbbFiller := mkEvent(0, rawOfLen(100))
	subPrompt := mkEvent(bbbFiller.End, rawUser("prompt from source bbb"))
	subPrompt.Prompt = subPrompt.Text
	aaa := Batch{SourceID: "aaa", Events: []Event{aaaFiller, mainPrompt}}
	bbb := Batch{SourceID: "bbb", Events: []Event{bbbFiller, subPrompt}}

	run := func(first, second Batch) string {
		hub := ingestHub(t)
		mustRegister(t, hub, "aaa", "/main.jsonl")
		mustRegister(t, hub, "bbb", "/sub.jsonl")
		mustIngest(t, hub, first)
		mustIngest(t, hub, second)
		row := sessionRowFor(t, hub, SessionID(deviceHex, "claude-code", "sess-1"))
		if !row.FirstPrompt.Valid {
			t.Fatal("first_prompt stayed NULL")
		}
		var md struct {
			FirstPromptRef struct {
				SourceID     string `json:"sourceId"`
				SourceOffset int64  `json:"sourceOffset"`
			} `json:"firstPromptRef"`
		}
		if err := json.Unmarshal([]byte(row.Metadata), &md); err != nil {
			t.Fatal(err)
		}
		if md.FirstPromptRef.SourceID != "aaa" || md.FirstPromptRef.SourceOffset != aaaFiller.End {
			t.Fatalf("firstPromptRef = %+v; want source aaa at offset %d", md.FirstPromptRef, aaaFiller.End)
		}
		return row.FirstPrompt.String
	}
	// aaa orders before bbb, so its prompt wins regardless of arrival order.
	if got := run(bbb, aaa); got != mainPrompt.Text {
		t.Fatalf("subagent-first order picked %q", got)
	}
	if got := run(aaa, bbb); got != mainPrompt.Text {
		t.Fatalf("main-first order picked %q", got)
	}

	// Within one source, offsets only grow, so the first prompt stays.
	hub := ingestHub(t)
	mustRegister(t, hub, "aaa", "/main.jsonl")
	first := mkEvent(0, rawUser("the first prompt"))
	first.Prompt = first.Text
	mustIngest(t, hub, Batch{SourceID: "aaa", Events: []Event{first}})
	second := mkEvent(first.End, rawUser("the second prompt"))
	second.Prompt = second.Text
	mustIngest(t, hub, Batch{SourceID: "aaa", Events: []Event{second}})
	row := sessionRowFor(t, hub, SessionID(deviceHex, "claude-code", "sess-1"))
	if !row.FirstPrompt.Valid || row.FirstPrompt.String != first.Text {
		t.Fatalf("first_prompt = %+v; want the first prompt", row.FirstPrompt)
	}

	// Non-prompt records never set it.
	hub2 := ingestHub(t)
	mustRegister(t, hub2, "ccc", "/c.jsonl")
	toolResult := mkEvent(0, rawUser("a tool result, not a prompt"))
	mustIngest(t, hub2, Batch{SourceID: "ccc", Events: []Event{toolResult}})
	row = sessionRowFor(t, hub2, SessionID(deviceHex, "claude-code", "sess-1"))
	if row.FirstPrompt.Valid {
		t.Fatalf("first_prompt = %q from a non-prompt record", row.FirstPrompt.String)
	}
}

// TestIngestInterruptedCommitRollsBack breaks a batch mid-transaction: the
// second event violates the metadata JSON check after the first inserted
// cleanly, so nothing may survive.
func TestIngestInterruptedCommitRollsBack(t *testing.T) {
	hub := ingestHub(t)
	mustRegister(t, hub, "s1", "/a.jsonl")
	good := mkEvent(0, rawUser("commits"))
	bad := mkEvent(good.End, rawUser("breaks"))
	bad.Metadata = "{not json"
	_, err := hub.Ingest(context.Background(), Batch{SourceID: "s1", DeviceID: deviceHex, Events: []Event{good, bad}})
	if err == nil {
		t.Fatal("invalid metadata was accepted")
	}
	if n := countEvents(t, hub, `SELECT count(*) FROM events`); n != 0 {
		t.Fatalf("events after failed commit = %d, want 0", n)
	}
	var cursor int64
	if err := hub.Writer().QueryRow(`SELECT cursor_bytes FROM sources WHERE id='s1'`).Scan(&cursor); err != nil || cursor != 0 {
		t.Fatalf("cursor after failed commit = %d, %v; want 0", cursor, err)
	}
	if n := countEvents(t, hub, `SELECT count(*) FROM sessions WHERE event_count > 0`); n != 0 {
		t.Fatal("a session kept an aggregate from a failed commit")
	}

	// A canceled context is the same interruption from outside.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := hub.Ingest(ctx, Batch{SourceID: "s1", DeviceID: deviceHex, Events: []Event{good}}); err == nil {
		t.Fatal("canceled context was accepted")
	}
	if n := countEvents(t, hub, `SELECT count(*) FROM events`); n != 0 {
		t.Fatalf("events after canceled ingest = %d, want 0", n)
	}

	// The same bytes then commit whole.
	bad.Metadata = ""
	if res := mustIngest(t, hub, Batch{SourceID: "s1", Events: []Event{good, bad}}); res.Inserted != 2 {
		t.Fatalf("retry after interruption = %+v; want 2 inserts", res)
	}
}

// TestIngestSurvivesCrashWithOpenTransaction kills a helper process holding
// an open write transaction, then reopens: SQLite recovery discards the
// uncommitted writes, and the same bytes ingest exactly once afterwards.
func TestIngestSurvivesCrashWithOpenTransaction(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "crash.db")
	hub, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	mustRegister(t, hub, "crash-src", filepath.Join(dir, "log.jsonl"))
	if err := hub.Close(); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestIngestCrashHelper")
	cmd.Env = append(os.Environ(), "OHYA_CRASH_DB="+path)
	if err := cmd.Run(); !errors.As(err, new(*exec.ExitError)) {
		t.Fatalf("helper process did not crash as intended: %v", err)
	}

	hub, err = Open(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen after crash: %v", err)
	}
	t.Cleanup(func() { hub.Close() })
	if n := countEvents(t, hub, `SELECT count(*) FROM events`); n != 0 {
		t.Fatalf("uncommitted events survived the crash: %d", n)
	}
	var cursor int64
	if err := hub.Writer().QueryRow(
		`SELECT cursor_bytes FROM sources WHERE id='crash-src'`).Scan(&cursor); err != nil || cursor != 0 {
		t.Fatalf("cursor after crash = %d, %v; want 0", cursor, err)
	}

	// The interrupted bytes commit exactly once on retry.
	e0 := mkEvent(0, rawUser("crash one"))
	e1 := mkEvent(e0.End, rawUser("crash two"))
	res := mustIngest(t, hub, Batch{SourceID: "crash-src", Events: []Event{e0, e1}})
	if res.Inserted != 2 || res.Cursor != e1.End {
		t.Fatalf("retry after crash = %+v; want {2, %d}", res, e1.End)
	}
}

// TestIngestCrashHelper is the helper process for the crash test above. It
// writes rows inside an open transaction, proves they are visible locally,
// then dies without committing.
func TestIngestCrashHelper(t *testing.T) {
	path := os.Getenv("OHYA_CRASH_DB")
	if path == "" {
		t.Skip("crash helper runs only under TestIngestSurvivesCrashWithOpenTransaction")
	}
	hub, err := Open(context.Background(), path)
	if err != nil {
		panic(err)
	}
	tx, err := hub.Writer().Begin()
	if err != nil {
		panic(err)
	}
	sid := SessionID(deviceHex, "claude-code", "sess-1")
	if _, err := tx.Exec(
		`INSERT INTO sessions (id, agent, device_id, native_id) VALUES (?, 'claude-code', ?, 'sess-1')`,
		sid, deviceHex); err != nil {
		panic(err)
	}
	var offset int64
	for _, raw := range []string{rawUser("doomed one"), rawUser("doomed two")} {
		if _, err := tx.Exec(
			`INSERT INTO events (session_id, source_id, source_offset, kind, raw_json)
			 VALUES (?, 'crash-src', ?, 'user', ?)`, sid, offset, raw); err != nil {
			panic(err)
		}
		offset += int64(len(raw)) + 1
	}
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM events`).Scan(&n); err != nil || n != 2 {
		panic(fmt.Sprintf("helper transaction sees %d events, %v", n, err))
	}
	os.Exit(3) // die with the transaction open
}

// TestIngestIncompleteTailCommitsWholeRecords ties parser and write path
// together on the crash-during-write fixture: the incomplete tail is left
// for the next poll, and only complete records commit.
func TestIngestIncompleteTailCommitsWholeRecords(t *testing.T) {
	hub := ingestHub(t)
	mustRegister(t, hub, "s1", "/incomplete-tail.jsonl")
	events, perr := parseFixture(t, "incomplete-tail.jsonl", "s1")
	if perr != nil {
		t.Fatalf("incomplete tail must not be a parse error: %v", perr)
	}
	if len(events) != 3 {
		t.Fatalf("complete records = %d, want 3", len(events))
	}
	res := mustIngest(t, hub, Batch{SourceID: "s1", Events: events})
	if res.Cursor != 1545 {
		t.Fatalf("cursor = %d, want 1545: the incomplete tail stays pending", res.Cursor)
	}
	if n := countEvents(t, hub, `SELECT count(*) FROM events`); n != 3 {
		t.Fatalf("events = %d, want 3", n)
	}
}
