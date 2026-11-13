package events

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/nibzard/ohya/internal/codex"
	"github.com/nibzard/ohya/internal/store"
)

const (
	device     = "0123456789abcdef0123456789abcdef"
	mainThread = "00000000-0000-7000-8000-0000000000c1"
	subThread  = "00000000-0000-7000-8000-0000000000d1"
)

func codexFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "codex", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestFromCodexUsage(t *testing.T) {
	tests := []struct {
		usage string
		want  store.Usage
		has   bool
	}{
		{`{"input_tokens":1300,"cached_input_tokens":1250,"output_tokens":30,"reasoning_output_tokens":5}`,
			store.Usage{InputTokens: 50, CacheReadInputTokens: 1250, OutputTokens: 30}, true},
		{`{"input_tokens":10}`, store.Usage{InputTokens: 10}, true},
		{`{"input_tokens":10,"cached_input_tokens":11}`, store.Usage{}, false},
		{`{"input_tokens":10,"output_tokens":-1}`, store.Usage{}, false},
		{`{"input_tokens":"ten"}`, store.Usage{}, false},
		{`[]`, store.Usage{}, false},
	}
	for _, tc := range tests {
		e := FromCodex(codex.Record{Offset: 1, End: 3, Raw: "{}", Native: "t", ResponseID: "r", Usage: []byte(tc.usage)})
		if e.HasUsage != tc.has || e.Usage != tc.want || e.MessageID != "r" {
			t.Errorf("%s: usage = (%t, %+v), want (%t, %+v)", tc.usage, e.HasUsage, e.Usage, tc.has, tc.want)
		}
	}
}

func TestParseDispatch(t *testing.T) {
	if !Supported("claude-code") || !Supported("codex") || Supported("pi") {
		t.Fatal("Supported does not match the parsers")
	}
	if _, err := Parse("pi", []byte("{}\n"), 0, Options{SourceScope: "s"}); err == nil {
		t.Fatal("unknown agent parsed")
	}
	data := codexFixture(t, "malformed-line.jsonl")
	res, err := Parse("codex", data, 0, Options{SourceScope: "s"})
	var stop *StopError
	if !errors.As(err, &stop) || stop.Offset != 502 || res.Cursor != 502 || len(res.Events) != 2 {
		t.Fatalf("codex stop = %v, cursor %d, %d events", err, res.Cursor, len(res.Events))
	}
	var perr *codex.ParseError
	if !errors.As(err, &perr) {
		t.Fatal("StopError hides the parser's error")
	}
	// Bytes past offset 0 without the first native are a caller error, not
	// a source stop.
	_, err = Parse("codex", data[286:], 286, Options{SourceScope: "s"})
	if !errors.Is(err, codex.ErrNoNative) || errors.As(err, &stop) {
		t.Fatalf("missing native error = %v", err)
	}
	_, err = Parse("claude-code", []byte("{bad\n"), 9, Options{SourceScope: "s"})
	if !errors.As(err, &stop) || stop.Offset != 9 {
		t.Fatalf("claude-code stop = %v", err)
	}
}

// ingestFile parses one fixture the way local ingestion does — in chunks,
// each chunk past offset 0 using the first native from the store — and
// commits it as one source.
func ingestFile(t *testing.T, hub *store.DB, source, name string, chunk int) store.IngestResult {
	t.Helper()
	ctx := context.Background()
	if err := hub.RegisterSource(ctx, source, device, "codex", "/"+name); err != nil {
		t.Fatal(err)
	}
	data := codexFixture(t, name)
	var total store.IngestResult
	cursor := int64(0)
	for {
		st, err := hub.SourceState(ctx, source)
		if err != nil {
			t.Fatal(err)
		}
		end := min(len(data), int(cursor)+chunk)
		res, err := Parse("codex", data[cursor:end], cursor, Options{SourceScope: source, FirstNative: st.FirstNativeID})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Events) == 0 {
			if end == len(data) {
				return total
			}
			chunk *= 2 // the chunk holds part of one record; widen it
			continue
		}
		r, err := hub.Ingest(ctx, store.Batch{SourceID: source, DeviceID: device, Events: res.Events})
		if err != nil {
			t.Fatal(err)
		}
		total.Inserted += r.Inserted
		total.Cursor = r.Cursor
		cursor = r.Cursor
	}
}

