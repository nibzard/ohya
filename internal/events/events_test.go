package events

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nibzard/ohya/internal/codex"
	"github.com/nibzard/ohya/internal/pi"
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
		{`{"input_tokens":1300,"cached_input_tokens":250,"cache_write_input_tokens":400,"output_tokens":30}`,
			store.Usage{InputTokens: 1050, CacheReadInputTokens: 250, OutputTokens: 30}, true},
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
	if !Supported("claude-code") || !Supported("codex") || !Supported("pi") || Supported("opencode") {
		t.Fatal("Supported does not match the parsers")
	}
	if _, err := Parse("opencode", []byte("{}\n"), 0, Options{SourceScope: "s"}); err == nil {
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
	_, err = Parse("codex", data[286:], 286, Options{SourceScope: "s"})
	if !errors.Is(err, codex.ErrNoNative) || errors.As(err, &stop) {
		t.Fatalf("missing native error = %v", err)
	}
	_, err = Parse("claude-code", []byte("{bad\n"), 9, Options{SourceScope: "s"})
	if !errors.As(err, &stop) || stop.Offset != 9 {
		t.Fatalf("claude-code stop = %v", err)
	}
	piData, err := os.ReadFile(filepath.Join("..", "..", "testdata", "pi", "malformed-line.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	res, err = Parse("pi", piData, 0, Options{SourceScope: "s"})
	if !errors.As(err, &stop) || stop.Offset != 283 || res.Cursor != 283 || len(res.Events) != 2 {
		t.Fatalf("pi stop = %v, cursor %d, %d events", err, res.Cursor, len(res.Events))
	}
	var piErr *pi.ParseError
	if !errors.As(err, &piErr) {
		t.Fatal("StopError hides the pi parser's error")
	}
	if _, err = Parse("pi", piData[143:], 143, Options{SourceScope: "s"}); !errors.Is(err, pi.ErrNoNative) {
		t.Fatalf("pi missing native error = %v", err)
	}
}

func TestParseMaxRecordBytesAndStopClassification(t *testing.T) {
	small := `{"type":"user","sessionId":"s1","message":{"role":"user","content":"hi"}}` + "\n"
	big := `{"type":"user","sessionId":"s1","message":{"role":"user","content":"` +
		strings.Repeat("x", 64) + `"}}` + "\n"

	for _, agent := range Agents {
		data := []byte(small + big)
		res, err := Parse(agent, data, 0, Options{SourceScope: "s", MaxRecordBytes: 128})
		var stop *StopError
		if !errors.As(err, &stop) || !stop.RecordTooLarge {
			t.Fatalf("%s with a small limit: %v", agent, err)
		}
		if len(res.Events) != 1 {
			t.Fatalf("%s: %d records before the stop, want 1", agent, len(res.Events))
		}
		res, err = Parse(agent, data, 0, Options{SourceScope: "s", MaxRecordBytes: 4096})
		if err != nil || len(res.Events) != 2 {
			t.Fatalf("%s with a larger limit: %v, %d records", agent, err, len(res.Events))
		}
	}

	_, err := Parse("claude-code", []byte(small+"{bad\n"), 0, Options{SourceScope: "s"})
	var stop *StopError
	if !errors.As(err, &stop) || stop.RecordTooLarge {
		t.Fatalf("malformed stop = %v, want RecordTooLarge false", err)
	}
}

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
			chunk *= 2
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
			{mainThread, "/home/dev/example", "main", "gpt-5-codex", "Add a Codex section to the README.", 17},
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

		var input, cached, output int64
		if err := db.QueryRow(`SELECT sum(input_tokens), sum(cache_read_input_tokens), sum(output_tokens)
		    FROM session_token_usage`).Scan(&input, &cached, &output); err != nil {
			t.Fatal(err)
		}
		if input != (1200-800)+(1300-1250)+(600-400) || cached != 800+1250+400 || output != 48+30+25 {
			t.Fatalf("chunk %d: tokens = %d/%d/%d", chunk, input, cached, output)
		}

		var hits int
		if err := db.QueryRow(`SELECT count(*) FROM events_fts WHERE events_fts MATCH '"no Codex section yet"'`).Scan(&hits); err != nil || hits != 1 {
			t.Fatalf("chunk %d: search hits = %d, %v; want 1", chunk, hits, err)
		}
		if _, err := db.Exec(`INSERT INTO events_fts(events_fts) VALUES ('integrity-check')`); err != nil {
			t.Fatalf("chunk %d: FTS integrity: %v", chunk, err)
		}
	}
}

