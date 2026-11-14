package pi

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Session ids of the Pi fixtures (testdata/pi/README.md).
const (
	mainSession   = "00000000-0000-7000-8000-0000000000b1"
	errorSession  = "00000000-0000-7000-8000-0000000000b2"
	tailSession   = "00000000-0000-7000-8000-0000000000b3"
	badSession    = "00000000-0000-7000-8000-0000000000b4"
	toolCallFixID = "call_fixture_0001|fc_000000000000000000000000000000000000000000000001"
)

// fixture reads a synthetic Pi fixture; byte offsets in the assertions below
// come from testdata/pi/README.md.
func fixture(t testing.TB, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "pi", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// ms builds the Unix millisecond value of a fixture timestamp.
func ms(min, sec, millis int) int64 {
	return time.Date(2026, time.November, 13, 9, min, sec, millis*int(time.Millisecond), time.UTC).UnixMilli()
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
		{0, 143, "session", "", "", "", ms(10, 0, 0)},
		{143, 283, "model_change", "", "", "", ms(10, 0, 100)},
		{283, 415, "thinking_level_change", "", "", "", ms(10, 0, 100)},
		{415, 869, "message", "system", "", "", ms(10, 0, 200)},
		{869, 1194, "message", "user", "", "Use your read or list tool once to see the file names in the current working directory, then reply with one short sentence that names them.", ms(10, 0, 300)},
		{1194, 1893, "message", "assistant", "bash", `bash {"command":"ls -1","timeout":10}`, ms(10, 2, 0)},
		{1893, 2239, "message", "toolResult", "bash", "main.py\nnotes.txt\n", ms(10, 2, 100)},
		{2239, 2953, "message", "assistant", "", "The directory contains `main.py` and `notes.txt`.", ms(10, 4, 0)},
	})
	if res.Cursor != 2953 || res.Native != mainSession {
		t.Fatalf("cursor/native = %d/%q, want 2953/%q", res.Cursor, res.Native, mainSession)
	}
	for i, r := range res.Records {
		if r.Native != mainSession || r.Synthetic {
			t.Errorf("record %d native = %q (synthetic %t)", i, r.Native, r.Synthetic)
		}
	}

	header := res.Records[0]
	if header.Cwd != "/home/dev/pi-project" {
		t.Errorf("header cwd = %q", header.Cwd)
	}
	if md := metadataOf(t, header); md["sessionVersion"] != float64(3) {
		t.Errorf("header metadata = %v", md)
	}
	// The session header names the session, not an entry; every other entry
	// carries its own 8-hex id.
	for i, r := range res.Records {
		want := ""
		if i > 0 {
			want = fmt.Sprintf("aa0000%02d", i)
		}
		if r.EntryID != want {
			t.Errorf("record %d entry id = %q, want %q", i, r.EntryID, want)
		}
	}
	if mc := res.Records[1]; mc.Model != "gpt-5.4-nano" || metadataOf(t, mc)["provider"] != "openai" {
		t.Errorf("model_change = %q/%v", mc.Model, metadataOf(t, mc))
	}
	if md := metadataOf(t, res.Records[2]); md["thinkingLevel"] != "off" {
		t.Errorf("thinking_level_change metadata = %v", md)
	}
	// The system entry is the harness preamble: its shape is observable, its
	// text is not conversation.
	sys := res.Records[3]
	if md := metadataOf(t, sys); !reflect.DeepEqual(md["sections"], []any{"preamble", "tools"}) ||
		!reflect.DeepEqual(md["toolsAdded"], []any{"read", "bash"}) {
		t.Errorf("system metadata = %v", md)
	}

	user := res.Records[4]
	if user.Prompt != user.Text || user.Prompt == "" {
		t.Errorf("user prompt = %q", user.Prompt)
	}
	for i, r := range res.Records {
		if i != 4 && r.Prompt != "" {
			t.Errorf("record %d carries prompt %q; only the user message does", i, r.Prompt)
		}
	}

	toolCall := res.Records[5]
	if toolCall.MessageID != "resp_fixture_0001" || toolCall.Model != "gpt-5.4-nano" || len(toolCall.Usage) == 0 {
		t.Errorf("tool call = %q/%q/%s", toolCall.MessageID, toolCall.Model, toolCall.Usage)
	}
	md := metadataOf(t, toolCall)
	if md["stopReason"] != "toolUse" || md["durationMs"] != float64(1382) || md["api"] != "openai-responses" {
		t.Errorf("tool call metadata = %v", md)
	}
	if md := metadataOf(t, res.Records[6]); md["toolCallId"] != toolCallFixID || md["isError"] != nil {
		t.Errorf("toolResult metadata = %v", md)
	}
	final := res.Records[7]
	if final.MessageID != "resp_fixture_0002" || metadataOf(t, final)["stopReason"] != "stop" {
		t.Errorf("final assistant = %q/%v", final.MessageID, metadataOf(t, final))
	}
}

