package codex

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const (
	mainThread     = "00000000-0000-7000-8000-0000000000c1"
	subagentThread = "00000000-0000-7000-8000-0000000000d1"
)

// fixture reads a synthetic Codex fixture; byte offsets in the assertions
// below come from testdata/codex/README.md.
func fixture(t testing.TB, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "codex", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// ms builds the Unix millisecond value of a fixture timestamp.
func ms(hour, min, sec, millis int) int64 {
	return time.Date(2026, time.November, 13, hour, min, sec, millis*int(time.Millisecond), time.UTC).UnixMilli()
}

// metadataOf decodes a record's metadata for spot checks.
func metadataOf(t *testing.T, r Record) map[string]any {
	t.Helper()
	var md map[string]any
	if err := json.Unmarshal([]byte(r.Metadata), &md); err != nil {
		t.Fatalf("metadata is not a JSON object: %v", err)
	}
	return md
}

// checkInvariants asserts the structural properties every parse result must
// hold, whatever the input: contiguous boundaries, raw text preserved
// verbatim, valid metadata, one native ID per source, and a cursor that
// never moves backwards.
func checkInvariants(t *testing.T, data []byte, base int64, res Result) {
	t.Helper()
	if res.Cursor < base || res.Cursor > base+int64(len(data)) {
		t.Fatalf("cursor %d outside [%d, %d]", res.Cursor, base, base+int64(len(data)))
	}
	for i, r := range res.Records {
		if i == 0 && r.Offset != base {
			t.Fatalf("first record starts at %d, want %d", r.Offset, base)
		}
		if i > 0 && r.Offset != res.Records[i-1].End {
			t.Fatalf("record %d starts at %d, want %d", i, r.Offset, res.Records[i-1].End)
		}
		if r.End != r.Offset+int64(len(r.Raw))+1 {
			t.Fatalf("record %d end %d does not follow its raw text", i, r.End)
		}
		rel := int(r.Offset - base)
		if r.Raw != string(data[rel:rel+len(r.Raw)]) {
			t.Fatalf("record %d raw text is not the verbatim source slice", i)
		}
		if r.Kind == "" {
			t.Fatalf("record %d has no kind", i)
		}
		if r.Native == "" || r.Native != res.Native {
			t.Fatalf("record %d native %q, want the source's %q", i, r.Native, res.Native)
		}
		var md map[string]any
		if err := json.Unmarshal([]byte(r.Metadata), &md); err != nil {
			t.Fatalf("record %d metadata: %v", i, err)
		}
		if len(r.Usage) > 0 && !json.Valid(r.Usage) {
			t.Fatalf("record %d usage is not valid JSON", i)
		}
	}
	if len(res.Records) > 0 && res.Records[len(res.Records)-1].End != res.Cursor {
		t.Fatal("cursor does not end at the last record's boundary")
	}
}

// want is one expected record of a fixture table.
type want struct {
	off, end   int64
	kind, role string
	tool, text string
	ordinal    int64 // -1 when the record has no ordinal
	timeMS     int64
}

// checkTable compares parsed records with a fixture table.
func checkTable(t *testing.T, res Result, tests []want) {
	t.Helper()
	if len(res.Records) != len(tests) {
		t.Fatalf("got %d records, want %d", len(res.Records), len(tests))
	}
	for i, w := range tests {
		got := res.Records[i]
		if got.Offset != w.off || got.End != w.end {
			t.Errorf("record %d bytes [%d, %d), want [%d, %d)", i, got.Offset, got.End, w.off, w.end)
		}
		if got.Kind != w.kind || got.Role != w.role || got.ToolName != w.tool {
			t.Errorf("record %d kind/role/tool = %q/%q/%q, want %q/%q/%q",
				i, got.Kind, got.Role, got.ToolName, w.kind, w.role, w.tool)
		}
		if got.Text != w.text {
			t.Errorf("record %d text = %q, want %q", i, got.Text, w.text)
		}
		if w.ordinal < 0 && got.HasOrdinal {
			t.Errorf("record %d has ordinal %d, want none", i, got.Ordinal)
		}
		if w.ordinal >= 0 && (!got.HasOrdinal || got.Ordinal != w.ordinal) {
			t.Errorf("record %d ordinal = (%t, %d), want %d", i, got.HasOrdinal, got.Ordinal, w.ordinal)
		}
		if !got.HasTime || got.TimeMS != w.timeMS {
			t.Errorf("record %d time = (%t, %d), want %d", i, got.HasTime, got.TimeMS, w.timeMS)
		}
	}
}

func TestParseMainSession(t *testing.T) {
	data := fixture(t, "main-session.jsonl")
	res, err := Parse(data, 0, Options{SourceScope: "src-main"})
	if err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, data, 0, res)
	checkTable(t, res, []want{
		{0, 422, "session_meta", "", "", "", -1, ms(9, 0, 0, 0)},
		{422, 718, "response_item", "developer", "", "<permissions instructions>\nSandbox is read-only. Approval policy is never.\n</permissions instructions>", 1, ms(9, 0, 0, 100)},
		{718, 939, "response_item", "user", "", "Add a Codex section to the README.", 2, ms(9, 0, 1, 0)},
		{939, 1141, "event_msg", "", "", "", 3, ms(9, 0, 1, 100)},
		{1141, 1478, "turn_context", "", "", "", 4, ms(9, 0, 1, 200)},
		{1478, 1678, "response_item", "", "", "", 5, ms(9, 0, 2, 0)},
		{1678, 1947, "response_item", "", "shell", `shell {"command":["sed","-n","1,20p","README.md"]}`, 6, ms(9, 0, 2, 500)},
		{1947, 2190, "response_item", "", "", "# Fixtures\n\nSynthetic session files live under testdata/.", 7, ms(9, 0, 2, 900)},
		{2190, 2542, "event_msg", "", "", "", 8, ms(9, 0, 3, 0)},
		{2542, 2890, "response_item", "assistant", "", "The README has no Codex section yet. I will add one after the Claude Code section.", 9, ms(9, 0, 4, 0)},
		{2890, 3137, "event_msg", "", "", "", 10, ms(9, 0, 4, 100)},
		{3137, 3848, "token_usage_record", "", "", "", 11, ms(9, 0, 5, 0)},
		{3848, 4561, "token_usage_record", "", "", "", 12, ms(9, 0, 6, 0)},
		{4561, 4780, "event_msg", "", "", "", 13, ms(9, 0, 7, 0)},
	})
	if res.Cursor != 4780 || res.Native != mainThread {
		t.Fatalf("cursor/native = %d/%q, want 4780/%q", res.Cursor, res.Native, mainThread)
	}
	for i, r := range res.Records {
		if r.Native != mainThread || r.Synthetic {
			t.Errorf("record %d native = %q (synthetic %t)", i, r.Native, r.Synthetic)
		}
	}

	meta := res.Records[0]
	if meta.Cwd != "/home/dev/example" || meta.GitBranch != "main" {
		t.Errorf("meta cwd/branch = %q/%q", meta.Cwd, meta.GitBranch)
	}
	md := metadataOf(t, meta)
	if md["threadId"] != mainThread || md["originator"] != "codex-tui" || md["source"] != "cli" || md["embedded"] != nil {
		t.Errorf("meta metadata = %v", md)
	}
	if p := res.Records[3].Prompt; p != "Add a Codex section to the README." {
		t.Errorf("user_message prompt = %q", p)
	}
	for i, r := range res.Records {
		if i != 3 && r.Prompt != "" {
			t.Errorf("record %d carries prompt %q; only the user_message event does", i, r.Prompt)
		}
	}
	if tc := res.Records[4]; tc.Model != "gpt-5-codex" || tc.Cwd != "/home/dev/example" {
		t.Errorf("turn_context model/cwd = %q/%q", tc.Model, tc.Cwd)
	}
	if md := metadataOf(t, res.Records[6]); md["payloadType"] != "function_call" || md["callId"] != "call_fixture_0001" {
		t.Errorf("function_call metadata = %v", md)
	}
	if md := metadataOf(t, res.Records[9]); md["phase"] != "commentary" {
		t.Errorf("assistant metadata = %v", md)
	}
	u1, u2 := res.Records[11], res.Records[12]
	if u1.ResponseID != "resp_fixture_0001" || u2.ResponseID != "resp_fixture_0002" {
		t.Errorf("response ids = %q/%q", u1.ResponseID, u2.ResponseID)
	}
	// usage is per response; the cumulative turn and thread totals stay out.
	if got := string(u2.Usage); got != `{"cache_write_input_tokens":0,"cached_input_tokens":1250,"input_tokens":1300,"output_tokens":30,"reasoning_output_tokens":0,"total_tokens":1330}` {
		t.Errorf("second usage = %s", got)
	}
}

