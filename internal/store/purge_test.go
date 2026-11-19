package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/nibzard/ohya/internal/workspace"
)

const otherDeviceHex = "fedcba9876543210fedcba9876543210"

func mustRegisterAs(t *testing.T, hub *DB, id, device, agent, path string) {
	t.Helper()
	if err := hub.RegisterSource(context.Background(), id, device, agent, path); err != nil {
		t.Fatal(err)
	}
}

func batchOf(sourceID, device, native string, texts ...string) Batch {
	var events []Event
	offset := int64(0)
	for _, text := range texts {
		raw := fmt.Sprintf(`{"type":"user","sessionId":%q,"message":{"role":"user","content":%q}}`, native, text)
		events = append(events, Event{
			Offset: offset, End: offset + int64(len(raw)) + 1,
			NativeID: native, Kind: "user", Role: "user", Text: text, RawJSON: raw,
			ParserVersion: 1,
		})
		offset = events[len(events)-1].End
	}
	return Batch{SourceID: sourceID, DeviceID: device, Events: events}
}

func purgeFixtureHub(t *testing.T) *DB {
	t.Helper()
	hub := ingestHub(t)
	mustRegisterAs(t, hub, "src-a1", deviceHex, "claude-code", "/home/a/main.jsonl")
	mustIngest(t, hub, batchOf("src-a1", deviceHex, "sess-a", "alpha one", "alpha two"))
	mustRegisterAs(t, hub, "src-a2", deviceHex, "claude-code", "/home/a/second.jsonl")
	mustIngest(t, hub, batchOf("src-a2", deviceHex, "sess-a2", "alpha other file"))
	mustRegisterAs(t, hub, "src-b1", otherDeviceHex, "claude-code", "/home/b/main.jsonl")
	mustIngestAs(t, hub, otherDeviceHex, batchOf("src-b1", otherDeviceHex, "sess-b", "bravo one"))
	mustRegisterAs(t, hub, "src-b2", otherDeviceHex, "claude-code", "/home/b/rollout-page.jsonl")
	mustIngestAs(t, hub, otherDeviceHex, batchOf("src-b2", otherDeviceHex, "sess-b", "bravo same thread other file"))
	return hub
}