func TestParseErrorSession(t *testing.T) {
	data := fixture(t, "error-session.jsonl")
	res, err := Parse(data, 0, Options{SourceScope: "src-err"})
	if err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, data, 0, res)
	checkTable(t, res, []want{
		{0, 143, "session", "", "", "", ms(20, 0, 0)},
		{143, 275, "model_change", "", "", "", ms(20, 0, 100)},
		{275, 410, "thinking_level_change", "", "", "", ms(20, 0, 100)},
		{410, 713, "message", "user", "", "List the file names in the current working directory with a tool, then reply with one short sentence that names them.", ms(20, 0, 200)},
		{713, 1205, "message", "assistant", "", "", ms(20, 1, 0)},
	})
	if res.Native != errorSession {
		t.Fatalf("native = %q, want %q", res.Native, errorSession)
	}
	failed := res.Records[4]
	// A failed model call persists no responseId; its usage object is all
	// zero, and the diagnostic stays out of metadata.
	if failed.MessageID != "" || len(failed.Usage) == 0 {
		t.Errorf("failed assistant = %q/%s", failed.MessageID, failed.Usage)
	}
	if md := metadataOf(t, failed); md["stopReason"] != "error" || md["errorMessage"] != nil {
		t.Errorf("failed assistant metadata = %v", md)
	}
	if mc := res.Records[1]; mc.Model != "glm-5.3" || metadataOf(t, mc)["provider"] != "zai" {
		t.Errorf("model_change = %q/%v", mc.Model, metadataOf(t, mc))
	}
}

// TestParseAnthropicSession parses the anthropic-messages fixture: a
// successful response without a responseId whose usage carries a non-zero
// cacheWrite, then a final response with a responseId and cache reads plus
// cache writes (testdata/pi/README.md, "Usage fields").
func TestParseAnthropicSession(t *testing.T) {
	data := fixture(t, "anthropic-session.jsonl")
	res, err := Parse(data, 0, Options{SourceScope: "src-anthropic"})
	if err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, data, 0, res)
	if len(res.Records) != 7 || res.Cursor != 2341 {
		t.Fatalf("records/cursor = %d/%d, want 7/2341", len(res.Records), res.Cursor)
	}
	first := res.Records[4]
	if first.EntryID != "0a1b2c04" || first.MessageID != "" || len(first.Usage) == 0 {
		t.Errorf("first assistant = %q/%q/%s, want entry id and no responseId", first.EntryID, first.MessageID, first.Usage)
	}
	if md := metadataOf(t, first); md["api"] != "anthropic-messages" || md["stopReason"] != "toolUse" {
		t.Errorf("first assistant metadata = %v", md)
	}
	final := res.Records[6]
	if final.EntryID != "0a1b2c06" || final.MessageID != "msg_fixture_0002" {
		t.Errorf("final assistant = %q/%q, want entry id and responseId", final.EntryID, final.MessageID)
	}
}

