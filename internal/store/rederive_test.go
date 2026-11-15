package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/nibzard/ohya/internal/claudecode"
)

func claudeDeriver(version int, mutate func(*Event)) ParserDeriver {
	return ParserDeriver{
		Agent:         "claude-code",
		OutputVersion: version,
		Derive: func(in DeriveInput) (Event, error) {
			res, err := claudecode.Parse([]byte(in.RawJSON+"\n"), in.SourceOffset,
				claudecode.Options{SourceScope: in.SourceID, MaxRecordBytes: int64(len(in.RawJSON))})
			if err != nil {
				return Event{}, err
			}
			if len(res.Records) != 1 {
				return Event{}, fmt.Errorf("record at offset %d re-parsed into %d records", in.SourceOffset, len(res.Records))
			}
			ev := eventFromRecord(res.Records[0])
			ev.ParserVersion = version
			if mutate != nil {
				mutate(&ev)
			}
			return ev, nil
		},
	}
}

func ingestFixtureHub(t *testing.T) (*DB, []Event) {
	t.Helper()
	hub := ingestHub(t)
	mustRegister(t, hub, "s1", "/main-session.jsonl")
	events, err := parseFixture(t, "main-session.jsonl", "s1")
	if err != nil {
		t.Fatalf("fixture parse stopped: %v", err)
	}
	mustIngest(t, hub, Batch{SourceID: "s1", Events: events})
	return hub, events
}