func TestParseSubagentSession(t *testing.T) {
	data := fixture(t, "subagent-session.jsonl")
	res, err := Parse(data, 0, Options{SourceScope: "src-sub"})
	if err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, data, 0, res)
	checkTable(t, res, []want{
		{0, 565, "session_meta", "", "", "", -1, ms(9, 1, 0, 0)},
		{565, 987, "session_meta", "", "", "", -1, ms(9, 1, 0, 0)},
		{987, 1234, "response_item", "user", "", "Find every fixture README that omits the record byte ranges.", 2, ms(9, 1, 1, 0)},
		{1234, 1555, "response_item", "assistant", "", "One file omits the byte ranges: codex/incomplete-tail.jsonl.", 3, ms(9, 1, 2, 0)},
		{1555, 2362, "token_usage_record", "", "", "", 4, ms(9, 1, 3, 0)},
	})
	// The subagent is its own thread; the embedded parent meta joins it
	// without lending the parent's observations.
	if res.Native != subagentThread {
		t.Fatalf("native = %q, want %q", res.Native, subagentThread)
	}
	own, parent := res.Records[0], res.Records[1]
	if md := metadataOf(t, own); md["source"] != "subagent" || md["parent_thread_id"] != mainThread {
		t.Errorf("own meta metadata = %v", md)
	}
	if own.Cwd != "/home/dev/example" || own.GitBranch != "" {
		t.Errorf("own meta cwd/branch = %q/%q", own.Cwd, own.GitBranch)
	}
	if md := metadataOf(t, parent); md["embedded"] != true || md["threadId"] != mainThread {
		t.Errorf("embedded meta metadata = %v", md)
	}
	if parent.Cwd != "" || parent.GitBranch != "" {
		t.Errorf("embedded meta lends cwd/branch %q/%q", parent.Cwd, parent.GitBranch)
	}
	if r := res.Records[4]; r.ResponseID != "resp_fixture_0101" || len(r.Usage) == 0 {
		t.Errorf("usage record = %q/%s", r.ResponseID, r.Usage)
	}
}