func TestParseIncompleteTail(t *testing.T) {
	data := fixture(t, "incomplete-tail.jsonl")
	res, err := Parse(data, 0, Options{SourceScope: "src-tail"})
	if err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, data, 0, res)
	if len(res.Records) != 3 || res.Cursor != 502 || res.Native != tailSession {
		t.Fatalf("records/cursor/native = %d/%d/%q, want 3/502/%q", len(res.Records), res.Cursor, res.Native, tailSession)
	}
	// Completing the tail on a later poll accepts exactly the rest.
	completed := append(append([]byte{}, data[502:]...), []byte("ains `main.py` and `notes.txt`.\"}]}}\n")...)
	res2, err := Parse(completed, res.Cursor, Options{SourceScope: "src-tail", Native: res.Native})
	if err != nil {
		t.Fatal(err)
	}
	checkInvariants(t, completed, 502, res2)
	if len(res2.Records) != 1 || res2.Records[0].Kind != "message" || res2.Records[0].Role != "assistant" ||
		res2.Records[0].Native != res.Native || res2.Records[0].Text != "The directory contains `main.py` and `notes.txt`." {
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
	if perr.Offset != 283 || res.Cursor != 283 || len(res.Records) != 2 || res.Native != badSession {
		t.Fatalf("stop %d, cursor %d, records %d, native %q; want 283/283/2/%q", perr.Offset, res.Cursor, len(res.Records), res.Native, badSession)
	}
	// The diagnostic never quotes transcript content.
	if strings.Contains(perr.Error(), "break") || strings.Contains(perr.Error(), "unescaped") {
		t.Fatalf("diagnostic leaks content: %v", perr)
	}
	// A retry from the stop offset stops at the same place again: bytes are
	// never skipped.
	_, err = Parse(data[283:], 283, Options{SourceScope: "src-bad", Native: res.Native})
	if !errors.As(err, &perr) || perr.Offset != 283 {
		t.Fatalf("retry error = %v, want a stop at 283", err)
	}
}