// TestCodexFixturesThroughWritePath ingests the main session, its resumed
// page and its subagent: the page joins the main thread's session and its
// replayed ordinals are stored once as originals, the subagent is its own
// session, and token totals count each response once.
func TestCodexFixturesThroughWritePath(t *testing.T) {
	for _, chunk := range []int{1 << 20, 700} {
		hub, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hub.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer hub.Close()
		if r := ingestFile(t, hub, "src-main", "main-session.jsonl", chunk); r.Inserted != 14 || r.Cursor != 4780 {
			t.Fatalf("chunk %d: main = %+v", chunk, r)
		}
		if r := ingestFile(t, hub, "src-page", "resumed-page.jsonl", chunk); r.Inserted != 5 || r.Cursor != 2048 {
			t.Fatalf("chunk %d: page = %+v", chunk, r)
		}
		if r := ingestFile(t, hub, "src-sub", "subagent-session.jsonl", chunk); r.Inserted != 5 || r.Cursor != 2362 {
			t.Fatalf("chunk %d: subagent = %+v", chunk, r)
		}
		db := hub.Writer()

		type session struct {
			native, cwd, branch, model, prompt string
			count                              int
		}
		rows, err := db.Query(`SELECT native_id, coalesce(cwd,''), coalesce(branch,''), coalesce(model,''),
		       coalesce(first_prompt,''), event_count FROM sessions ORDER BY native_id`)
		if err != nil {
			t.Fatal(err)
		}
		var got []session
		for rows.Next() {
			var s session
			if err := rows.Scan(&s.native, &s.cwd, &s.branch, &s.model, &s.prompt, &s.count); err != nil {
				t.Fatal(err)
			}
			got = append(got, s)
		}
		rows.Close()
		want := []session{
			// 14 main records, plus the page's meta and two new records.
			{mainThread, "/home/dev/example", "main", "gpt-5-codex", "Add a Codex section to the README.", 17},
			// The embedded parent meta lends no branch.
			{subThread, "/home/dev/example", "", "", "", 5},
		}
		if len(got) != len(want) {
			t.Fatalf("chunk %d: sessions = %+v", chunk, got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("chunk %d: session %d = %+v, want %+v", chunk, i, got[i], want[i])
			}
		}

		// The page's replays of ordinals 12 and 13 are duplicates of the
		// main file's records.
		var dups int
		if err := db.QueryRow(`SELECT count(*) FROM events d JOIN events o ON o.id = d.duplicate_of
		    WHERE d.source_id = 'src-page' AND o.source_id = 'src-main' AND d.ordinal = o.ordinal
		      AND d.raw_json = o.raw_json AND d.ordinal IN (12, 13)`).Scan(&dups); err != nil || dups != 2 {
			t.Fatalf("chunk %d: page duplicates = %d, %v; want 2", chunk, dups, err)
		}
		var originals int
		if err := db.QueryRow(`SELECT count(*) FROM events WHERE duplicate_of IS NULL`).Scan(&originals); err != nil || originals != 22 {
			t.Fatalf("chunk %d: original records = %d, %v; want 22", chunk, originals, err)
		}

		// Two main responses and one subagent response; the replayed usage
		// record adds nothing.
		var input, cached, output int64
		if err := db.QueryRow(`SELECT sum(input_tokens), sum(cache_read_input_tokens), sum(output_tokens)
		    FROM session_token_usage`).Scan(&input, &cached, &output); err != nil {
			t.Fatal(err)
		}
		if input != (1200-800)+(1300-1250)+(600-400) || cached != 800+1250+400 || output != 48+30+25 {
			t.Fatalf("chunk %d: tokens = %d/%d/%d", chunk, input, cached, output)
		}

		// Search finds the transcript text once: the response_item, not
		// its event_msg mirror.
		var hits int
		if err := db.QueryRow(`SELECT count(*) FROM events_fts WHERE events_fts MATCH '"no Codex section yet"'`).Scan(&hits); err != nil || hits != 1 {
			t.Fatalf("chunk %d: search hits = %d, %v; want 1", chunk, hits, err)
		}
		if _, err := db.Exec(`INSERT INTO events_fts(events_fts) VALUES ('integrity-check')`); err != nil {
			t.Fatalf("chunk %d: FTS integrity: %v", chunk, err)
		}
	}
}