func hubCount(t *testing.T, hub *DB, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := hub.Writer().QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPurgeDryRunChangesNothing(t *testing.T) {
	hub := purgeFixtureHub(t)
	before := hubSnapshot(t, hub)
	plan, err := hub.PlanPurge(context.Background(), PurgeSelector{Session: "sess-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Sessions) != 1 || plan.Sessions[0].NativeID != "sess-a" {
		t.Fatalf("plan sessions = %+v", plan.Sessions)
	}
	if len(plan.Sources) != 1 || plan.Sources[0].ID != "src-a1" {
		t.Fatalf("plan sources = %+v", plan.Sources)
	}
	if plan.Events != 2 || plan.Bytes != plan.Sources[0].Cursor {
		t.Fatalf("plan totals: events %d, bytes %d, cursor %d", plan.Events, plan.Bytes, plan.Sources[0].Cursor)
	}
	after := hubSnapshot(t, hub)
	if before != after {
		t.Fatalf("dry run changed the database\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func hubSnapshot(t *testing.T, hub *DB) string {
	t.Helper()
	var b strings.Builder
	queries := []string{
		`SELECT count(*) FROM sessions`,
		`SELECT count(*) FROM sources`,
		`SELECT count(*) FROM events`,
		`SELECT count(*) FROM session_token_usage`,
		`SELECT count(*) FROM event_annotations`,
		`SELECT count(*) FROM purged_sources`,
		`SELECT group_concat(id || ':' || cursor_bytes, ',') FROM (SELECT id, cursor_bytes FROM sources ORDER BY id)`,
		`SELECT group_concat(id || ':' || event_count, ',') FROM (SELECT id, event_count FROM sessions ORDER BY id)`,
		`SELECT count(*) FROM events_fts WHERE events_fts MATCH 'alpha OR bravo'`,
	}
	for _, q := range queries {
		var v any
		if err := hub.Writer().QueryRow(q).Scan(&v); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		fmt.Fprintf(&b, "%s => %v\n", q, v)
	}
	return b.String()
}

func TestPurgeSessionEvictsWholeSources(t *testing.T) {
	hub := purgeFixtureHub(t)
	ctx := context.Background()
	if _, err := Annotate(ctx, hub.Writer(), deviceHex, annotateArgs(t, hub, "src-a1")); err != nil {
		t.Fatal(err)
	}
	_, res, err := hub.ApplyPurge(ctx, PurgeSelector{Session: "sess-a"})
	if err != nil {
		t.Fatal(err)
	}
	if got := hubCount(t, hub, `SELECT count(*) FROM events WHERE source_id = 'src-a1'`); got != 0 {
		t.Fatalf("events of src-a1 survive: %d", got)
	}
	if got := hubCount(t, hub, `SELECT count(*) FROM sources WHERE id = 'src-a1'`); got != 0 {
		t.Fatalf("sources row of src-a1 survives: %d", got)
	}
	if got := hubCount(t, hub, `SELECT count(*) FROM sessions WHERE native_id = 'sess-a'`); got != 0 {
		t.Fatalf("session sess-a survives: %d", got)
	}
	if got := hubCount(t, hub, `SELECT count(*) FROM event_annotations`); got != 0 {
		t.Fatalf("annotations of purged events survive: %d", got)
	}
	var tomb PurgedSourceInfo
	var native sql.NullString
	err = hub.Writer().QueryRow(
		`SELECT source_id, device_id, agent, path, cursor_bytes, native_id, scope, purged_at_ms
		 FROM purged_sources`).Scan(&tomb.SourceID, &tomb.DeviceID, &tomb.Agent, &tomb.Path,
		&tomb.Cursor, &native, &tomb.Scope, &tomb.PurgedAtMS)
	if err != nil {
		t.Fatalf("read tombstone: %v", err)
	}
	tomb.NativeID = native.String
	if tomb.SourceID != "src-a1" || tomb.DeviceID != deviceHex || tomb.Agent != "claude-code" ||
		tomb.Path != "/home/a/main.jsonl" || tomb.NativeID != "sess-a" || tomb.Cursor == 0 {
		t.Fatalf("tombstone = %+v", tomb)
	}
	if !strings.Contains(tomb.Scope, "session=sess-a") {
		t.Fatalf("tombstone scope = %q", tomb.Scope)
	}
	if ms := time.UnixMilli(tomb.PurgedAtMS); ms.After(time.Now().Add(time.Minute)) || ms.Before(time.Now().Add(-time.Hour)) {
		t.Fatalf("tombstone purged_at_ms out of range: %d", tomb.PurgedAtMS)
	}
	if res.PurgedAtMS != tomb.PurgedAtMS {
		t.Fatalf("result purge time %d differs from tombstone %d", res.PurgedAtMS, tomb.PurgedAtMS)
	}
	if got := hubCount(t, hub, `SELECT count(*) FROM events`); got != 3 {
		t.Fatalf("surviving events = %d, want the 3 of sess-a2 and sess-b's two files", got)
	}
	if got := hubCount(t, hub, `SELECT count(*) FROM events_fts WHERE events_fts MATCH 'alpha AND one'`); got != 0 {
		t.Fatalf("FTS still finds purged text: %d", got)
	}
	if err := hub.FTSIndexConsistent(ctx); err != nil {
		t.Fatalf("FTS integrity after purge: %v", err)
	}
}

func annotateArgs(t *testing.T, hub *DB, sourceID string) []AnnotationOp {
	t.Helper()
	var id int64
	if err := hub.Writer().QueryRow(`SELECT id FROM events WHERE source_id = ? ORDER BY source_offset LIMIT 1`, sourceID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return []AnnotationOp{{Op: "upsert", EventID: id, Namespace: "tags", Key: "purge-me", Value: `true`}}
}

func TestPurgeSessionSpansSourcesAndDevicesScope(t *testing.T) {
	hub := ingestHub(t)
	ctx := context.Background()
	mustRegisterAs(t, hub, "src-1", deviceHex, "codex", "/home/a/rollout.jsonl")
	mustIngest(t, hub, batchOf("src-1", deviceHex, "thread-x", "page one", "page two"))
	mustRegisterAs(t, hub, "src-2", deviceHex, "codex", "/home/a/rollout-resumed.jsonl")
	mustIngest(t, hub, batchOf("src-2", deviceHex, "thread-x", "page three"))
	mustRegisterAs(t, hub, "src-3", deviceHex, "codex", "/home/a/other-thread.jsonl")
	mustIngest(t, hub, batchOf("src-3", deviceHex, "thread-y", "other thread"))

	plan, _, err := hub.ApplyPurge(ctx, PurgeSelector{Session: "thread-x"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Sources) != 2 {
		t.Fatalf("plan sources = %+v, want both files of the thread", plan.Sources)
	}
	if got := hubCount(t, hub, `SELECT count(*) FROM sessions`); got != 1 {
		t.Fatalf("sessions after purge = %d, want only thread-y", got)
	}
	if got := hubCount(t, hub, `SELECT count(*) FROM events`); got != 1 {
		t.Fatalf("events after purge = %d", got)
	}
	if got := hubCount(t, hub, `SELECT count(*) FROM sources`); got != 1 {
		t.Fatalf("sources after purge = %d", got)
	}
	if got := hubCount(t, hub, `SELECT count(*) FROM purged_sources`); got != 2 {
		t.Fatalf("tombstones after purge = %d", got)
	}
}

func TestPurgeSelectors(t *testing.T) {
	hub := purgeFixtureHub(t)
	ctx := context.Background()
	old := Event{NativeID: "sess-old"}
	old.RawJSON = `{"type":"user","sessionId":"sess-old","message":{"role":"user","content":"old"}}`
	old.End = int64(len(old.RawJSON)) + 1
	old.Kind, old.Role, old.Text, old.ParserVersion = "user", "user", "old session", 1
	old.HasTime, old.TimeMS = true, time.Now().Add(-48*time.Hour).UnixMilli()
	mustRegisterAs(t, hub, "src-old", deviceHex, "claude-code", "/home/a/old.jsonl")
	mustIngest(t, hub, Batch{SourceID: "src-old", DeviceID: deviceHex, Events: []Event{old}})

	plan, err := hub.PlanPurge(ctx, PurgeSelector{Agent: "claude-code", BeforeMS: time.Now().Add(-time.Hour).UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Sessions) != 1 || plan.Sessions[0].NativeID != "sess-old" {
		t.Fatalf("before selector planned %+v", plan.Sessions)
	}

	plan, err = hub.PlanPurge(ctx, PurgeSelector{Session: "sess-old", Agent: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Sessions) != 0 {
		t.Fatalf("conflicting selectors planned %+v", plan.Sessions)
	}

	_, err = hub.PlanPurge(ctx, PurgeSelector{})
	if !errors.Is(err, ErrBatchRejected) {
		t.Fatalf("empty selector error = %v", err)
	}
}

func TestPurgeRefusesTombstonedSource(t *testing.T) {
	hub := purgeFixtureHub(t)
	ctx := context.Background()
	if _, _, err := hub.ApplyPurge(ctx, PurgeSelector{Session: "sess-a"}); err != nil {
		t.Fatal(err)
	}
	tomb, err := hub.purgedSource(ctx, "src-a1")
	if err != nil {
		t.Fatal(err)
	}

	err = hub.RegisterSource(ctx, "src-a1", deviceHex, "claude-code", "/home/a/main.jsonl")
	if !errors.Is(err, ErrSourcePurged) {
		t.Fatalf("re-register error = %v", err)
	}
	assertRefusalText(t, err, tomb)

	_, err = hub.Ingest(ctx, batchOf("src-a1", deviceHex, "sess-a", "resurrect attempt"))
	if !errors.Is(err, ErrSourcePurged) {
		t.Fatalf("ingest error = %v", err)
	}
	assertRefusalText(t, err, tomb)

	_, err = hub.SourceState(ctx, "src-a1")
	if !errors.Is(err, ErrSourcePurged) {
		t.Fatalf("source state error = %v", err)
	}

	err = hub.RegisterSource(ctx, "src-a1", otherDeviceHex, "claude-code", "/home/a/main.jsonl")
	if !errors.Is(err, ErrSourceOwnership) {
		t.Fatalf("re-register as another device error = %v, want ErrSourceOwnership", err)
	}

	if _, err := hub.Ingest(ctx, batchOf("src-unknown", otherDeviceHex, "sess-b", "unregistered")); err == nil {
		t.Fatal("an unregistered source unexpectedly ingested")
	} else if !errors.Is(err, ErrSourceNotRegistered) {
		t.Fatalf("unrelated source error = %v", err)
	}
	var b2cursor int64
	if err := hub.Writer().QueryRow(`SELECT cursor_bytes FROM sources WHERE id = 'src-b2'`).Scan(&b2cursor); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.Ingest(ctx, replayBatch("src-b2", otherDeviceHex, "sess-b", b2cursor, "still syncing")); err != nil {
		t.Fatalf("an unaffected registered source cannot ingest after the purge: %v", err)
	}
	if got := hubCount(t, hub, `SELECT count(*) FROM events WHERE session_id = ?`,
		SessionID(deviceHex, "claude-code", "sess-a")); got != 0 {
		t.Fatalf("purged session gained events: %d", got)
	}
}

func assertRefusalText(t *testing.T, err error, tomb PurgedSourceInfo) {
	t.Helper()
	text := err.Error()
	if !strings.Contains(text, msRFC3339(tomb.PurgedAtMS)) {
		t.Errorf("refusal %q names no purge date %s", text, msRFC3339(tomb.PurgedAtMS))
	}
	if !strings.Contains(text, "ohya sources reallow src-a1") {
		t.Errorf("refusal %q names no remedy", text)
	}
	if !strings.Contains(text, tomb.Scope) {
		t.Errorf("refusal %q names no purge scope %q", text, tomb.Scope)
	}
}

func TestReallowResumesFromPurgeCursorAndBlocksReplay(t *testing.T) {
	hub := purgeFixtureHub(t)
	ctx := context.Background()
	if _, _, err := hub.ApplyPurge(ctx, PurgeSelector{Session: "sess-a"}); err != nil {
		t.Fatal(err)
	}
	var cursor int64
	if err := hub.Writer().QueryRow(`SELECT cursor_bytes FROM purged_sources WHERE source_id = 'src-a1'`).Scan(&cursor); err != nil {
		t.Fatal(err)
	}

	if _, err := hub.ReallowSource(ctx, "src-a1"); err != nil {
		t.Fatal(err)
	}
	if got := hubCount(t, hub, `SELECT count(*) FROM purged_sources`); got != 0 {
		t.Fatalf("tombstone survives reallow: %d", got)
	}
	var restored int64
	if err := hub.Writer().QueryRow(`SELECT cursor_bytes FROM sources WHERE id = 'src-a1'`).Scan(&restored); err != nil {
		t.Fatalf("reallow restored no source row: %v", err)
	}
	if restored != cursor {
		t.Fatalf("restored cursor %d, want the purge cursor %d", restored, cursor)
	}
	st, err := hub.SourceState(ctx, "src-a1")
	if err != nil {
		t.Fatal(err)
	}
	if st.Cursor != cursor {
		t.Fatalf("probe cursor after reallow = %d, want %d", st.Cursor, cursor)
	}

	if _, err := hub.Ingest(ctx, replayBatch("src-a1", deviceHex, "sess-a", 0, "alpha one")); !errors.Is(err, ErrBatchRejected) {
		t.Fatalf("below-cursor replay after reallow error = %v, want a deterministic rejection", err)
	}
	if got := hubCount(t, hub, `SELECT count(*) FROM events WHERE source_id = 'src-a1'`); got != 0 {
		t.Fatalf("below-cursor replay resurrected %d events", got)
	}

	raw := `{"type":"user","sessionId":"sess-a","message":{"role":"user","content":"alpha resumes"}}`
	res := mustIngest(t, hub, Batch{SourceID: "src-a1", DeviceID: deviceHex, Events: []Event{{
		Offset: cursor, End: cursor + int64(len(raw)) + 1,
		NativeID: "sess-a", Kind: "user", Role: "user", Text: "alpha resumes", RawJSON: raw,
		ParserVersion: 1,
	}}})
	if res.Inserted != 1 || res.Cursor != cursor+int64(len(raw))+1 {
		t.Fatalf("resume after reallow: %+v", res)
	}
	if got := hubCount(t, hub, `SELECT count(*) FROM events WHERE source_id = 'src-a1'`); got != 1 {
		t.Fatalf("resumed source holds %d events, want only the new one", got)
	}
	if err := hub.FTSIndexConsistent(ctx); err != nil {
		t.Fatalf("FTS integrity after resume: %v", err)
	}
}

func replayBatch(sourceID, device, native string, offset int64, texts ...string) Batch {
	b := batchOf(sourceID, device, native, texts...)
	for i := range b.Events {
		b.Events[i].Offset += offset
		b.Events[i].End += offset
	}
	return b
}

func TestReallowUnknownSource(t *testing.T) {
	hub := purgeFixtureHub(t)
	if _, err := hub.ReallowSource(context.Background(), "src-none"); !errors.Is(err, ErrNotPurged) {
		t.Fatalf("reallow unknown error = %v", err)
	}
}

func TestApplyPurgePerSourceTransactionsResume(t *testing.T) {
	hub := ingestHub(t)
	ctx := context.Background()
	mustRegisterAs(t, hub, "src-1", deviceHex, "codex", "/a/1.jsonl")
	mustIngest(t, hub, batchOf("src-1", deviceHex, "thread-1", "one"))
	mustRegisterAs(t, hub, "src-2", deviceHex, "codex", "/a/2.jsonl")
	mustIngest(t, hub, batchOf("src-2", deviceHex, "thread-2", "two"))
	if _, err := hub.Writer().Exec(
		`CREATE TRIGGER block_second_tombstone BEFORE INSERT ON purged_sources
		 WHEN NEW.source_id = 'src-2'
		 BEGIN SELECT RAISE(ABORT, 'injected: second tombstone refused'); END`); err != nil {
		t.Fatal(err)
	}
	_, _, err := hub.ApplyPurge(ctx, PurgeSelector{Agent: "codex"})
	if err == nil || !strings.Contains(err.Error(), "injected") {
		t.Fatalf("apply error = %v, want the injected failure", err)
	}
	if got := hubCount(t, hub, `SELECT count(*) FROM purged_sources WHERE source_id = 'src-1'`); got != 1 {
		t.Fatalf("first source was not committed separately: %d tombstones", got)
	}
	if got := hubCount(t, hub, `SELECT count(*) FROM events WHERE source_id = 'src-1'`); got != 0 {
		t.Fatalf("first source events survive its committed purge: %d", got)
	}
	if got := hubCount(t, hub, `SELECT count(*) FROM events WHERE source_id = 'src-2'`); got != 1 {
		t.Fatalf("second source lost events from the failed transaction: %d", got)
	}
	if got := hubCount(t, hub, `SELECT count(*) FROM sessions WHERE native_id = 'thread-2'`); got != 1 {
		t.Fatalf("second session lost its row from the failed transaction: %d", got)
	}

	if _, err := hub.Writer().Exec(`DROP TRIGGER block_second_tombstone`); err != nil {
		t.Fatal(err)
	}
	plan, _, err := hub.ApplyPurge(ctx, PurgeSelector{Agent: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Sources) != 1 || plan.Sources[0].ID != "src-2" {
		t.Fatalf("rerun planned %+v, want only the remaining source", plan.Sources)
	}
	if got := hubCount(t, hub, `SELECT count(*) FROM sources`); got != 0 {
		t.Fatalf("sources after the resumed purge = %d", got)
	}
	if got := hubCount(t, hub, `SELECT count(*) FROM sessions`); got != 0 {
		t.Fatalf("sessions after the resumed purge = %d", got)
	}
}

func TestPurgeIdempotentRerun(t *testing.T) {
	hub := purgeFixtureHub(t)
	ctx := context.Background()
	if _, _, err := hub.ApplyPurge(ctx, PurgeSelector{Session: "sess-a"}); err != nil {
		t.Fatal(err)
	}
	snapshot := hubSnapshot(t, hub)
	plan, _, err := hub.ApplyPurge(ctx, PurgeSelector{Session: "sess-a"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Sessions) != 0 || len(plan.Sources) != 0 {
		t.Fatalf("second apply planned %+v %+v", plan.Sessions, plan.Sources)
	}
	if got := hubSnapshot(t, hub); got != snapshot {
		t.Fatalf("second apply changed the database\nbefore:\n%s\nafter:\n%s", snapshot, got)
	}
}

func TestNoSourceHoldsTwoRealSessions(t *testing.T) {
	hub := ingestHub(t)

	main, _ := parseFixture(t, "main-session.jsonl", "src-cl-main")
	mustRegisterAs(t, hub, "src-cl-main", deviceHex, "claude-code", "/home/claude/main-session.jsonl")
	b := Batch{SourceID: "src-cl-main", DeviceID: deviceHex}
	for i := range main {
		b.Events = append(b.Events, main[i])
	}
	mustIngest(t, hub, b)

	sub, _ := parseFixture(t, "subagent-session.jsonl", "src-cl-sub")
	mustRegisterAs(t, hub, "src-cl-sub", deviceHex, "claude-code", "/home/claude/subagent-session.jsonl")
	b = Batch{SourceID: "src-cl-sub", DeviceID: deviceHex}
	for i := range sub {
		b.Events = append(b.Events, sub[i])
	}
	mustIngest(t, hub, b)

	mustRegisterAs(t, hub, "src-cx-1", otherDeviceHex, "codex", "/home/codex/rollout.jsonl")
	mustIngestAs(t, hub, otherDeviceHex, batchOf("src-cx-1", otherDeviceHex, "thread-9", "rollout main page"))
	mustRegisterAs(t, hub, "src-cx-2", otherDeviceHex, "codex", "/home/codex/rollout-resumed.jsonl")
	mustIngestAs(t, hub, otherDeviceHex, batchOf("src-cx-2", otherDeviceHex, "thread-9", "rollout resumed page"))

	mustRegisterAs(t, hub, "src-pi", otherDeviceHex, "pi", "/home/pi/session.jsonl")
	mustIngestAs(t, hub, otherDeviceHex, batchOf("src-pi", otherDeviceHex, "pi-sess", "pi one file one session"))

	assertSourceSessionInvariant(t, hub)
}

func assertSourceSessionInvariant(t *testing.T, hub *DB) {
	t.Helper()
	var multi int64
	if err := hub.Writer().QueryRow(`
		SELECT count(*) FROM (
		  SELECT e.source_id FROM events e
		  JOIN sessions s ON s.id = e.session_id
		  WHERE s.native_id NOT LIKE 'ohya-synthetic:%'
		  GROUP BY e.source_id HAVING count(DISTINCT e.session_id) > 1)`).
		Scan(&multi); err != nil {
		t.Fatal(err)
	}
	if multi != 0 {
		t.Fatalf("%d sources hold events of two real sessions", multi)
	}
	var foreign int64
	if err := hub.Writer().QueryRow(`
		SELECT count(*) FROM (
		  SELECT e.source_id, e.session_id FROM events e
		  JOIN sessions s ON s.id = e.session_id
		  WHERE s.native_id LIKE 'ohya-synthetic:%'
		    AND s.native_id != 'ohya-synthetic:' || e.source_id)`).
		Scan(&foreign); err != nil {
		t.Fatal(err)
	}
	if foreign != 0 {
		t.Fatalf("%d sources hold a synthetic session of another source", foreign)
	}
	var syntheticSources int64
	if err := hub.Writer().QueryRow(`
		SELECT count(DISTINCT source_id) FROM events e JOIN sessions s ON s.id = e.session_id
		WHERE s.native_id LIKE 'ohya-synthetic:%'`).Scan(&syntheticSources); err != nil {
		t.Fatal(err)
	}
	if syntheticSources == 0 {
		t.Fatalf("the fixture set never exercised a source-scoped synthetic session; the invariant test needs it")
	}
}

func TestPurgeSyntheticCompanionFollowsSource(t *testing.T) {
	hub := ingestHub(t)
	ctx := context.Background()
	mixed, _ := parseFixture(t, "subagent-session.jsonl", "src-mixed")
	mustRegisterAs(t, hub, "src-mixed", deviceHex, "claude-code", "/home/claude/subagent-session.jsonl")
	b := Batch{SourceID: "src-mixed", DeviceID: deviceHex}
	for i := range mixed {
		b.Events = append(b.Events, mixed[i])
	}
	mustIngest(t, hub, b)
	var realSession string
	if err := hub.Writer().QueryRow(
		`SELECT id FROM sessions WHERE native_id NOT LIKE 'ohya-synthetic:%'`).Scan(&realSession); err != nil {
		t.Fatalf("fixture produced no real session: %v", err)
	}
	assertSourceSessionInvariant(t, hub)

	plan, err := hub.PlanPurge(ctx, PurgeSelector{Session: "ohya-synthetic:src-mixed"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Sessions) != 0 {
		t.Fatalf("a synthetic session is selectable on its own: %+v", plan.Sessions)
	}

	if _, _, err := hub.ApplyPurge(ctx, PurgeSelector{Session: realSessionNative(t, hub, realSession)}); err != nil {
		t.Fatal(err)
	}
	if got := hubCount(t, hub, `SELECT count(*) FROM sessions WHERE agent = 'claude-code'`); got != 0 {
		t.Fatalf("sessions survive the purge of the real session: %d (synthetic companion must follow)", got)
	}
	if got := hubCount(t, hub, `SELECT count(*) FROM events`); got != 0 {
		t.Fatalf("events survive the purge of the real session: %d", got)
	}
}

func realSessionNative(t *testing.T, hub *DB, sid string) string {
	t.Helper()
	var native string
	if err := hub.Writer().QueryRow(`SELECT native_id FROM sessions WHERE id = ?`, sid).Scan(&native); err != nil {
		t.Fatal(err)
	}
	return native
}

func TestPurgePlanOnMigratedReleasedDatabase(t *testing.T) {
	dbPath := buildReleasedDatabase(t, 7)
	hub, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("upgrade a released database through store.Open: %v", err)
	}
	t.Cleanup(func() { hub.Close() })
	ctx := context.Background()
	if !tableExists(t, hub.Writer(), "purged_sources") {
		t.Fatal("purged_sources table missing after upgrade")
	}
	if _, err := hub.ReallowSource(ctx, "relsrc-a"); !errors.Is(err, ErrNotPurged) {
		t.Fatalf("reallow of a live source error = %v", err)
	}
	plan, _, err := hub.ApplyPurge(ctx, PurgeSelector{Session: "rel-sess"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Sources) != 1 || plan.Sources[0].ID != "relsrc-a" {
		t.Fatalf("upgrade purged %+v", plan.Sources)
	}
	err = hub.RegisterSource(ctx, "relsrc-a", deviceHex, "claude-code", releasedClaudePath)
	if !errors.Is(err, ErrSourcePurged) {
		t.Fatalf("re-register after purge on an upgraded database = %v", err)
	}
}

func TestPurgeMultiSourceCodexWithCrossSourceDuplicateOf(t *testing.T) {
	ctx := context.Background()
	hub := ingestHub(t)
	mustRegisterAs(t, hub, "src-1", deviceHex, "codex", "/c/orig.jsonl")
	mustRegisterAs(t, hub, "src-2", deviceHex, "codex", "/c/page.jsonl")

	raw := `{"type":"response_item","ordinal":1,"payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"shared"}]}}`
	e1 := Event{
		Offset: 0, End: int64(len(raw)) + 1, NativeID: "codex-thread-1",
		Kind: "message", Role: "user", Text: "shared", RawJSON: raw, Metadata: "{}",
		HasTime: true, TimeMS: 1000, HasOrdinal: true, Ordinal: 1, ParserVersion: 1,
	}
	mustIngest(t, hub, Batch{SourceID: "src-1", DeviceID: deviceHex, Events: []Event{e1}})
	mustIngest(t, hub, Batch{SourceID: "src-2", DeviceID: deviceHex, Events: []Event{e1}})

	if got := hubCount(t, hub, `SELECT count(*) FROM events WHERE duplicate_of IS NOT NULL`); got != 1 {
		t.Fatalf("expected 1 cross-source duplicate_of row, got %d", got)
	}

	plan, _, err := hub.ApplyPurge(ctx, PurgeSelector{Session: "codex-thread-1"})
	if err != nil {
		t.Fatalf("ApplyPurge with cross-source duplicate_of failed: %v", err)
	}
	if len(plan.Sources) != 2 {
		t.Fatalf("purged %d sources, want 2", len(plan.Sources))
	}
	if got := hubCount(t, hub, `SELECT count(*) FROM events`); got != 0 {
		t.Fatalf("remaining events = %d, want 0", got)
	}
	if err := hub.FTSIndexConsistent(ctx); err != nil {
		t.Fatalf("FTS integrity after cross-source duplicate purge: %v", err)
	}
}

func TestPurgeRefusesSourceHoldingUnselectedRealSession(t *testing.T) {
	ctx := context.Background()
	hub := ingestHub(t)
	mustRegisterAs(t, hub, "src-mixed-real", deviceHex, "claude-code", "/c/mixed.jsonl")
	raw1 := rawUser("first session")
	raw2 := rawUser("second session")
	e1 := mkEvent(0, raw1)
	e1.NativeID = "real-1"
	e2 := mkEvent(e1.End, raw2)
	e2.NativeID = "real-2"
	mustIngest(t, hub, Batch{SourceID: "src-mixed-real", DeviceID: deviceHex, Events: []Event{e1, e2}})

	if _, err := hub.PlanPurge(ctx, PurgeSelector{Session: "real-1"}); !errors.Is(err, ErrBatchRejected) {
		t.Fatalf("PlanPurge on shared source = %v, want ErrBatchRejected", err)
	}
	if _, _, err := hub.ApplyPurge(ctx, PurgeSelector{Session: "real-1"}); !errors.Is(err, ErrBatchRejected) {
		t.Fatalf("ApplyPurge on shared source = %v, want ErrBatchRejected", err)
	}
	if got := hubCount(t, hub, `SELECT count(*) FROM events`); got != 2 {
		t.Fatalf("events deleted despite refusal: %d remaining", got)
	}
}

func TestPurgeReadsAuthoritativeCursorInsideTransaction(t *testing.T) {
	ctx := context.Background()
	hub := ingestHub(t)
	mustRegisterAs(t, hub, "src-live", deviceHex, "claude-code", "/c/live.jsonl")
	e1 := mkEvent(0, rawUser("before plan"))
	e1.NativeID = "live-sess"
	mustIngest(t, hub, Batch{SourceID: "src-live", DeviceID: deviceHex, Events: []Event{e1}})

	plan, err := hub.PlanPurge(ctx, PurgeSelector{Session: "live-sess"})
	if err != nil {
		t.Fatal(err)
	}
	staleSource := plan.Sources[0]

	e2 := mkEvent(e1.End, rawUser("arrived during purge"))
	e2.NativeID = "live-sess"
	mustIngest(t, hub, Batch{SourceID: "src-live", DeviceID: deviceHex, Events: []Event{e2}})

	selected := map[string]bool{plan.Sessions[0].ID: true}
	committedCursor, err := hub.purgeOneSource(ctx, staleSource, selected, "session=live-sess", 5000)
	if err != nil {
		t.Fatalf("purgeOneSource: %v", err)
	}
	if committedCursor != e2.End {
		t.Fatalf("committedCursor = %d, want fresh in-tx cursor %d (stale was %d)", committedCursor, e2.End, staleSource.Cursor)
	}
	var storedCursor int64
	if err := hub.Writer().QueryRow(`SELECT cursor_bytes FROM purged_sources WHERE source_id = 'src-live'`).Scan(&storedCursor); err != nil {
		t.Fatal(err)
	}
	if storedCursor != e2.End {
		t.Fatalf("tombstone cursor_bytes = %d, want %d", storedCursor, e2.End)
	}
}

func TestPurgePreservesFirstNativeIDAcrossReallowAndReDerive(t *testing.T) {
	ctx := context.Background()
	hub := ingestHub(t)
	mustRegisterAs(t, hub, "src-codex", deviceHex, "codex", "/c/rollout.jsonl")
	raw0 := `{"type":"session_meta","payload":{"id":"codex-native-1"}}`
	e0 := Event{
		Offset: 0, End: int64(len(raw0)) + 1, NativeID: "codex-native-1",
		Kind: "session_meta", RawJSON: raw0, Metadata: "{}", HasTime: true, TimeMS: 1000, ParserVersion: 1,
	}
	mustIngest(t, hub, Batch{SourceID: "src-codex", DeviceID: deviceHex, Events: []Event{e0}})

	if _, _, err := hub.ApplyPurge(ctx, PurgeSelector{Session: "codex-native-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.ReallowSource(ctx, "src-codex"); err != nil {
		t.Fatal(err)
	}
	st, err := hub.SourceState(ctx, "src-codex")
	if err != nil {
		t.Fatalf("SourceState after reallow: %v", err)
	}
	if st.FirstNativeID != "codex-native-1" || st.Cursor != e0.End {
		t.Fatalf("SourceState = %+v, want FirstNativeID=codex-native-1 Cursor=%d", st, e0.End)
	}

	raw1 := `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"post-reallow"}]}}`
	e1 := Event{
		Offset: e0.End, End: e0.End + int64(len(raw1)) + 1, NativeID: st.FirstNativeID,
		Kind: "message", Role: "user", Text: "post-reallow", RawJSON: raw1, Metadata: "{}",
		HasTime: true, TimeMS: 2000, ParserVersion: 1,
	}
	mustIngest(t, hub, Batch{SourceID: "src-codex", DeviceID: deviceHex, Events: []Event{e1}})

	res, err := hub.ReDerive(ctx, []ParserDeriver{{
		Agent:         "codex",
		OutputVersion: 2,
		Derive: func(in DeriveInput) (Event, error) {
			if in.SourceOffset > 0 && in.FirstNativeID == "" {
				return Event{}, errors.New("missing FirstNativeID past offset 0")
			}
			return Event{
				Offset: in.SourceOffset, End: in.SourceOffset + int64(len(in.RawJSON)) + 1,
				NativeID: in.FirstNativeID, Kind: "message", Role: "user", Text: "post-reallow",
				RawJSON: in.RawJSON, Metadata: "{}", HasTime: true, TimeMS: 2000, ParserVersion: 2,
			}, nil
		},
	}})
	if err != nil {
		t.Fatalf("ReDerive after reallow failed: %v", err)
	}
	if res.Events != 1 {
		t.Fatalf("ReDerive events = %d, want 1", res.Events)
	}
}

func TestPurgePrunesOrphanedWorkspacesOnly(t *testing.T) {
	ctx := context.Background()
	hub := ingestHub(t)
	mustRegisterAs(t, hub, "src-w1", deviceHex, "claude-code", "/c/w1.jsonl")
	mustRegisterAs(t, hub, "src-w2", deviceHex, "claude-code", "/c/w2.jsonl")
	mustRegisterAs(t, hub, "src-w3", deviceHex, "claude-code", "/c/w3.jsonl")

	e1 := mkEvent(0, rawUser("a"))
	e1.NativeID, e1.Cwd = "ws-1", "/work/secret"
	e2 := mkEvent(0, rawUser("b"))
	e2.NativeID, e2.Cwd = "ws-2", "/work/shared"
	e3 := mkEvent(0, rawUser("c"))
	e3.NativeID, e3.Cwd = "ws-3", "/work/shared"
	mustIngest(t, hub, Batch{SourceID: "src-w1", DeviceID: deviceHex, Events: []Event{e1}})
	mustIngest(t, hub, Batch{SourceID: "src-w2", DeviceID: deviceHex, Events: []Event{e2}})
	mustIngest(t, hub, Batch{SourceID: "src-w3", DeviceID: deviceHex, Events: []Event{e3}})

	if _, err := hub.ObserveWorkspaces(ctx, deviceHex, []workspace.Observation{
		{Path: "/work/secret", Repo: "github.com/org/secret"},
		{Path: "/work/shared", Repo: "github.com/org/shared"},
	}); err != nil {
		t.Fatal(err)
	}

	if _, _, err := hub.ApplyPurge(ctx, PurgeSelector{Session: "ws-1"}); err != nil {
		t.Fatal(err)
	}
	if got := hubCount(t, hub, `SELECT count(*) FROM workspaces WHERE path = '/work/secret'`); got != 0 {
		t.Fatalf("orphaned workspace survived purge: %d", got)
	}
	if got := hubCount(t, hub, `SELECT count(*) FROM workspaces WHERE path = '/work/shared'`); got != 1 {
		t.Fatalf("unrelated workspace removed: %d", got)
	}

	if _, _, err := hub.ApplyPurge(ctx, PurgeSelector{Session: "ws-2"}); err != nil {
		t.Fatal(err)
	}
	if got := hubCount(t, hub, `SELECT count(*) FROM workspaces WHERE path = '/work/shared'`); got != 1 {
		t.Fatalf("workspace still referenced by ws-3 was pruned: %d", got)
	}

	if _, _, err := hub.ApplyPurge(ctx, PurgeSelector{Session: "ws-3"}); err != nil {
		t.Fatal(err)
	}
	if got := hubCount(t, hub, `SELECT count(*) FROM workspaces WHERE path = '/work/shared'`); got != 0 {
		t.Fatalf("workspace orphaned after purging ws-3 survived: %d", got)
	}
}

func TestPurgedSourceOwnershipHidesRefusalFromForeignDevice(t *testing.T) {
	ctx := context.Background()
	hub := ingestHub(t)
	otherDev := "fedcba9876543210fedcba9876543210"
	mustRegisterAs(t, hub, "src-priv", deviceHex, "claude-code", "/c/priv.jsonl")
	e := mkEvent(0, rawUser("secret"))
	e.NativeID = "sess-priv"
	mustIngest(t, hub, Batch{SourceID: "src-priv", DeviceID: deviceHex, Events: []Event{e}})
	if _, _, err := hub.ApplyPurge(ctx, PurgeSelector{Session: "sess-priv"}); err != nil {
		t.Fatal(err)
	}

	err := hub.RegisterSource(ctx, "src-priv", otherDev, "claude-code", "/c/priv.jsonl")
	if !errors.Is(err, ErrSourceOwnership) || errors.Is(err, ErrSourcePurged) {
		t.Fatalf("foreign RegisterSource err = %v, want ErrSourceOwnership only", err)
	}
	_, err = hub.SourceStateForDevice(ctx, "src-priv", otherDev)
	if !errors.Is(err, ErrSourceOwnership) || errors.Is(err, ErrSourcePurged) {
		t.Fatalf("foreign SourceStateForDevice err = %v, want ErrSourceOwnership only", err)
	}
	_, err = hub.Ingest(ctx, Batch{SourceID: "src-priv", DeviceID: otherDev, Events: []Event{e}})
	if !errors.Is(err, ErrSourceOwnership) || errors.Is(err, ErrSourcePurged) {
		t.Fatalf("foreign Ingest err = %v, want ErrSourceOwnership only", err)
	}
}