func TestParseResumedPage(t *testing.T) {
	data := fixture(t, "resumed-page.jsonl")
	res, err := Parse(data, 0, Options{SourceScope: "src-page"})
	if err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, data, 0, res)
	checkTable(t, res, []want{
		// The opening meta repeats ordinal 12 in its envelope; it never
		// takes part in the (thread, ordinal) identity.
		{0, 568, "session_meta", "", "", "", -1, ms(10, 30, 0, 0)},
		{568, 1281, "token_usage_record", "", "", "", 12, ms(9, 0, 6, 0)},
		{1281, 1500, "event_msg", "", "", "", 13, ms(9, 0, 7, 0)},
		{1500, 1726, "response_item", "user", "", "Also mention the resumed-page fixture.", 14, ms(10, 30, 1, 0)},
		{1726, 2048, "response_item", "assistant", "", "Mentioned the resumed-page fixture in the same section.", 15, ms(10, 30, 2, 0)},
	})
	// The page continues the main thread: same native ID.
	if res.Native != mainThread {
		t.Fatalf("native = %q, want %q", res.Native, mainThread)
	}
	if md := metadataOf(t, res.Records[0]); md["history_mode"] != "paginated" || md["embedded"] != nil {
		t.Errorf("page meta metadata = %v", md)
	}

	// The replayed records are byte-identical to the originals.
	main, err := Parse(fixture(t, "main-session.jsonl"), 0, Options{SourceScope: "src-main"})
	if err != nil {
		t.Fatal(err)
	}
	for i, j := range map[int]int{1: 12, 2: 13} {
		if res.Records[i].Raw != main.Records[j].Raw || res.Records[i].Ordinal != main.Records[j].Ordinal {
			t.Errorf("page record %d does not replay main record %d", i, j)
		}
	}
}

