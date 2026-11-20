package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nibzard/ohya/internal/events"
	"github.com/nibzard/ohya/internal/mcpserver"
	"github.com/nibzard/ohya/internal/store"
	"github.com/nibzard/ohya/internal/watch"
)

func purgeTestHub(t *testing.T) (string, string, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "hub.db")
	hub, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hub.Close() })
	deviceID := "0123456789abcdef0123456789abcdef"
	for _, src := range []struct {
		id   string
		file string
		nat  string
	}{{"src-1", "main-session.jsonl", ""}, {"src-2", "subagent-session.jsonl", ""}} {
		data, err := os.ReadFile(fixturePath(src.file))
		if err != nil {
			t.Fatal(err)
		}
		if err := hub.RegisterSource(context.Background(), src.id, deviceID, "claude-code", "/"+src.file); err != nil {
			t.Fatal(err)
		}
		parsed, err := events.Parse("claude-code", data, 0, events.Options{SourceScope: src.id})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := hub.Ingest(context.Background(), store.Batch{
			SourceID: src.id, DeviceID: deviceID, Events: parsed.Events,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return dbPath, deviceID, store.SessionID(deviceID, "claude-code", "00000000-0000-4000-8000-0000000000a1")
}

func TestPurgeCommandDryRunIsDefaultAndChangesNothing(t *testing.T) {
	dbPath, _, sessionID := purgeTestHub(t)
	hub, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	before := countRows(t, hub.Writer(), `SELECT count(*) FROM events`) +
		countRows(t, hub.Writer(), `SELECT count(*) FROM sources`) +
		countRows(t, hub.Writer(), `SELECT count(*) FROM purged_sources`)

	native := "00000000-0000-4000-8000-0000000000a1"
	code, out, stderr := runCLI("purge", "--db", dbPath, "--session", native)
	if code != 0 {
		t.Fatalf("purge dry run exited %d: %s %s", code, out, stderr)
	}
	for _, want := range []string{
		"would purge 1 sessions, 2 sources", "src-1", "src-2",
		"dry run only; nothing was deleted", sessionID,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry-run output missing %q:\n%s", want, out)
		}
	}
	after := countRows(t, hub.Writer(), `SELECT count(*) FROM events`) +
		countRows(t, hub.Writer(), `SELECT count(*) FROM sources`) +
		countRows(t, hub.Writer(), `SELECT count(*) FROM purged_sources`)
	if after != before {
		t.Fatalf("dry run changed row counts: %d -> %d", before, after)
	}
}

func TestPurgeCommandJSONDryRunGolden(t *testing.T) {
	dbPath, deviceID, _ := purgeTestHub(t)
	code, out, _ := runCLI("purge", "--db", dbPath,
		"--session", "00000000-0000-4000-8000-0000000000a1", "--json")
	if code != 0 {
		t.Fatalf("purge --json exited %d: %s", code, out)
	}
	got := scrubJSONNumbers(t, scrubJSONKeys(t, out, "purged_at"),
		"events", "bytes", "cursor_bytes")
	want := `{"schema_version":1,"command":"ohya purge","data":{"dry_run":true,"scope":"session=00000000-0000-4000-8000-0000000000a1","sessions":[{"id":"<sid>","agent":"claude-code","device_id":"<device>","native_id":"00000000-0000-4000-8000-0000000000a1","events":<events>}],"sources":[{"id":"src-1","agent":"claude-code","device_id":"<device>","path":"/main-session.jsonl","cursor_bytes":<cursor_bytes>,"events":<events>,"native_id":"00000000-0000-4000-8000-0000000000a1"},{"id":"src-2","agent":"claude-code","device_id":"<device>","path":"/subagent-session.jsonl","cursor_bytes":<cursor_bytes>,"events":<events>,"native_id":"00000000-0000-4000-8000-0000000000a1"}],"events":<events>,"bytes":<bytes>},"error":null}` + "\n"
	want = strings.ReplaceAll(strings.ReplaceAll(want, "<sid>", store.SessionID(deviceID, "claude-code", "00000000-0000-4000-8000-0000000000a1")), "<device>", deviceID)
	goldenCompare(t, got, want)
}

func TestPurgeCommandApplyAndRefusalRemedy(t *testing.T) {
	dbPath, deviceID, _ := purgeTestHub(t)
	code, out, stderr := runCLI("purge", "--db", dbPath,
		"--session", "00000000-0000-4000-8000-0000000000a1", "--apply")
	if code != 0 {
		t.Fatalf("purge --apply exited %d: %s %s", code, out, stderr)
	}
	if !strings.Contains(out, "purged source src-1") || !strings.Contains(out, "purged source src-2") ||
		!strings.Contains(out, "purged 1 sessions, 2 sources") {
		t.Fatalf("apply output = %q", out)
	}
	hub, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	if got := countRows(t, hub.Writer(), `SELECT count(*) FROM events WHERE source_id = 'src-1'`); got != 0 {
		t.Fatalf("events survive the applied purge: %d", got)
	}
	err = hub.RegisterSource(context.Background(), "src-1", deviceID, "claude-code", "/main-session.jsonl")
	var text string
	if err == nil {
		t.Fatal("re-register after purge succeeded")
	}
	text = err.Error()
	if !errors.Is(err, store.ErrSourcePurged) {
		t.Fatalf("re-register error = %v", err)
	}
	if !strings.Contains(text, "ohya sources reallow src-1") {
		t.Fatalf("refusal names no remedy: %q", text)
	}

	code, out, _ = runCLI("sources", "reallow", "--db", dbPath, "src-1")
	if code != 0 {
		t.Fatalf("sources reallow exited %d: %s", code, out)
	}
	if !strings.Contains(out, "re-admitted") || !strings.Contains(out, "src-1") {
		t.Fatalf("reallow output = %q", out)
	}
	err = hub.RegisterSource(context.Background(), "src-1", deviceID, "claude-code", "/main-session.jsonl")
	if err != nil {
		t.Fatalf("re-register after reallow: %v", err)
	}
	var cursor int64
	if err := hub.Writer().QueryRow(`SELECT cursor_bytes FROM sources WHERE id = 'src-1'`).Scan(&cursor); err != nil {
		t.Fatal(err)
	}
	if cursor == 0 {
		t.Fatal("reallowed source restarted at cursor 0; history would replay into the purge gap")
	}
}

func TestPurgeCommandJSONApplyGolden(t *testing.T) {
	dbPath, deviceID, _ := purgeTestHub(t)
	code, out, _ := runCLI("purge", "--db", dbPath,
		"--session", "00000000-0000-4000-8000-0000000000a1", "--apply", "--json")
	if code != 0 {
		t.Fatalf("purge --apply --json exited %d: %s", code, out)
	}
	got := scrubJSONNumbers(t, scrubJSONKeys(t, out, "purged_at"),
		"events", "bytes", "cursor_bytes")
	want := strings.ReplaceAll(strings.ReplaceAll(
		`{"schema_version":1,"command":"ohya purge","data":{"dry_run":false,"scope":"session=00000000-0000-4000-8000-0000000000a1","sessions":[{"id":"<sid>","agent":"claude-code","device_id":"<device>","native_id":"00000000-0000-4000-8000-0000000000a1","events":<events>}],"sources":[{"id":"src-1","agent":"claude-code","device_id":"<device>","path":"/main-session.jsonl","cursor_bytes":<cursor_bytes>,"events":<events>,"native_id":"00000000-0000-4000-8000-0000000000a1","purged_at":"<purged_at>"},{"id":"src-2","agent":"claude-code","device_id":"<device>","path":"/subagent-session.jsonl","cursor_bytes":<cursor_bytes>,"events":<events>,"native_id":"00000000-0000-4000-8000-0000000000a1","purged_at":"<purged_at>"}],"events":<events>,"bytes":<bytes>},"error":null}`+"\n",
		"<sid>", store.SessionID(deviceID, "claude-code", "00000000-0000-4000-8000-0000000000a1")), "<device>", deviceID)
	goldenCompare(t, got, want)
}

func TestPurgeCommandUsageErrors(t *testing.T) {
	dbPath, _, _ := purgeTestHub(t)
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"purge", "--db", dbPath}, "purge needs at least one selector"},
		{[]string{"purge", "--db", dbPath, "--before", "2026-13-01"}, "--before must be an RFC3339 date"},
		{[]string{"purge", "--db", dbPath, "--session", "x", "extra"}, "unexpected argument"},
		{[]string{"purge", "--db", dbPath, "--marked", "garbage"}, "must be namespace.key, for example tags.garbage"},
		{[]string{"purge", "--db", dbPath, "--marked", "tags."}, "must be namespace.key, for example tags.garbage"},
		{[]string{"purge", "--db", dbPath, "--marked", ".garbage"}, "must be namespace.key, for example tags.garbage"},
		{[]string{"purge", "--db", dbPath, "--marked", "ta gs.garbage"}, "holds an invalid namespace or key"},
	}
	for _, tc := range cases {
		code, _, stderr := runCLI(tc.args...)
		if code != 2 {
			t.Fatalf("%v exited %d, want 2", tc.args, code)
		}
		if !strings.Contains(stderr, tc.want) {
			t.Fatalf("%v stderr = %q, want %q", tc.args, stderr, tc.want)
		}
	}
}