func TestCodexReenumeratedPageThroughWritePath(t *testing.T) {
	for _, chunk := range []int{1 << 20, 700} {
		hub, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hub.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer hub.Close()
		if r := ingestFile(t, hub, "src-main", "main-session.jsonl", chunk); r.Inserted != 14 || r.Cursor != 4780 {
			t.Fatalf("chunk %d: main = %+v", chunk, r)
		}
		if r := ingestFile(t, hub, "src-reenum", "reenumerated-page.jsonl", chunk); r.Inserted != 5 || r.Cursor != 2073 {
			t.Fatalf("chunk %d: re-enumerated page = %+v", chunk, r)
		}
		if r := ingestFile(t, hub, "src-page", "resumed-page.jsonl", chunk); r.Inserted != 5 || r.Cursor != 2048 {
			t.Fatalf("chunk %d: replay page = %+v", chunk, r)
		}
		db := hub.Writer()

		var dups int
		if err := db.QueryRow(`SELECT count(*) FROM events d JOIN events o ON o.id = d.duplicate_of
		    WHERE d.source_id = 'src-page' AND d.raw_json = o.raw_json AND d.ordinal IN (12, 13)`).Scan(&dups); err != nil || dups != 2 {
			t.Fatalf("chunk %d: byte-replay duplicates = %d, %v; want 2", chunk, dups, err)
		}
		var reenumDups int
		if err := db.QueryRow(`SELECT count(*) FROM events WHERE source_id = 'src-reenum' AND duplicate_of IS NOT NULL`).Scan(&reenumDups); err != nil || reenumDups != 0 {
			t.Fatalf("chunk %d: re-enumerated page suppressed %d records, want 0", chunk, reenumDups)
		}
		var originals int
		if err := db.QueryRow(`SELECT count(*) FROM events WHERE duplicate_of IS NULL`).Scan(&originals); err != nil || originals != 22 {
			t.Fatalf("chunk %d: originals = %d, %v; want 22", chunk, originals, err)
		}

		var input, cached, output int64
		if err := db.QueryRow(`SELECT sum(input_tokens), sum(cache_read_input_tokens), sum(output_tokens)
		    FROM session_token_usage`).Scan(&input, &cached, &output); err != nil {
			t.Fatal(err)
		}
		if input != (1200-800)+(1300-1250)+(1400-900) || cached != 800+1250+900 || output != 48+30+60 {
			t.Fatalf("chunk %d: tokens = %d/%d/%d; want all three responses", chunk, input, cached, output)
		}

		var hits int
		if err := db.QueryRow(`SELECT count(*) FROM events_fts WHERE events_fts MATCH '"rolled back"'`).Scan(&hits); err != nil || hits != 1 {
			t.Fatalf("chunk %d: re-enumerated answer hits = %d, %v; want 1", chunk, hits, err)
		}
		if _, err := db.Exec(`INSERT INTO events_fts(events_fts) VALUES ('integrity-check')`); err != nil {
			t.Fatalf("chunk %d: FTS integrity: %v", chunk, err)
		}
	}
}