func TestParseIncompleteTail(t *testing.T) {
	data := fixture(t, "incomplete-tail.jsonl")
	res, err := Parse(data, 0, Options{SourceScope: "src-tail"})
	if err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, data, 0, res)
	if len(res.Records) != 3 || res.Cursor != 797 {
		t.Fatalf("records/cursor = %d/%d, want 3/797", len(res.Records), res.Cursor)
	}
	// Completing the tail on a later poll accepts exactly the rest.
	completed := append(append([]byte{}, data[797:]...), []byte(`ory/.","phase":"final"}}`+"\n")...)
	res2, err := Parse(completed, res.Cursor, Options{SourceScope: "src-tail", Native: res.Native})
	if err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, completed, 797, res2)
	if len(res2.Records) != 1 || res2.Records[0].Kind != "event_msg" || res2.Records[0].Native != res.Native {
		t.Fatalf("completed tail = %+v", res2.Records)
	}
}

func TestParseMalformedLineStopsSource(t *testing.T) {
	data := fixture(t, "malformed-line.jsonl")
	res, err := Parse(data, 0, Options{SourceScope: "src-bad"})
	var perr *ParseError
	if !errors.As(err, &perr) {
		t.Fatalf("error = %v, want a ParseError", err)
	}
	if perr.Offset != 502 || res.Cursor != 502 || len(res.Records) != 2 {
		t.Fatalf("stop %d, cursor %d, records %d; want 502/502/2", perr.Offset, res.Cursor, len(res.Records))
	}
	// The diagnostic never quotes transcript content.
	if strings.Contains(perr.Error(), "quote") || strings.Contains(perr.Error(), "unescaped") {
		t.Fatalf("diagnostic leaks content: %v", perr)
	}
	// A retry from the stop offset stops at the same place again: bytes are
	// never skipped.
	_, err = Parse(data[502:], 502, Options{SourceScope: "src-bad", Native: res.Native})
	if !errors.As(err, &perr) || perr.Offset != 502 {
		t.Fatalf("retry error = %v, want a stop at 502", err)
	}
}