func markedPurgeHub(t *testing.T) (string, []string) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "hub.db")
	hub, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hub.Close() })
	deviceID := "0123456789abcdef0123456789abcdef"
	natives := []string{"mark-a", "mark-b", "keep-c"}
	for _, native := range natives {
		raw := fmt.Sprintf(`{"type":"user","sessionId":%q,"message":{"role":"user","content":"session %s"}}`, native, native)
		sourceID := "src-" + native
		if err := hub.RegisterSource(ctx, sourceID, deviceID, "claude-code", "/"+native+".jsonl"); err != nil {
			t.Fatal(err)
		}
		if _, err := hub.Ingest(ctx, store.Batch{
			SourceID: sourceID, DeviceID: deviceID,
			Events: []store.Event{{
				Offset: 0, End: int64(len(raw)) + 1, NativeID: native,
				Kind: "user", Role: "user", Text: "session " + native, RawJSON: raw, ParserVersion: 1,
			}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	return dbPath, natives
}

func TestPurgeMarkedEndToEnd(t *testing.T) {
	ctx := context.Background()
	dbPath, natives := markedPurgeHub(t)
	deviceID := "0123456789abcdef0123456789abcdef"
	hub, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	readers, err := hub.Reader()
	if err != nil {
		t.Fatal(err)
	}
	defer readers.Close()
	engine := mcpserver.NewEngine(readers, hub.Writer(), mcpserver.EngineOptions{WriterID: deviceID})

	marked := natives[:2]
	for _, native := range marked {
		var eventID int64
		if err := hub.Writer().QueryRow(
			`SELECT e.id FROM events e JOIN sessions s ON s.id = e.session_id WHERE s.native_id = ?`,
			native).Scan(&eventID); err != nil {
			t.Fatal(err)
		}
		args := fmt.Sprintf(`{"operations":[{"op":"upsert","event_id":%d,"namespace":"tags","key":"garbage","value":{"reason":"e2e audit","evidence":%d}}]}`,
			eventID, eventID)
		res, err := engine.Annotate(ctx, json.RawMessage(args))
		if err != nil {
			t.Fatalf("annotate %s: %v", native, err)
		}
		if res.Applied != 1 {
			t.Fatalf("annotate %s applied %d operations, want 1", native, res.Applied)
		}
	}

	markedA := store.SessionID(deviceID, "claude-code", natives[0])
	markedB := store.SessionID(deviceID, "claude-code", natives[1])
	keeper := store.SessionID(deviceID, "claude-code", natives[2])

	code, out, stderr := runCLI("purge", "--db", dbPath, "--marked", "tags.garbage")
	if code != 0 {
		t.Fatalf("marked dry run exited %d: %s %s", code, out, stderr)
	}
	for _, want := range []string{
		"would purge 2 sessions", "dry run only; nothing was deleted", markedA, markedB,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("marked dry-run output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, keeper) {
		t.Fatalf("marked dry-run output names the unmarked session %s:\n%s", keeper, out)
	}

	code, out, stderr = runCLI("purge", "--db", dbPath, "--marked", "tags.garbage", "--apply")
	if code != 0 {
		t.Fatalf("marked apply exited %d: %s %s", code, out, stderr)
	}
	if !strings.Contains(out, "purged 2 sessions") {
		t.Fatalf("marked apply output = %q", out)
	}

	if got := countRows(t, hub.Writer(), `SELECT count(*) FROM events WHERE session_id = ?`, keeper); got != 1 {
		t.Fatalf("unmarked session lost events: %d", got)
	}
	if got := countRows(t, hub.Writer(), `SELECT count(*) FROM sessions WHERE id = ?`, keeper); got != 1 {
		t.Fatalf("unmarked session row lost: %d", got)
	}
	if err := hub.RegisterSource(ctx, "src-"+natives[2], deviceID, "claude-code", "/"+natives[2]+".jsonl"); err != nil {
		t.Fatalf("unmarked source was evicted: %v", err)
	}
	for _, sid := range []string{markedA, markedB} {
		if got := countRows(t, hub.Writer(), `SELECT count(*) FROM events WHERE session_id = ?`, sid); got != 0 {
			t.Fatalf("marked session %s kept events: %d", sid, got)
		}
		if got := countRows(t, hub.Writer(), `SELECT count(*) FROM sessions WHERE id = ?`, sid); got != 0 {
			t.Fatalf("marked session %s kept its row: %d", sid, got)
		}
	}
	if got := countRows(t, hub.Writer(), `SELECT count(*) FROM purged_sources WHERE scope = 'marked=tags.garbage'`); got != 2 {
		t.Fatalf("purged_sources tombstones with marked scope = %d, want 2", got)
	}
	if got := countRows(t, hub.Writer(), `SELECT count(*) FROM event_annotations WHERE namespace = 'tags' AND key = 'garbage'`); got != 0 {
		t.Fatalf("garbage marks survived the purge: %d", got)
	}

	code, out, _ = runCLI("purge", "--db", dbPath, "--marked", "tags.garbage", "--json")
	if code != 0 {
		t.Fatalf("rerun dry run exited %d: %s", code, out)
	}
	if !strings.Contains(out, `"scope":"marked=tags.garbage"`) || !strings.Contains(out, `"sessions":[]`) {
		t.Fatalf("rerun reports a marked scope with nothing left:\n%s", out)
	}
}

func TestPurgeCommandNoMatch(t *testing.T) {
	dbPath, _, _ := purgeTestHub(t)
	code, out, stderr := runCLI("purge", "--db", dbPath, "--session", "no-such-session", "--apply")
	if code != 0 {
		t.Fatalf("no-match apply exited %d: %s %s", code, out, stderr)
	}
	if !strings.Contains(out, "purged 0 sessions, 0 sources, 0 events, 0 bytes") {
		t.Fatalf("no-match output = %q", out)
	}
}

func TestSourcesReallowErrors(t *testing.T) {
	dbPath, _, _ := purgeTestHub(t)
	code, _, stderr := runCLI("sources", "reallow", "--db", dbPath, "src-1")
	if code != 1 {
		t.Fatalf("reallow of a live source exited %d", code)
	}
	if !strings.Contains(stderr, "no purged source with this ID") {
		t.Fatalf("stderr = %q", stderr)
	}

	var stdout bytes.Buffer
	err := runSourcesReallow([]string{"--db", dbPath, "--json", "src-1"}, &stdout, &bytes.Buffer{})
	if err == nil {
		t.Fatal("reallow --json of a live source succeeded")
	}
	want := `{"schema_version":1,"command":"ohya sources reallow","data":null,"error":{"code":"source_not_purged","message":` +
		`"store: reallow src-1: no purged source with this ID: store: source is not purged",` +
		`"hint":"run ohya purge with the selectors of the purge; its report prints the purged source IDs"}}` + "\n"
	goldenCompare(t, stdout.String(), want)

	code, _, stderr = runCLI("sources", "reallow", "--db", dbPath)
	if code != 2 || !strings.Contains(stderr, "reallow needs exactly one source ID") {
		t.Fatalf("reallow without ID: code %d stderr %q", code, stderr)
	}
	code, _, stderr = runCLI("sources", "bogus")
	if code != 2 || !strings.Contains(stderr, "unknown sources subcommand") {
		t.Fatalf("unknown subcommand: code %d stderr %q", code, stderr)
	}
}

func TestSourcesReallowJSONGolden(t *testing.T) {
	dbPath, _, _ := purgeTestHub(t)
	hub, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	if _, _, err := hub.ApplyPurge(context.Background(), store.PurgeSelector{Agent: "claude-code"}); err != nil {
		t.Fatal(err)
	}
	code, out, _ := runCLI("sources", "reallow", "--db", dbPath, "--json", "src-1")
	if code != 0 {
		t.Fatalf("reallow --json exited %d: %s", code, out)
	}
	got := scrubJSONNumbers(t, out, "cursor_bytes")
	goldenCompare(t, got, `{"schema_version":1,"command":"ohya sources reallow","data":{"source_id":"src-1","cursor_bytes":<cursor_bytes>},"error":null}`+"\n")
}

func TestPurgeAndSourcesHelp(t *testing.T) {
	for _, args := range [][]string{{"purge", "--help"}, {"sources", "--help"}, {"sources", "reallow", "--help"}} {
		code, out, stderr := runCLI(args...)
		if code != 0 {
			t.Fatalf("%v exited %d: %s", args, code, stderr)
		}
		if !strings.Contains(out, "Usage:") {
			t.Fatalf("%v printed no usage: %q", args, out)
		}
	}
	code, _, stderr := runCLI("purge", "--bogus")
	if code != 2 || !strings.Contains(stderr, "ohya purge --help") {
		t.Fatalf("unknown flag: code %d stderr %q", code, stderr)
	}
}

func TestLocalIngestDoesNotSpamPurgedSource(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "hub.db")
	hub, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()

	srcDir := filepath.Join(tmp, "claude")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(fixturePath("main-session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(srcDir, "sess.jsonl")
	if err := os.WriteFile(file, data, 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := watch.New(filepath.Join(tmp, "state"), "claude-code", watch.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	ing := newIngestor(hub, w, "claude-code", "0123456789abcdef0123456789abcdef", 0, nil, &stderr, nil, nil)
	dirs := []agentDir{{dir: srcDir}}
	if _, err := ing.poll(ctx, dirs, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := hub.ApplyPurge(ctx, store.PurgeSelector{Agent: "claude-code"}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	if _, err := ing.poll(ctx, dirs, false); err != nil {
		t.Fatal(err)
	}
	firstLog := stderr.String()
	if !strings.Contains(firstLog, "was purged") {
		t.Fatalf("first post-purge poll stderr = %q, want purge refusal", firstLog)
	}
	if _, err := ing.poll(ctx, dirs, false); err != nil {
		t.Fatal(err)
	}
	if stderr.String() != firstLog {
		t.Fatalf("second post-purge poll spammed stderr:\n%s", stderr.String())
	}
}