func TestFromPiUsage(t *testing.T) {
	tests := []struct {
		usage string
		want  store.Usage
		has   bool
	}{
		{`{"input":1840,"output":24,"cacheRead":0,"cacheWrite":0,"reasoning":0,"totalTokens":1864}`,
			store.Usage{InputTokens: 1840, OutputTokens: 24}, true},
		{`{"input":345,"output":41,"cacheRead":1536,"cacheWrite":0,"reasoning":0,"totalTokens":1922}`,
			store.Usage{InputTokens: 345, CacheReadInputTokens: 1536, OutputTokens: 41}, true},
		{`{"input":3,"output":186,"cacheRead":0,"cacheWrite":5429,"totalTokens":5618}`,
			store.Usage{InputTokens: 3, OutputTokens: 186, CacheCreationInputTokens: 5429}, true},
		{`{"input":2,"output":53,"cacheRead":5618,"cacheWrite":7143,"cacheWrite1h":0,"totalTokens":12816}`,
			store.Usage{InputTokens: 2, CacheReadInputTokens: 5618, CacheCreationInputTokens: 7143, OutputTokens: 53}, true},
		{`{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"reasoning":0,"totalTokens":0}`,
			store.Usage{}, true},
		{`{"input":10,"output":5,"cacheRead":0,"cacheWrite":0,"reasoning":5,"totalTokens":15}`,
			store.Usage{InputTokens: 10, OutputTokens: 5}, true},
		{`{"input":10,"output":2,"cacheRead":0,"totalTokens":11}`, store.Usage{}, false},
		{`{"input":3,"output":186,"cacheRead":0,"cacheWrite":5000,"totalTokens":5618}`, store.Usage{}, false},
		{`{"input":10}`, store.Usage{}, false},
		{`{"input":-1,"output":1,"cacheRead":0,"totalTokens":0}`, store.Usage{}, false},
		{`{"input":3,"output":186,"cacheRead":0,"cacheWrite":-1,"totalTokens":188}`, store.Usage{}, false},
		{`{"input":"ten"}`, store.Usage{}, false},
		{`[]`, store.Usage{}, false},
	}
	for _, tc := range tests {
		e := FromPi(pi.Record{Offset: 1, End: 3, Raw: "{}", Native: "s", MessageID: "r", Usage: []byte(tc.usage)})
		if e.HasUsage != tc.has || e.Usage != tc.want {
			t.Errorf("%s: usage = (%t, %+v), want (%t, %+v)", tc.usage, e.HasUsage, e.Usage, tc.has, tc.want)
		}
	}
}

func TestFromPiUsageKey(t *testing.T) {
	tests := []struct {
		name        string
		messageID   string
		entryID     string
		wantMessage string
	}{
		{"responseId wins", "resp_1", "ab000001", "resp_1"},
		{"entry id falls back", "", "ab000001", "ab000001"},
		{"neither ids the response", "", "", ""},
	}
	for _, tc := range tests {
		e := FromPi(pi.Record{Native: "s", MessageID: tc.messageID, EntryID: tc.entryID})
		if e.MessageID != tc.wantMessage {
			t.Errorf("%s: message id = %q, want %q", tc.name, e.MessageID, tc.wantMessage)
		}
	}
}