func TestParseNativeIdentity(t *testing.T) {
	meta := `{"timestamp":"2026-11-13T09:00:00.000Z","type":"session_meta","payload":{"id":"thread-1"}}` + "\n"
	msg := `{"timestamp":"2026-11-13T09:00:01.000Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}}` + "\n"
	tests := []struct {
		name      string
		data      string
		base      int64
		opts      Options
		native    string
		synthetic bool
		err       error
	}{
		{"meta names the thread", meta + msg, 0, Options{SourceScope: "s"}, "thread-1", false, nil},
		{"base 0 ignores a passed native", meta + msg, 0, Options{SourceScope: "s", Native: "other"}, "thread-1", false, nil},
		{"later bytes use the passed native", msg, 500, Options{SourceScope: "s", Native: "thread-1"}, "thread-1", false, nil},
		{"first record without meta is synthetic", msg + meta, 0, Options{SourceScope: "s"}, SyntheticNativeIDPrefix + "s", true, nil},
		{"meta with empty id is synthetic", `{"type":"session_meta","payload":{"id":""}}` + "\n" + msg, 0, Options{SourceScope: "s"}, SyntheticNativeIDPrefix + "s", true, nil},
		{"meta with numeric id is synthetic", `{"type":"session_meta","payload":{"id":7}}` + "\n", 0, Options{SourceScope: "s"}, SyntheticNativeIDPrefix + "s", true, nil},
		{"passed synthetic native stays synthetic", msg, 500, Options{SourceScope: "s", Native: SyntheticNativeIDPrefix + "s"}, SyntheticNativeIDPrefix + "s", true, nil},
		{"later bytes without native fail", msg, 500, Options{SourceScope: "s"}, "", false, ErrNoNative},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Parse([]byte(tc.data), tc.base, tc.opts)
			if !errors.Is(err, tc.err) {
				t.Fatalf("error = %v, want %v", err, tc.err)
			}
			if err != nil {
				return
			}
			checkInvariants(t, []byte(tc.data), tc.base, res)
			for i, r := range res.Records {
				if r.Native != tc.native || r.Synthetic != tc.synthetic {
					t.Errorf("record %d native = %q (synthetic %t), want %q (%t)", i, r.Native, r.Synthetic, tc.native, tc.synthetic)
				}
			}
		})
	}

	// Without a scope, a source whose first record names no thread stops
	// instead of inventing an identity.
	_, err := Parse([]byte(msg), 0, Options{})
	var perr *ParseError
	if !errors.As(err, &perr) || perr.Offset != 0 {
		t.Fatalf("error = %v, want a stop at 0", err)
	}
}

func TestParseRecordNormalization(t *testing.T) {
	line := func(typ, payload string) string {
		return `{"timestamp":"2026-11-13T09:00:00.000Z","ordinal":5,"type":"` + typ + `","payload":` + payload + "}"
	}
	tests := []struct {
		name       string
		line       string
		kind, role string
		tool, text string
		prompt     string
		md         map[string]any
	}{
		{"custom tool call", line("response_item", `{"type":"custom_tool_call","name":"apply_patch","input":"*** Begin Patch"}`),
			"response_item", "", "apply_patch", "apply_patch *** Begin Patch", "", map[string]any{"payloadType": "custom_tool_call"}},
		{"custom tool output", line("response_item", `{"type":"custom_tool_call_output","call_id":"c1","output":"Done"}`),
			"response_item", "", "", "Done", "", map[string]any{"payloadType": "custom_tool_call_output", "callId": "c1"}},
		{"output as content array", line("response_item", `{"type":"function_call_output","output":[{"type":"input_text","text":"a"},{"type":"input_image"},{"type":"input_text","text":"b"}]}`),
			"response_item", "", "", "a\nb", "", map[string]any{"payloadType": "function_call_output"}},
		{"reasoning summary", line("response_item", `{"type":"reasoning","summary":[{"type":"summary_text","text":"plan"}],"content":[{"type":"reasoning_text","text":"think"}],"encrypted_content":"x"}`),
			"response_item", "", "", "plan\nthink", "", map[string]any{"payloadType": "reasoning"}},
		{"web search", line("response_item", `{"type":"web_search_call","action":{"type":"search","query":"sqlite fts5"}}`),
			"response_item", "", "", "sqlite fts5", "", map[string]any{"payloadType": "web_search_call"}},
		{"function call without name", line("response_item", `{"type":"function_call","arguments":"{}"}`),
			"response_item", "", "", "{}", "", map[string]any{"payloadType": "function_call"}},
		{"unknown payload type", line("response_item", `{"type":"brand_new","text":"x"}`),
			"response_item", "", "", "", "", map[string]any{"payloadType": "brand_new"}},
		{"agent message mirror has no text", line("event_msg", `{"type":"agent_message","message":"mirrored"}`),
			"event_msg", "", "", "", "", map[string]any{"payloadType": "agent_message"}},
		{"user message marks the prompt", line("event_msg", `{"type":"user_message","message":"typed prompt"}`),
			"event_msg", "", "", "", "typed prompt", map[string]any{"payloadType": "user_message"}},
		{"unknown top-level type", line("brand_new_kind", `{"a":1}`),
			"unknown", "", "", "", "", map[string]any{"observedType": "brand_new_kind"}},
		{"missing payload", `{"type":"response_item"}`,
			"response_item", "", "", "", "", map[string]any{}},
		{"non-object payload", `{"type":"turn_context","payload":[1]}`,
			"turn_context", "", "", "", "", map[string]any{}},
		{"world state", line("world_state", `{"instructions":"long"}`),
			"world_state", "", "", "", "", map[string]any{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data := []byte(tc.line + "\n")
			res, err := Parse(data, 100, Options{Native: "thread-1"})
			if err != nil {
				t.Fatal(err)
			}
			checkInvariants(t, data, 100, res)
			r := res.Records[0]
			if r.Kind != tc.kind || r.Role != tc.role || r.ToolName != tc.tool || r.Text != tc.text || r.Prompt != tc.prompt {
				t.Errorf("kind/role/tool/text/prompt = %q/%q/%q/%q/%q, want %q/%q/%q/%q/%q",
					r.Kind, r.Role, r.ToolName, r.Text, r.Prompt, tc.kind, tc.role, tc.tool, tc.text, tc.prompt)
			}
			if got := metadataOf(t, r); !reflect.DeepEqual(got, tc.md) {
				t.Errorf("metadata = %v, want %v", got, tc.md)
			}
		})
	}
}