func TestParseNativeIdentity(t *testing.T) {
	header := `{"type":"session","version":3,"id":"sess-1","timestamp":"2026-11-13T09:00:00.000Z","cwd":"/w"}` + "\n"
	msg := `{"type":"message","id":"ab000001","parentId":null,"timestamp":"2026-11-13T09:00:01.000Z","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}` + "\n"
	tests := []struct {
		name      string
		data      string
		base      int64
		opts      Options
		native    string
		synthetic bool
		err       error
	}{
		{"header names the session", header + msg, 0, Options{SourceScope: "s"}, "sess-1", false, nil},
		{"base 0 ignores a passed native", header + msg, 0, Options{SourceScope: "s", Native: "other"}, "sess-1", false, nil},
		{"later bytes use the passed native", msg, 500, Options{SourceScope: "s", Native: "sess-1"}, "sess-1", false, nil},
		{"first record without header is synthetic", msg + header, 0, Options{SourceScope: "s"}, SyntheticNativeIDPrefix + "s", true, nil},
		{"header with empty id is synthetic", `{"type":"session","version":3,"id":"","cwd":"/w"}` + "\n" + msg, 0, Options{SourceScope: "s"}, SyntheticNativeIDPrefix + "s", true, nil},
		{"header with numeric id is synthetic", `{"type":"session","version":3,"id":7,"cwd":"/w"}` + "\n", 0, Options{SourceScope: "s"}, SyntheticNativeIDPrefix + "s", true, nil},
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

	// Without a scope, a source whose first record names no session stops
	// instead of inventing an identity.
	_, err := Parse([]byte(msg), 0, Options{})
	var perr *ParseError
	if !errors.As(err, &perr) || perr.Offset != 0 {
		t.Fatalf("error = %v, want a stop at 0", err)
	}
}

func TestParseRecordNormalization(t *testing.T) {
	entry := func(payload string) string {
		return `{"type":"message","id":"ab000009","parentId":null,"timestamp":"2026-11-13T09:00:00.000Z","message":` + payload + "}"
	}
	tests := []struct {
		name       string
		line       string
		kind, role string
		tool, text string
		prompt     string
		md         map[string]any
	}{
		{"assistant text with signature", entry(`{"role":"assistant","content":[{"type":"text","text":"answer","textSignature":"sig"}],"responseId":"r1"}`),
			"message", "assistant", "", "answer", "", map[string]any{}},
		{"assistant tool call without arguments", entry(`{"role":"assistant","content":[{"type":"toolCall","id":"c","name":"read"}],"responseId":"r2"}`),
			"message", "assistant", "read", "read", "", map[string]any{}},
		{"two tool calls leave the tool ambiguous", entry(`{"role":"assistant","content":[{"type":"toolCall","name":"read"},{"type":"toolCall","name":"bash","arguments":{"command":"ls"}}],"responseId":"r3"}`),
			"message", "assistant", "", "read\nbash {\"command\":\"ls\"}", "",
			map[string]any{"toolNames": []any{"read", "bash"}}},
		{"unknown block type contributes nothing", entry(`{"role":"user","content":[{"type":"image","data":"…"},{"type":"text","text":"see this"}]}`),
			"message", "user", "", "see this", "see this", map[string]any{}},
		{"error tool result", entry(`{"role":"toolResult","toolCallId":"c1","toolName":"bash","content":[{"type":"text","text":"boom"}],"isError":true}`),
			"message", "toolResult", "bash", "boom", "", map[string]any{"toolCallId": "c1", "isError": true}},
		{"string content reads as no text", entry(`{"role":"user","content":"plain"}`),
			"message", "user", "", "", "", map[string]any{}},
		{"missing message object", `{"type":"message","id":"ab000010","timestamp":"2026-11-13T09:00:00.000Z"}`,
			"message", "", "", "", "", map[string]any{}},
		{"non-object message", `{"type":"message","message":[1],"timestamp":"2026-11-13T09:00:00.000Z"}`,
			"message", "", "", "", "", map[string]any{}},
		{"unknown role keeps its text out", entry(`{"role":"future","content":[{"type":"text","text":"x"}]}`),
			"message", "future", "", "", "", map[string]any{}},
		{"empty system sections", entry(`{"role":"system","content":""}`),
			"message", "system", "", "", "", map[string]any{}},
		{"unknown top-level type", `{"type":"brand_new","id":"ab000011","timestamp":"2026-11-13T09:00:00.000Z"}`,
			"unknown", "", "", "", "", map[string]any{"observedType": "brand_new"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data := []byte(tc.line + "\n")
			res, err := Parse(data, 100, Options{Native: "sess-1"})
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

func TestParseTimestampEdges(t *testing.T) {
	tests := []struct {
		line    string
		hasTime bool
	}{
		{`{"type":"model_change","id":"a","timestamp":"2026-11-13T09:00:00.000Z","modelId":"m"}`, true},
		{`{"type":"model_change","id":"a","timestamp":"2026-11-13T10:00:00.000+01:00","modelId":"m"}`, true},
		{`{"type":"model_change","id":"a","timestamp":"not a time","modelId":"m"}`, false},
		{`{"type":"model_change","id":"a","timestamp":1794561000200,"modelId":"m"}`, false},
		{`{"type":"model_change","id":"a","modelId":"m"}`, false},
	}
	for _, tc := range tests {
		data := []byte(tc.line + "\n")
		res, err := Parse(data, 10, Options{Native: "sess-1"})
		if err != nil {
			t.Fatalf("%s: %v", tc.line, err)
		}
		if res.Records[0].HasTime != tc.hasTime {
			t.Errorf("%s: time %t, want %t", tc.line, res.Records[0].HasTime, tc.hasTime)
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
			res, err := Parse([]byte(tc.data), 40, Options{Native: "sess-1"})
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
	rec := `{"type":"model_change","id":"a","modelId":"m"}`
	limit := int64(len(rec))
	// A record exactly at the limit passes.
	if _, err := Parse([]byte(rec+"\n"), 7, Options{Native: "s", MaxRecordBytes: limit}); err != nil {
		t.Fatalf("record at the limit: %v", err)
	}
	var perr *ParseError
	// One byte more stops the source at the record.
	_, err := Parse([]byte(rec+"\n"), 7, Options{Native: "s", MaxRecordBytes: limit - 1})
	if !errors.As(err, &perr) || perr.Offset != 7 || !perr.RecordTooLarge {
		t.Fatalf("oversized record error = %v", err)
	}
	// An incomplete tail already above the limit stops the source too.
	res, err := Parse([]byte(rec+"\n"+rec), 7, Options{Native: "s", MaxRecordBytes: limit - 1})
	if !errors.As(err, &perr) || perr.Offset != 7 || len(res.Records) != 0 || !perr.RecordTooLarge {
		t.Fatalf("tail error = %v", err)
	}
	res, err = Parse([]byte(rec+"\n"+rec), 7, Options{Native: "s", MaxRecordBytes: limit})
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
	for _, name := range []string{"main-session.jsonl", "error-session.jsonl", "anthropic-session.jsonl", "incomplete-tail.jsonl", "malformed-line.jsonl"} {
		data := fixture(t, name)
		a, errA := Parse(data, 0, Options{SourceScope: "s"})
		b, errB := Parse(data, 0, Options{SourceScope: "s"})
		if !reflect.DeepEqual(a, b) || !reflect.DeepEqual(errA, errB) {
			t.Fatalf("%s: parse is not deterministic", name)
		}
	}
}