func dumpHub(t *testing.T, hub *DB) string {
	t.Helper()
	var b strings.Builder
	rows, err := hub.Writer().Query(
		`SELECT e.id, e.session_id, e.source_id, e.source_offset, e.time_ms, e.kind, e.role,
		        e.tool_name, e.text, e.ordinal, e.duplicate_of, e.parser_version
		 FROM events e ORDER BY e.id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, offset, pv int64
		var sid, src, kind string
		var timeMS, ordinal, dup any
		var role, tool, text any
		if err := rows.Scan(&id, &sid, &src, &offset, &timeMS, &kind, &role, &tool, &text, &ordinal, &dup, &pv); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "event %d %s %s@%d time=%v kind=%s role=%v tool=%v text=%q ordinal=%v dup=%v pv=%d\n",
			id, sid, src, offset, timeMS, kind, role, tool, text, ordinal, dup, pv)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	srows, err := hub.Writer().Query(
		`SELECT id, event_count, started_at_ms, last_event_at_ms, cwd, branch, repo, model, first_prompt, metadata
		 FROM sessions ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for srows.Next() {
		var id string
		var count int
		var started, last any
		var cwd, branch, repo, model, prompt any
		var metadata string
		if err := srows.Scan(&id, &count, &started, &last, &cwd, &branch, &repo, &model, &prompt, &metadata); err != nil {
			srows.Close()
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "session %s count=%d started=%v last=%v cwd=%v branch=%v repo=%v model=%v prompt=%q md=%s\n",
			id, count, started, last, cwd, branch, repo, model, prompt, metadata)
	}
	srows.Close()
	if err := srows.Err(); err != nil {
		t.Fatal(err)
	}

	urows, err := hub.Writer().Query(
		`SELECT session_id, message_id, input_tokens, cache_creation_input_tokens, cache_read_input_tokens, output_tokens
		 FROM session_token_usage ORDER BY session_id, message_id`)
	if err != nil {
		t.Fatal(err)
	}
	for urows.Next() {
		var sid, mid string
		var in, cc, cr, out int64
		if err := urows.Scan(&sid, &mid, &in, &cc, &cr, &out); err != nil {
			urows.Close()
			t.Fatal(err)
		}
		fmt.Fprintf(&b, "usage %s %s %d %d %d %d\n", sid, mid, in, cc, cr, out)
	}
	urows.Close()
	if err := urows.Err(); err != nil {
		t.Fatal(err)
	}

	b.WriteString(ftsDocuments(t, hub))
	return b.String()
}

func ftsDocuments(t *testing.T, hub *DB) string {
	t.Helper()
	if _, err := hub.Writer().Exec(`INSERT INTO events_fts(events_fts) VALUES ('integrity-check')`); err != nil {
		t.Fatalf("FTS integrity-check: %v", err)
	}
	rows, err := hub.Writer().Query(
		`SELECT rowid FROM events_fts WHERE events_fts MATCH 'prime OR fixture OR verify OR token OR nineteen' ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("fts %v\n", ids)
}

func matchCount(t *testing.T, hub *DB, term string) int {
	t.Helper()
	var n int
	if err := hub.Writer().QueryRow(
		`SELECT count(*) FROM events_fts WHERE events_fts MATCH ?`, term).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestReDeriveIsNoOpWhenCurrent(t *testing.T) {
	hub, events := ingestFixtureHub(t)
	if len(events) == 0 {
		t.Fatal("fixture produced no events")
	}
	before := dumpHub(t, hub)

	res, err := hub.ReDerive(context.Background(), []ParserDeriver{claudeDeriver(1, nil)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Events != 0 || res.Sessions != 0 {
		t.Fatalf("no-op result = %+v, want zero counts", res)
	}
	if after := dumpHub(t, hub); after != before {
		t.Fatalf("no-op changed the hub:\n%s", diffSnip(before, after))
	}
}

func TestReDeriveReproducesRowsAndIsIdempotent(t *testing.T) {
	hub, _ := ingestFixtureHub(t)
	before := dumpHub(t, hub)

	res, err := hub.ReDerive(context.Background(), []ParserDeriver{claudeDeriver(2, nil)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Events == 0 || res.Sessions == 0 {
		t.Fatalf("rebuild result = %+v, want work done", res)
	}
	rebuilt := dumpHub(t, hub)
	if strings.Count(rebuilt, "pv=1") != 0 || strings.Count(rebuilt, "pv=2") != strings.Count(before, "pv=1") {
		t.Fatalf("rebuild did not stamp version 2 everywhere:\n%s", rebuilt)
	}
	if stripVersions(rebuilt) != stripVersions(before) {
		t.Fatalf("rebuild changed derived state beyond the version stamp:\n%s", diffSnip(before, rebuilt))
	}

	res2, err := hub.ReDerive(context.Background(), []ParserDeriver{claudeDeriver(2, nil)})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Events != 0 || res2.Sessions != 0 {
		t.Fatalf("second pass = %+v, want zero counts", res2)
	}
	if after := dumpHub(t, hub); after != rebuilt {
		t.Fatalf("second pass changed the hub:\n%s", diffSnip(rebuilt, after))
	}
}

func TestReDeriveRewritesTextSessionsAndFTS(t *testing.T) {
	hub, _ := ingestFixtureHub(t)
	ctx := context.Background()

	var eventID int64
	if err := hub.Writer().QueryRow(`SELECT id FROM events WHERE kind = 'user' ORDER BY id LIMIT 1`).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := Annotate(ctx, hub.Writer(), deviceHex, []AnnotationOp{{
		Op: AnnotationUpsert, EventID: eventID, Namespace: "review", Key: "status", Value: `"ok"`,
	}}); err != nil {
		t.Fatal(err)
	}
	var rawBefore, cursorBefore string
	var cursorBytes int64
	if err := hub.Writer().QueryRow(`SELECT raw_json FROM events WHERE id = ?`, eventID).Scan(&rawBefore); err != nil {
		t.Fatal(err)
	}
	if err := hub.Writer().QueryRow(`SELECT path, cursor_bytes FROM sources WHERE id = 's1'`).Scan(&cursorBefore, &cursorBytes); err != nil {
		t.Fatal(err)
	}

	if matchCount(t, hub, "nineteen") != 0 {
		t.Fatal("precondition: the new term must not exist yet")
	}
	oldPrime := matchCount(t, hub, "prime")

	res, err := hub.ReDerive(ctx, []ParserDeriver{claudeDeriver(2, func(ev *Event) {
		if ev.Text != "" {
			ev.Text = ev.Text + " nineteen"
		}
	})})
	if err != nil {
		t.Fatal(err)
	}
	if res.Events == 0 {
		t.Fatal("rebuild did no work")
	}

	var textRows int
	if err := hub.Writer().QueryRow(
		`SELECT count(*) FROM events WHERE text IS NOT NULL AND length(text) > 0`).Scan(&textRows); err != nil {
		t.Fatal(err)
	}
	if n := matchCount(t, hub, "nineteen"); n != textRows {
		t.Fatalf("MATCH nineteen = %d, want every text-bearing row (%d)", n, textRows)
	}
	if n := matchCount(t, hub, "prime"); n != oldPrime {
		t.Fatalf("MATCH prime = %d after rebuild, want the pre-rebuild %d", n, oldPrime)
	}
	if _, err := hub.Writer().Exec(`INSERT INTO events_fts(events_fts) VALUES ('integrity-check')`); err != nil {
		t.Fatalf("FTS integrity-check after rebuild: %v", err)
	}

	var rawAfter string
	if err := hub.Writer().QueryRow(`SELECT raw_json FROM events WHERE id = ?`, eventID).Scan(&rawAfter); err != nil {
		t.Fatal(err)
	}
	if rawAfter != rawBefore {
		t.Fatal("re-derivation rewrote raw_json")
	}
	var pathAfter string
	var cursorAfter int64
	if err := hub.Writer().QueryRow(`SELECT path, cursor_bytes FROM sources WHERE id = 's1'`).Scan(&pathAfter, &cursorAfter); err != nil {
		t.Fatal(err)
	}
	if pathAfter != cursorBefore || cursorAfter != cursorBytes {
		t.Fatal("re-derivation moved a source cursor")
	}
	var annotation int
	if err := hub.Writer().QueryRow(
		`SELECT count(*) FROM event_annotations a JOIN events e ON e.id = a.event_id
		 WHERE a.event_id = ? AND a.writer_id = ?`, eventID, deviceHex).Scan(&annotation); err != nil || annotation != 1 {
		t.Fatalf("annotation after rebuild = %d, %v; want 1", annotation, err)
	}
}

func TestReDeriveMovesSessions(t *testing.T) {
	hub, _ := ingestFixtureHub(t)
	old := SessionID(deviceHex, "claude-code", "00000000-0000-4000-8000-0000000000a1")

	res, err := hub.ReDerive(context.Background(), []ParserDeriver{claudeDeriver(2, func(ev *Event) {
		ev.NativeID = "thread-renamed"
	})})
	if err != nil {
		t.Fatal(err)
	}
	if res.Events == 0 {
		t.Fatal("rebuild did no work")
	}

	var n int
	if err := hub.Writer().QueryRow(`SELECT count(*) FROM sessions WHERE id = ?`, old).Scan(&n); err != nil || n != 0 {
		t.Fatalf("old session survives = %d, %v; want deleted", n, err)
	}
	row := sessionRowFor(t, hub, SessionID(deviceHex, "claude-code", "thread-renamed"))
	if row.EventCount != 16 || row.FirstPrompt.String != "Find the highest prime below 20." ||
		row.Cwd.String != "/home/dev/fixture-project" || row.Model.String != "fixture-model-v1" {
		t.Fatalf("moved session = %+v", row)
	}
	if u := tokenTotals(t, hub, SessionID(deviceHex, "claude-code", "thread-renamed")); u.InputTokens != 180 {
		t.Fatalf("moved token totals = %+v", u)
	}
}

func rawOrdinal(content string, ordinal int64) string {
	return fmt.Sprintf(`{"type":"user","sessionId":"sess-1","ordinal":%d,"message":{"role":"user","content":%q}}`, ordinal, content)
}

func ordinalDeriver(version int) ParserDeriver {
	inner := claudeDeriver(version, nil)
	return ParserDeriver{
		Agent:         "claude-code",
		OutputVersion: version,
		Derive: func(in DeriveInput) (Event, error) {
			ev, err := inner.Derive(in)
			if err != nil {
				return Event{}, err
			}
			var o struct {
				Ordinal int64 `json:"ordinal"`
			}
			if err := json.Unmarshal([]byte(in.RawJSON), &o); err != nil {
				return Event{}, err
			}
			ev.Ordinal, ev.HasOrdinal = o.Ordinal, true
			return ev, nil
		},
	}
}

func TestReDeriveReassignsDuplicatesInWalkOrder(t *testing.T) {
	hub := ingestHub(t)
	mustRegister(t, hub, "orig", "/orig.jsonl")
	mustRegister(t, hub, "page", "/page.jsonl")

	o1 := ordinalEvent(0, 1, rawOrdinal("replayed text", 1))
	p1 := ordinalEvent(0, 1, o1.RawJSON)
	mustIngest(t, hub, Batch{SourceID: "page", Events: []Event{p1}})
	mustIngest(t, hub, Batch{SourceID: "orig", Events: []Event{o1}})
	pageRow, origRow := eventAt(t, hub, "page", 0), eventAt(t, hub, "orig", 0)
	if pageRow.DuplicateOf.Valid || !origRow.DuplicateOf.Valid || origRow.DuplicateOf.Int64 != pageRow.ID {
		t.Fatalf("precondition: arrival made page %d the original of orig %d", pageRow.ID, origRow.ID)
	}

	if _, err := hub.ReDerive(context.Background(), []ParserDeriver{ordinalDeriver(2)}); err != nil {
		t.Fatal(err)
	}
	pageRow, origRow = eventAt(t, hub, "page", 0), eventAt(t, hub, "orig", 0)
	if origRow.DuplicateOf.Valid || pageRow.DuplicateOf.Int64 != origRow.ID {
		t.Fatalf("after rebuild: orig = %+v, page = %+v; want orig the original", origRow, pageRow)
	}
	if !origRow.Text.Valid || pageRow.Text.Valid {
		t.Fatalf("text after reassignment: orig = %+v, page = %+v; want text once", origRow, pageRow)
	}
	if n := matchCount(t, hub, "replayed"); n != 1 {
		t.Fatalf("MATCH replayed = %d, want 1", n)
	}
	if _, err := hub.Writer().Exec(`INSERT INTO events_fts(events_fts) VALUES ('integrity-check')`); err != nil {
		t.Fatalf("FTS integrity-check: %v", err)
	}
	row := sessionRowFor(t, hub, SessionID(deviceHex, "claude-code", "sess-1"))
	if row.EventCount != 1 {
		t.Fatalf("event_count = %d, want 1 (the duplicate counts once)", row.EventCount)
	}
}

func TestReDeriveKeepsDistinctRecordsAtOneOrdinal(t *testing.T) {
	hub := ingestHub(t)
	mustRegister(t, hub, "orig", "/orig.jsonl")
	mustRegister(t, hub, "page", "/page.jsonl")
	mustRegister(t, hub, "replay", "/replay.jsonl")

	o2 := ordinalEvent(0, 2, rawOrdinal("aborted end", 2))
	p2 := ordinalEvent(0, 2, rawOrdinal("regenerated answer", 2))
	r2 := ordinalEvent(0, 2, o2.RawJSON)
	mustIngest(t, hub, Batch{SourceID: "page", Events: []Event{p2}})
	mustIngest(t, hub, Batch{SourceID: "orig", Events: []Event{o2}})
	mustIngest(t, hub, Batch{SourceID: "replay", Events: []Event{r2}})

	if _, err := hub.ReDerive(context.Background(), []ParserDeriver{ordinalDeriver(2)}); err != nil {
		t.Fatal(err)
	}
	oRow, pRow, rRow := eventAt(t, hub, "orig", 0), eventAt(t, hub, "page", 0), eventAt(t, hub, "replay", 0)
	if oRow.DuplicateOf.Valid || pRow.DuplicateOf.Valid || !oRow.Text.Valid || !pRow.Text.Valid {
		t.Fatalf("distinct records at ordinal 2 = orig %+v, page %+v; want two text-bearing originals", oRow, pRow)
	}
	if !rRow.DuplicateOf.Valid || rRow.DuplicateOf.Int64 != oRow.ID || rRow.Text.Valid {
		t.Fatalf("byte-identical replay = %+v; want a text-less duplicate of orig %d", rRow, oRow.ID)
	}
	if n := matchCount(t, hub, "aborted"); n != 1 {
		t.Fatalf("MATCH aborted = %d, want 1", n)
	}
	if n := matchCount(t, hub, "regenerated"); n != 1 {
		t.Fatalf("MATCH regenerated = %d, want 1", n)
	}
	if row := sessionRowFor(t, hub, SessionID(deviceHex, "claude-code", "sess-1")); row.EventCount != 2 {
		t.Fatalf("event_count = %d, want 2 (the duplicate counts once)", row.EventCount)
	}
	if _, err := hub.Writer().Exec(`INSERT INTO events_fts(events_fts) VALUES ('integrity-check')`); err != nil {
		t.Fatalf("FTS integrity-check: %v", err)
	}
}

func TestReDeriveRefusesNewerStoredVersions(t *testing.T) {
	hub, _ := ingestFixtureHub(t)
	if _, err := hub.Writer().Exec(`UPDATE events SET parser_version = 3`); err != nil {
		t.Fatal(err)
	}
	before := dumpHub(t, hub)

	_, err := hub.ReDerive(context.Background(), []ParserDeriver{claudeDeriver(2, nil)})
	if err == nil || !strings.Contains(err.Error(), "upgrade") {
		t.Fatalf("newer-version error = %v, want an upgrade refusal", err)
	}
	if after := dumpHub(t, hub); after != before {
		t.Fatalf("refusal changed the hub:\n%s", diffSnip(before, after))
	}
}

func TestReDeriveRollsBackOnDeriveFailure(t *testing.T) {
	hub, _ := ingestFixtureHub(t)
	before := dumpHub(t, hub)

	_, err := hub.ReDerive(context.Background(), []ParserDeriver{claudeDeriver(2, func(ev *Event) {
		if ev.Offset > 0 {
			ev.Text = ""
			ev.NativeID = ""
		}
	})})
	if err == nil {
		t.Fatal("a deriver that drops the native ID must fail the pass")
	}
	if after := dumpHub(t, hub); after != before {
		t.Fatalf("failed pass changed the hub:\n%s", diffSnip(before, after))
	}
}

func TestReDeriveRejectsBadBindings(t *testing.T) {
	hub, _ := ingestFixtureHub(t)
	good := claudeDeriver(1, nil)
	fails := func(p []ParserDeriver) {
		t.Helper()
		if _, err := hub.ReDerive(context.Background(), p); err == nil {
			t.Fatalf("binding %v accepted", p)
		}
	}
	fails(nil)
	fails([]ParserDeriver{{Agent: "claude-code", OutputVersion: 1}})
	fails([]ParserDeriver{{Agent: "claude-code", OutputVersion: 0, Derive: good.Derive}})
	fails([]ParserDeriver{{Agent: "Claude Code", OutputVersion: 1, Derive: good.Derive}})
	fails([]ParserDeriver{good, {Agent: "claude-code", OutputVersion: 2, Derive: good.Derive}})
}

func TestReDeriveWalksMoreThanOneChunk(t *testing.T) {
	hub := ingestHub(t)
	mustRegister(t, hub, "s1", "/big.jsonl")
	var events []Event
	off := int64(0)
	for i := range rederiveChunk*2 + 7 {
		raw := rawUser(fmt.Sprintf("record %d of the long source", i))
		events = append(events, mkEvent(off, raw))
		off += int64(len(raw)) + 1
	}
	mustIngest(t, hub, Batch{SourceID: "s1", Events: events})

	res, err := hub.ReDerive(context.Background(), []ParserDeriver{claudeDeriver(2, nil)})
	if err != nil {
		t.Fatal(err)
	}
	if res.Events != len(events) {
		t.Fatalf("re-derived %d events, want %d", res.Events, len(events))
	}
	row := sessionRowFor(t, hub, SessionID(deviceHex, "claude-code", "sess-1"))
	if row.EventCount != len(events) {
		t.Fatalf("event_count = %d, want %d", row.EventCount, len(events))
	}
	if n := countEvents(t, hub, `SELECT count(*) FROM events WHERE parser_version = 2`); n != len(events) {
		t.Fatalf("version-2 rows = %d, want %d", n, len(events))
	}
}

func stripVersions(dump string) string {
	return parserVersionRE.ReplaceAllString(dump, "pv=*")
}

var parserVersionRE = regexp.MustCompile(`pv=\d+`)

func diffSnip(want, got string) string {
	wl, gl := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := range wl {
		if i >= len(gl) || wl[i] != gl[i] {
			return fmt.Sprintf("line %d:\n  want: %s\n  got:  %s", i, wl[i], gl[i])
		}
	}
	return fmt.Sprintf("got %d extra lines from %q", len(gl)-len(wl), strings.Join(gl[len(wl):], " | "))
}

func TestIngestRejectsMissingParserVersion(t *testing.T) {
	hub := ingestHub(t)
	mustRegister(t, hub, "s1", "/m.jsonl")
	anon := mkEvent(0, rawUser("no version"))
	anon.ParserVersion = 0
	_, err := hub.Ingest(context.Background(), Batch{SourceID: "s1", DeviceID: deviceHex, Events: []Event{anon}})
	if !errors.Is(err, ErrBatchRejected) {
		t.Fatalf("missing version error = %v", err)
	}
}

func TestReDeriveMatchesIngestionAcrossSources(t *testing.T) {
	hub := ingestHub(t)
	for _, source := range []string{"z", "a"} {
		mustRegister(t, hub, source, "/"+source+".jsonl")
		records, err := parseFixture(t, "main-session.jsonl", source)
		if err != nil {
			t.Fatal(err)
		}
		split := len(records) / 2
		mustIngest(t, hub, Batch{SourceID: source, Events: records[:split]})
		mustIngest(t, hub, Batch{SourceID: source, Events: records[split:]})
	}
	before := dumpHub(t, hub)
	if _, err := hub.ReDerive(context.Background(), []ParserDeriver{claudeDeriver(2, nil)}); err != nil {
		t.Fatal(err)
	}
	after := dumpHub(t, hub)
	if stripVersions(before) != stripVersions(after) {
		t.Fatalf("rebuild changed state:\n%s", diffSnip(before, after))
	}
}