func ingestPiFixture(t *testing.T, hub *store.DB, source, name string, chunk int) store.IngestResult {
	t.Helper()
	ctx := context.Background()
	if err := hub.RegisterSource(ctx, source, device, "pi", "/"+name); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "pi", name))
	if err != nil {
		t.Fatal(err)
	}
	var total store.IngestResult
	cursor := int64(0)
	for {
		st, err := hub.SourceState(ctx, source)
		if err != nil {
			t.Fatal(err)
		}
		end := min(len(data), int(cursor)+chunk)
		res, err := Parse("pi", data[cursor:end], cursor, Options{SourceScope: source, FirstNative: st.FirstNativeID})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Events) == 0 {
			if end == len(data) {
				return total
			}
			chunk *= 2
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

func TestPiFixturesThroughWritePath(t *testing.T) {
	const (
		mainSess  = "00000000-0000-7000-8000-0000000000b1"
		errorSess = "00000000-0000-7000-8000-0000000000b2"
	)
	for _, chunk := range []int{1 << 20, 400} {
		hub, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hub.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer hub.Close()
		ingest := func(source, name string) store.IngestResult {
			return ingestPiFixture(t, hub, source, name, chunk)
		}
		if r := ingest("src-main", "main-session.jsonl"); r.Inserted != 8 || r.Cursor != 2953 {
			t.Fatalf("chunk %d: main = %+v", chunk, r)
		}
		if r := ingest("src-err", "error-session.jsonl"); r.Inserted != 5 || r.Cursor != 1205 {
			t.Fatalf("chunk %d: error = %+v", chunk, r)
		}
		db := hub.Writer()

		type session struct {
			native, cwd, model, prompt string
			count                      int
		}
		rows, err := db.Query(`SELECT native_id, coalesce(cwd,''), coalesce(model,''),
		       coalesce(first_prompt,''), event_count FROM sessions ORDER BY native_id`)
		if err != nil {
			t.Fatal(err)
		}
		var got []session
		for rows.Next() {
			var s session
			if err := rows.Scan(&s.native, &s.cwd, &s.model, &s.prompt, &s.count); err != nil {
				t.Fatal(err)
			}
			got = append(got, s)
		}
		rows.Close()
		want := []session{
			{mainSess, "/home/dev/pi-project", "gpt-5.4-nano", "Use your read or list tool once to see the file names in the current working directory, then reply with one short sentence that names them.", 8},
			{errorSess, "/home/dev/pi-project", "glm-5.3", "List the file names in the current working directory with a tool, then reply with one short sentence that names them.", 5},
		}
		if len(got) != len(want) {
			t.Fatalf("chunk %d: sessions = %+v", chunk, got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("chunk %d: session %d = %+v, want %+v", chunk, i, got[i], want[i])
			}
		}

		var rows2 int
		if err := db.QueryRow(`SELECT count(*) FROM session_token_usage`).Scan(&rows2); err != nil || rows2 != 3 {
			t.Fatalf("chunk %d: token rows = %d, %v; want 3", chunk, rows2, err)
		}
		var input, cached, output int64
		if err := db.QueryRow(`SELECT sum(input_tokens), sum(cache_read_input_tokens), sum(output_tokens)
		    FROM session_token_usage`).Scan(&input, &cached, &output); err != nil {
			t.Fatal(err)
		}
		if input != 1840+345 || cached != 1536 || output != 24+41 {
			t.Fatalf("chunk %d: tokens = %d/%d/%d", chunk, input, cached, output)
		}

		for _, q := range []struct {
			match string
			hits  int
		}{
			{`"coding assistant running inside pi"`, 0},
			{`"file names in the current working directory"`, 2},
			{`"directory contains"`, 1},
			{`"timeout 10"`, 1},
		} {
			var hits int
			if err := db.QueryRow(`SELECT count(*) FROM events_fts WHERE events_fts MATCH ?`, q.match).Scan(&hits); err != nil || hits != q.hits {
				t.Fatalf("chunk %d: search %s = %d hits, %v; want %d", chunk, q.match, hits, err, q.hits)
			}
		}
		if _, err := db.Exec(`INSERT INTO events_fts(events_fts) VALUES ('integrity-check')`); err != nil {
			t.Fatalf("chunk %d: FTS integrity: %v", chunk, err)
		}
	}
}

func TestPiAnthropicFixtureThroughWritePath(t *testing.T) {
	const anthropicSess = "00000000-0000-7000-8000-0000000000b5"
	for _, chunk := range []int{1 << 20, 300} {
		hub, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hub.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer hub.Close()
		if r := ingestPiFixture(t, hub, "src-anthropic", "anthropic-session.jsonl", chunk); r.Inserted != 7 || r.Cursor != 2341 {
			t.Fatalf("chunk %d: anthropic = %+v", chunk, r)
		}
		db := hub.Writer()

		var native, cwd, model string
		var count int
		if err := db.QueryRow(`SELECT native_id, coalesce(cwd,''), coalesce(model,''), event_count
		    FROM sessions WHERE id = ?`, store.SessionID(device, "pi", anthropicSess)).
			Scan(&native, &cwd, &model, &count); err != nil || native != anthropicSess ||
			cwd != "/home/dev/pi-anthropic" || model != "claude-opus-4-8" || count != 7 {
			t.Fatalf("chunk %d: session = %q/%q/%q/%d, %v", chunk, native, cwd, model, count, err)
		}

		rows, err := db.Query(`SELECT message_id, input_tokens, cache_creation_input_tokens,
		       cache_read_input_tokens, output_tokens
		    FROM session_token_usage ORDER BY message_id`)
		if err != nil {
			t.Fatal(err)
		}
		type usage struct {
			id     string
			input  int64
			create int64
			read   int64
			output int64
		}
		var got []usage
		for rows.Next() {
			var u usage
			if err := rows.Scan(&u.id, &u.input, &u.create, &u.read, &u.output); err != nil {
				t.Fatal(err)
			}
			got = append(got, u)
		}
		rows.Close()
		want := []usage{
			{"0a1b2c04", 3, 5429, 0, 186},
			{"msg_fixture_0002", 2, 7143, 5618, 53},
		}
		if len(got) != len(want) {
			t.Fatalf("chunk %d: usage rows = %+v, want %+v", chunk, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("chunk %d: usage row %d = %+v, want %+v", chunk, i, got[i], want[i])
			}
		}

		var input, create, read, output int64
		if err := db.QueryRow(`SELECT sum(input_tokens), sum(cache_creation_input_tokens),
		       sum(cache_read_input_tokens), sum(output_tokens) FROM session_token_usage`).
			Scan(&input, &create, &read, &output); err != nil {
			t.Fatal(err)
		}
		if input != 3+2 || create != 5429+7143 || read != 5618 || output != 186+53 {
			t.Fatalf("chunk %d: tokens = %d/%d/%d/%d", chunk, input, create, read, output)
		}
		if _, err := db.Exec(`INSERT INTO events_fts(events_fts) VALUES ('integrity-check')`); err != nil {
			t.Fatalf("chunk %d: FTS integrity: %v", chunk, err)
		}
	}
}

func TestFromClaudeCodeIgnoresNegativeAndNullUsage(t *testing.T) {
	ctx := context.Background()
	hub, err := store.Open(ctx, filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	if err := hub.RegisterSource(ctx, "src-1", "device-a", "claude-code", "/s.jsonl"); err != nil {
		t.Fatal(err)
	}
	lines := strings.Join([]string{
		`{"type":"assistant","sessionId":"sess-neg","message":{"id":"msg-neg","role":"assistant","content":"hi","usage":{"input_tokens":-1,"output_tokens":5}}}`,
		`{"type":"assistant","sessionId":"sess-neg","message":{"id":"msg-null","role":"assistant","content":"hi","usage":null}}`,
		`{"type":"assistant","sessionId":"sess-neg","message":{"id":"msg-ok","role":"assistant","content":"hi","usage":{"input_tokens":10,"output_tokens":3}}}`,
	}, "\n") + "\n"
	parsed, err := Parse("claude-code", []byte(lines), 0, Options{SourceScope: "src-1"})
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Events[0].HasUsage {
		t.Fatalf("negative usage accepted: %+v", parsed.Events[0].Usage)
	}
	if parsed.Events[1].HasUsage {
		t.Fatalf("null usage accepted: %+v", parsed.Events[1].Usage)
	}
	if !parsed.Events[2].HasUsage {
		t.Fatal("valid usage dropped")
	}
	if _, err := hub.Ingest(ctx, store.Batch{SourceID: "src-1", DeviceID: "device-a", Events: parsed.Events}); err != nil {
		t.Fatalf("ingest failed on negative/null usage: %v", err)
	}
	var count int
	if err := hub.Writer().QueryRow(`SELECT count(*) FROM session_token_usage`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("token usage rows = %d, %v; want 1", count, err)
	}
}

func TestParsersIgnoreNullUsageAvoidHTMLEscapingAndOverflow(t *testing.T) {
	codexLines := strings.Join([]string{
		`{"timestamp":"2026-04-01T10:00:00Z","type":"session_meta","payload":{"id":"codex-null-u"}}`,
		`{"timestamp":"2026-04-01T10:00:01Z","type":"token_usage_record","payload":{"response_id":"r1","usage":null}}`,
	}, "\n") + "\n"
	codexParsed, err := Parse("codex", []byte(codexLines), 0, Options{SourceScope: "src-c"})
	if err != nil {
		t.Fatal(err)
	}
	if codexParsed.Events[1].HasUsage {
		t.Fatalf("codex null usage accepted: %+v", codexParsed.Events[1].Usage)
	}

	piLines := strings.Join([]string{
		`{"type":"session","version":3,"id":"pi-null-u","timestamp":"2026-04-01T10:00:00Z","cwd":"/w"}`,
		`{"type":"message","id":"m1","timestamp":"2026-04-01T10:00:01Z","message":{"role":"assistant","responseId":"r1","content":[{"type":"toolCall","name":"run","arguments":{"cmd":"a < b && c > d"}}],"usage":null}}`,
		`{"type":"message","id":"m2","timestamp":"2026-04-01T10:00:02Z","message":{"role":"assistant","responseId":"r2","content":[{"type":"text","text":"overflow"}],"usage":{"input":9223372036854775807,"output":9223372036854775807,"cacheRead":2,"cacheWrite":0,"totalTokens":0}}}`,
	}, "\n") + "\n"
	piParsed, err := Parse("pi", []byte(piLines), 0, Options{SourceScope: "src-p"})
	if err != nil {
		t.Fatal(err)
	}
	if piParsed.Events[1].HasUsage {
		t.Fatalf("pi null usage accepted: %+v", piParsed.Events[1].Usage)
	}
	if !strings.Contains(piParsed.Events[1].Text, "a < b && c > d") {
		t.Fatalf("pi compactJSON escaped HTML: %q", piParsed.Events[1].Text)
	}
	if piParsed.Events[2].HasUsage {
		t.Fatalf("pi int64-overflow usage accepted: %+v", piParsed.Events[2].Usage)
	}

	ccLines := strings.Join([]string{
		`{"type":"user","sessionId":"cc-side","isSidechain":true,"timestamp":"2026-04-01T10:00:00Z","message":{"role":"user","content":"subagent prompt"}}`,
		`{"type":"assistant","sessionId":"cc-side","timestamp":"2026-04-01T10:00:01Z","message":{"role":"assistant","content":[{"type":"tool_use","name":"bash","input":{"cmd":"x < y && z > w"}}]}}`,
	}, "\n") + "\n"
	ccParsed, err := Parse("claude-code", []byte(ccLines), 0, Options{SourceScope: "src-cc"})
	if err != nil {
		t.Fatal(err)
	}
	if ccParsed.Events[0].Prompt != "" {
		t.Fatalf("sidechain prompt should not set Prompt, got %q", ccParsed.Events[0].Prompt)
	}
	if !strings.Contains(ccParsed.Events[1].Text, "x < y && z > w") {
		t.Fatalf("claudecode compactJSON escaped HTML: %q", ccParsed.Events[1].Text)
	}
}

func TestAntigravityParseIngestAndRederive(t *testing.T) {
	ctx := context.Background()
	hub, err := store.Open(ctx, filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()

	sourcePath := "/home/dev/.gemini/antigravity-cli/brain/11111111-2222-4333-8444-555555555555/.system_generated/logs/transcript_full.jsonl"
	if err := hub.RegisterSource(ctx, "src-agy", device, "antigravity", sourcePath); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "antigravity", "main-session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse("antigravity", data, 0, Options{SourceScope: "src-agy", SourcePath: sourcePath})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(parsed.Events) != 6 {
		t.Fatalf("events = %d, want 6", len(parsed.Events))
	}
	res, err := hub.Ingest(ctx, store.Batch{SourceID: "src-agy", DeviceID: device, Events: parsed.Events})
	if err != nil {
		t.Fatalf("Ingest: %v", err)
	}
	if res.Inserted != 6 {
		t.Fatalf("inserted = %d, want 6", res.Inserted)
	}

	sid := store.SessionID(device, "antigravity", "11111111-2222-4333-8444-555555555555")
	var prompt, cwd, model string
	if err := hub.Writer().QueryRowContext(ctx,
		`SELECT first_prompt, cwd, model FROM sessions WHERE id = ?`, sid).Scan(&prompt, &cwd, &model); err != nil {
		t.Fatalf("session row: %v", err)
	}
	if prompt != "fix the auth bug" || cwd != "/home/dev/project" || model != "gemini-4-neon-high" {
		t.Fatalf("session = (prompt=%q cwd=%q model=%q)", prompt, cwd, model)
	}

	var inTok, cacheRead, outTok int64
	if err := hub.Writer().QueryRowContext(ctx,
		`SELECT sum(input_tokens), sum(cache_read_input_tokens), sum(output_tokens) FROM session_token_usage WHERE session_id = ?`, sid).
		Scan(&inTok, &cacheRead, &outTok); err != nil {
		t.Fatalf("usage sum: %v", err)
	}
	if inTok != 2300 || cacheRead != 12000 || outTok != 460 {
		t.Fatalf("usage sum = (%d, %d, %d), want (2300, 12000, 460)", inTok, cacheRead, outTok)
	}

	bumped := []store.ParserDeriver{{
		Agent:         "antigravity",
		OutputVersion: 2,
		Derive: func(in store.DeriveInput) (store.Event, error) {
			ev, err := deriveAntigravity(in)
			ev.ParserVersion = 2
			return ev, err
		},
	}}
	reRes, err := hub.ReDerive(ctx, bumped)
	if err != nil {
		t.Fatalf("ReDerive: %v", err)
	}
	if reRes.Events != 6 || reRes.Sessions != 1 {
		t.Fatalf("ReDerive = %+v, want 6 events / 1 session", reRes)
	}
}