func TestParseOrdinalAndTimestampEdges(t *testing.T) {
	tests := []struct {
		line       string
		hasOrdinal bool
		ordinal    int64
		hasTime    bool
	}{
		{`{"type":"event_msg","ordinal":0}`, true, 0, false},
		{`{"type":"event_msg","ordinal":-1}`, false, 0, false},
		{`{"type":"event_msg","ordinal":1.5}`, false, 0, false},
		{`{"type":"event_msg","ordinal":"3"}`, false, 0, false},
		{`{"type":"event_msg","ordinal":99999999999999999999}`, false, 0, false},
		{`{"type":"event_msg"}`, false, 0, false},
		{`{"type":"session_meta","ordinal":4,"payload":{"id":"x"}}`, false, 0, false},
		{`{"type":"event_msg","timestamp":"not a time"}`, false, 0, false},
		{`{"type":"event_msg","timestamp":"2026-11-13T09:00:00+01:00"}`, false, 0, true},
	}
	for _, tc := range tests {
		data := []byte(tc.line + "\n")
		res, err := Parse(data, 10, Options{Native: "thread-1"})
		if err != nil {
			t.Fatalf("%s: %v", tc.line, err)
		}
		r := res.Records[0]
		if r.HasOrdinal != tc.hasOrdinal || r.Ordinal != tc.ordinal || r.HasTime != tc.hasTime {
			t.Errorf("%s: ordinal (%t, %d), time %t; want (%t, %d), %t",
				tc.line, r.HasOrdinal, r.Ordinal, r.HasTime, tc.hasOrdinal, tc.ordinal, tc.hasTime)
		}
	}
}

func TestParseMalformedRecords(t *testing.T) {
	tests := []struct {
		name   string
		data   string
		reason string
	}{
		{"invalid JSON", "{bad\n", "not valid JSON"},
		{"array", "[1]\n", "not a JSON object"},
		{"null", "null\n", "not a JSON object"},
		{"empty line", "\n", "not valid JSON"},
		{"trailing content", `{"a":1}{"b":2}` + "\n", "content after"},
		{"invalid UTF-8", "{\"a\":\"\xff\"}\n", "UTF-8"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Parse([]byte(tc.data), 40, Options{Native: "thread-1"})
			var perr *ParseError
			if !errors.As(err, &perr) || perr.Offset != 40 || res.Cursor != 40 {
				t.Fatalf("error = %v, cursor %d; want a stop at 40", err, res.Cursor)
			}
			if !strings.Contains(perr.Reason, tc.reason) {
				t.Fatalf("reason %q, want %q", perr.Reason, tc.reason)
			}
		})
	}
}

func TestParseRecordSizeLimit(t *testing.T) {
	rec := `{"type":"event_msg","payload":{"type":"x"}}`
	limit := int64(len(rec))
	// A record exactly at the limit passes.
	if _, err := Parse([]byte(rec+"\n"), 7, Options{Native: "t", MaxRecordBytes: limit}); err != nil {
		t.Fatalf("record at the limit: %v", err)
	}
	var perr *ParseError
	// One byte more stops the source at the record.
	_, err := Parse([]byte(rec+"\n"), 7, Options{Native: "t", MaxRecordBytes: limit - 1})
	if !errors.As(err, &perr) || perr.Offset != 7 {
		t.Fatalf("oversized record error = %v", err)
	}
	// An incomplete tail already above the limit stops the source too.
	res, err := Parse([]byte(rec+"\n"+rec), 7, Options{Native: "t", MaxRecordBytes: limit - 1})
	if !errors.As(err, &perr) || perr.Offset != 7 || len(res.Records) != 0 {
		t.Fatalf("tail error = %v", err)
	}
	res, err = Parse([]byte(rec+"\n"+rec), 7, Options{Native: "t", MaxRecordBytes: limit})
	if err != nil || len(res.Records) != 1 || res.Cursor != 7+limit+1 {
		t.Fatalf("tail at the limit: %v, %d records, cursor %d", err, len(res.Records), res.Cursor)
	}
}

func TestParseEmptyInput(t *testing.T) {
	res, err := Parse(nil, 0, Options{SourceScope: "s"})
	if err != nil || len(res.Records) != 0 || res.Cursor != 0 || res.Native != "" {
		t.Fatalf("empty parse = %+v, %v", res, err)
	}
}

func TestParseSplitAtEveryOffset(t *testing.T) {
	// Polling can cut the file anywhere. Parsing the first part, then the
	// rest from the cursor with the established native, yields exactly the
	// records of one full parse.
	data := fixture(t, "main-session.jsonl")
	full, err := Parse(data, 0, Options{SourceScope: "s"})
	if err != nil {
		t.Fatal(err)
	}
	for cut := 0; cut <= len(data); cut++ {
		first, err := Parse(data[:cut], 0, Options{SourceScope: "s"})
		if err != nil {
			t.Fatalf("cut %d: %v", cut, err)
		}
		records := first.Records
		if first.Cursor > 0 {
			rest, err := Parse(data[first.Cursor:], first.Cursor, Options{SourceScope: "s", Native: first.Native})
			if err != nil {
				t.Fatalf("cut %d rest: %v", cut, err)
			}
			records = append(records, rest.Records...)
		} else {
			rest, err := Parse(data, 0, Options{SourceScope: "s"})
			if err != nil {
				t.Fatalf("cut %d rest: %v", cut, err)
			}
			records = rest.Records
		}
		if !reflect.DeepEqual(records, full.Records) {
			t.Fatalf("cut %d changed the records", cut)
		}
	}
}

func TestParseIsDeterministic(t *testing.T) {
	for _, name := range []string{"main-session.jsonl", "subagent-session.jsonl", "resumed-page.jsonl"} {
		data := fixture(t, name)
		a, errA := Parse(data, 0, Options{SourceScope: "s"})
		b, errB := Parse(data, 0, Options{SourceScope: "s"})
		if !reflect.DeepEqual(a, b) || !reflect.DeepEqual(errA, errB) {
			t.Fatalf("%s: parse is not deterministic", name)
		}
	}
}
