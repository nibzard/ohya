package claudecode

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

// fixture reads a synthetic Claude Code fixture; byte offsets in the
// assertions below come from testdata/claude-code/README.md.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "claude-code", name))
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
// verbatim, valid metadata, and a cursor that never moves backwards.
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

func TestParseMainSession(t *testing.T) {
	const native = "00000000-0000-4000-8000-0000000000a1"
	usage1 := `{"cache_creation_input_tokens":0,"cache_read_input_tokens":48,"input_tokens":120,"output_tokens":35,"service_tier":"standard"}`
	usage2 := `{"cache_creation_input_tokens":0,"cache_read_input_tokens":380,"input_tokens":60,"output_tokens":12,"service_tier":"standard"}`
	tests := []struct {
		off, end           int64
		kind, role, tool   string
		text, msgID, model string
		hasTime            bool
		timeMS             int64
	}{
		{0, 186, "queue-operation", "", "", "", "", "", true, ms(9, 0, 0, 0)},
		{186, 269, "mode", "", "", "", "", "", false, 0},
		{269, 673, "user", "user", "", "Find the highest prime below 20.", "", "", true, ms(9, 0, 1, 0)},
		{673, 1086, "attachment", "", "", "Snapshot of the fixture prompt context", "", "", true, ms(9, 0, 1, 0)},
		{1086, 1847, "assistant", "assistant", "", "Fixture reasoning: check 19 for factors.", "msg_fixture_0001", "fixture-model-v1", true, ms(9, 0, 2, 0)},
		{1847, 2565, "assistant", "assistant", "", "19 is prime; I will verify with a command.", "msg_fixture_0001", "fixture-model-v1", true, ms(9, 0, 2, 0)},
		{2565, 3347, "assistant", "assistant", "Bash", `Bash {"command":"echo 19","description":"Print the candidate prime"}`, "msg_fixture_0001", "fixture-model-v1", true, ms(9, 0, 2, 0)},
		{3347, 3995, "user", "user", "", "19", "", "", true, ms(9, 0, 3, 0)},
		{3995, 4410, "attachment", "", "", "Fixture token reminder placeholder", "", "", true, ms(9, 0, 3, 100)},
		{4410, 4848, "system", "", "", "Fixture turn duration record", "", "", true, ms(9, 0, 3, 200)},
		{4848, 5557, "assistant", "assistant", "", "The highest prime below 20 is 19.", "msg_fixture_0002", "fixture-model-v1", true, ms(9, 0, 4, 0)},
		{5557, 5663, "atis-latch", "", "", "", "", "", false, 0},
		{5663, 5768, "permission-mode", "", "", "", "", "", false, 0},
		{5768, 5940, "last-prompt", "", "", "", "", "", false, 0},
		{5940, 6044, "ai-title", "", "", "", "", "", false, 0},
		{6044, 6528, "cost-state", "", "", "", "", "", false, 0},
	}
	res, err := Parse(fixture(t, "main-session.jsonl"), 0, Options{SourceScope: "src-main"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Records) != len(tests) {
		t.Fatalf("got %d records, want %d", len(res.Records), len(tests))
	}
	if res.Cursor != 6528 {
		t.Fatalf("cursor %d, want 6528", res.Cursor)
	}
	checkInvariants(t, fixture(t, "main-session.jsonl"), 0, res)
	for i, want := range tests {
		got := res.Records[i]
		if got.Offset != want.off || got.End != want.end {
			t.Errorf("record %d bytes [%d, %d), want [%d, %d)", i, got.Offset, got.End, want.off, want.end)
		}
		if got.Kind != want.kind || got.Role != want.role || got.ToolName != want.tool {
			t.Errorf("record %d kind/role/tool = %q/%q/%q, want %q/%q/%q",
				i, got.Kind, got.Role, got.ToolName, want.kind, want.role, want.tool)
		}
		if got.Text != want.text {
			t.Errorf("record %d text = %q, want %q", i, got.Text, want.text)
		}
		if got.MessageID != want.msgID || got.Model != want.model {
			t.Errorf("record %d message/model = %q/%q, want %q/%q", i, got.MessageID, got.Model, want.msgID, want.model)
		}
		if got.HasTime != want.hasTime || got.TimeMS != want.timeMS {
			t.Errorf("record %d time = (%t, %d), want (%t, %d)", i, got.HasTime, got.TimeMS, want.hasTime, want.timeMS)
		}
		if got.Native != native || got.Synthetic {
			t.Errorf("record %d native = %q (synthetic %t), want %q", i, got.Native, got.Synthetic, native)
		}
	}

	prompt := res.Records[2]
	md := metadataOf(t, prompt)
	if md["contentForm"] != "string" || md["uuid"] != "00000000-0000-4000-8000-0000000000b1" {
		t.Errorf("prompt metadata: %v", md)
	}
	if _, ok := md["parentUuid"]; ok {
		t.Errorf("null parentUuid must be omitted: %v", md)
	}
	if prompt.Cwd != "/home/dev/fixture-project" || prompt.GitBranch != "main" {
		t.Errorf("prompt cwd/branch = %q/%q", prompt.Cwd, prompt.GitBranch)
	}

	thinking := metadataOf(t, res.Records[4])
	if thinking["apiBlockIndex"] != float64(0) {
		t.Errorf("thinking apiBlockIndex: %v", thinking["apiBlockIndex"])
	}
	if string(res.Records[4].Usage) != usage1 || string(res.Records[5].Usage) != usage1 || string(res.Records[6].Usage) != usage1 {
		t.Error("records of one response must surface the same usage object")
	}
	if string(res.Records[10].Usage) != usage2 {
		t.Errorf("second response usage = %s", res.Records[10].Usage)
	}

	toolUse := metadataOf(t, res.Records[6])["blocks"].([]any)
	if len(toolUse) != 1 {
		t.Fatalf("tool_use blocks: %v", toolUse)
	}
	block := toolUse[0].(map[string]any)
	if block["name"] != "Bash" || block["id"] != "toolu_fixture_0001" {
		t.Errorf("tool_use block: %v", block)
	}
	if block["input"].(map[string]any)["command"] != "echo 19" {
		t.Errorf("tool_use input: %v", block["input"])
	}

	toolResult := metadataOf(t, res.Records[7])
	blocks := toolResult["blocks"].([]any)
	if blocks[0].(map[string]any)["tool_use_id"] != "toolu_fixture_0001" {
		t.Errorf("tool_result block: %v", blocks[0])
	}
	if toolResult["toolUseResult"].(map[string]any)["stdout"] != "19\n" {
		t.Errorf("toolUseResult: %v", toolResult["toolUseResult"])
	}
	if toolResult["sourceToolAssistantUUID"] != "00000000-0000-4000-8000-0000000000b5" {
		t.Errorf("sourceToolAssistantUUID: %v", toolResult["sourceToolAssistantUUID"])
	}

	if md := metadataOf(t, res.Records[8]); md["attachmentType"] != "total_tokens_reminder" {
		t.Errorf("attachment type: %v", md["attachmentType"])
	}
	system := metadataOf(t, res.Records[9])
	if system["subtype"] != "turn_duration" || system["level"] != "info" {
		t.Errorf("system metadata: %v", system)
	}
	if res.Records[15].Metadata != "{}" {
		t.Errorf("cost-state metadata = %q, want {}", res.Records[15].Metadata)
	}
}

func TestParseSubagentSession(t *testing.T) {
	const parent = "00000000-0000-4000-8000-0000000000a1"
	const scope = "src-sub"
	res, err := Parse(fixture(t, "subagent-session.jsonl"), 0, Options{SourceScope: scope})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Records) != 6 || res.Cursor != 2956 {
		t.Fatalf("got %d records, cursor %d; want 6 records, cursor 2956", len(res.Records), res.Cursor)
	}
	checkInvariants(t, fixture(t, "subagent-session.jsonl"), 0, res)

	// started and result carry no sessionId: they must land in the
	// source-scoped synthetic session, never merged into the parent's.
	for _, i := range []int{0, 5} {
		if got := res.Records[i]; got.Native != SyntheticNativeIDPrefix+scope || !got.Synthetic {
			t.Errorf("record %d native = %q (synthetic %t), want %q", i, got.Native, got.Synthetic, SyntheticNativeIDPrefix+scope)
		}
	}
	for i := 1; i <= 4; i++ {
		if got := res.Records[i]; got.Native != parent || got.Synthetic {
			t.Errorf("record %d native = %q, want parent %q", i, got.Native, parent)
		}
	}

	if got := res.Records[1]; got.Text != "Check the fixture notes file." || got.Role != "user" {
		t.Errorf("subagent prompt: %q role %q", got.Text, got.Role)
	}
	read := res.Records[2]
	if read.ToolName != "Read" || read.Text != `Read {"file_path":"/home/dev/fixture-project/notes.txt"}` {
		t.Errorf("tool_use record: tool %q text %q", read.ToolName, read.Text)
	}
	if read.MessageID != "msg_fixture_0003" || res.Records[4].MessageID != "msg_fixture_0003" {
		t.Error("records of one subagent response must share message.id")
	}
	if string(read.Usage) != string(res.Records[4].Usage) {
		t.Error("records of one subagent response must surface the same usage")
	}

	promptMD := metadataOf(t, res.Records[1])
	if promptMD["isSidechain"] != true || promptMD["agentId"] != "agent-0123456789abcdef" {
		t.Errorf("sidechain metadata: %v", promptMD)
	}
	resultMD := metadataOf(t, res.Records[5])
	if resultMD["agentId"] != "agent-0123456789abcdef" {
		t.Errorf("result metadata: %v", resultMD)
	}
	if got := res.Records[3]; got.Text != "1     fixture note line" || got.Role != "user" {
		t.Errorf("tool result: %q role %q", got.Text, got.Role)
	}
	fileMD := metadataOf(t, res.Records[3])["toolUseResult"].(map[string]any)["file"].(map[string]any)
	if fileMD["filePath"] != "/home/dev/fixture-project/notes.txt" {
		t.Errorf("toolUseResult file: %v", fileMD)
	}
}

func TestParseIncompleteTail(t *testing.T) {
	data := fixture(t, "incomplete-tail.jsonl")
	res, err := Parse(data, 0, Options{SourceScope: "src-tail"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Records) != 3 || res.Cursor != 1545 {
		t.Fatalf("got %d records, cursor %d; want 3 records, cursor 1545", len(res.Records), res.Cursor)
	}
	checkInvariants(t, data, 0, res)

	// The next poll resumes at the cursor; the completed line parses then.
	completion := []byte(` mid-write."}],"stop_reason":"end_turn"},"timestamp":"2026-11-13T10:00:03.000Z"}` + "\n")
	next := append(append([]byte{}, data[1545:]...), completion...)
	res2, err := Parse(next, 1545, Options{SourceScope: "src-tail"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.Records) != 1 || res2.Records[0].Kind != "assistant" || res2.Records[0].Offset != 1545 {
		t.Fatalf("resumed tail: %+v", res2.Records)
	}
	if res2.Records[0].Text != "This record is cut mid-write." {
		t.Errorf("resumed text: %q", res2.Records[0].Text)
	}
	if res2.Cursor != 1997+int64(len(completion)) {
		t.Errorf("resumed cursor %d", res2.Cursor)
	}
}

func TestParseMalformedLineStopsSource(t *testing.T) {
	data := fixture(t, "malformed-line.jsonl")
	res, err := Parse(data, 0, Options{SourceScope: "src-bad"})
	var perr *ParseError
	if !errors.As(err, &perr) {
		t.Fatalf("want ParseError, got %v", err)
	}
	if perr.Offset != 1094 {
		t.Errorf("error offset %d, want 1094", perr.Offset)
	}
	if perr.Reason != "record is not valid JSON" {
		t.Errorf("reason %q", perr.Reason)
	}
	if strings.Contains(perr.Error(), "unterminated") {
		t.Errorf("diagnostic leaks transcript content: %s", perr.Error())
	}
	if len(res.Records) != 2 || res.Cursor != 1094 {
		t.Fatalf("got %d records, cursor %d; want 2 records, cursor 1094", len(res.Records), res.Cursor)
	}
	if res.Records[0].Text != "Record before the malformed line." || res.Records[1].Text != "Valid record before corruption." {
		t.Errorf("records before the failure: %q, %q", res.Records[0].Text, res.Records[1].Text)
	}
	checkInvariants(t, data[:1094], 0, res)
}

func TestParseSyntheticNativeID(t *testing.T) {
	line := []byte(`{"type":"user","message":{"role":"user","content":"no identity"}}` + "\n")
	withScope := func(scope string) Options { return Options{SourceScope: scope} }

	a, err := Parse(line, 0, withScope("scope-a"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Parse(line, 0, withScope("scope-b"))
	if err != nil {
		t.Fatal(err)
	}
	if !a.Records[0].Synthetic || a.Records[0].Native != SyntheticNativeIDPrefix+"scope-a" {
		t.Errorf("synthetic native = %q", a.Records[0].Native)
	}
	if a.Records[0].Native == b.Records[0].Native {
		t.Error("different scopes must not share a synthetic session")
	}
	if strings.HasPrefix("00000000-0000-4000-8000-0000000000a1", SyntheticNativeIDPrefix) {
		t.Error("synthetic prefix collides with the UUID namespace")
	}

	again, err := Parse(line, 0, withScope("scope-a"))
	if err != nil || again.Records[0].Native != a.Records[0].Native {
		t.Error("the same scope must yield the same synthetic ID")
	}

	if _, err := Parse(line, 0, Options{}); err == nil {
		t.Error("missing sessionId without a source scope must fail, not guess")
	}

	// A non-string sessionId reads as absent identity, not as an ID.
	nonString := []byte(`{"type":"user","sessionId":42}` + "\n")
	res, err := Parse(nonString, 0, withScope("scope-a"))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Records[0].Synthetic {
		t.Errorf("non-string sessionId must be synthetic, got %q", res.Records[0].Native)
	}
}

func TestParseRecordSizeLimit(t *testing.T) {
	small := `{"type":"mode","sessionId":"s1","mode":"a"}`
	big := `{"type":"mode","sessionId":"s1","mode":"` + strings.Repeat("x", 100) + `"}`
	data := []byte(small + "\n" + big + "\n")

	res, err := Parse(data, 0, Options{SourceScope: "s", MaxRecordBytes: 64})
	var perr *ParseError
	if !errors.As(err, &perr) || perr.Offset != int64(len(small)+1) {
		t.Fatalf("want oversized error at %d, got %v", len(small)+1, err)
	}
	if !strings.Contains(perr.Reason, "64") || strings.Contains(perr.Error(), "xxxx") {
		t.Errorf("reason must cite the limit without content: %s", perr.Error())
	}
	if len(res.Records) != 1 || res.Cursor != int64(len(small)+1) {
		t.Fatalf("records before the limit: %d, cursor %d", len(res.Records), res.Cursor)
	}

	// A tail already above the limit can never become valid; the source
	// stops at the tail instead of waiting forever.
	partial := []byte(small + "\n" + big[:80])
	res, err = Parse(partial, 0, Options{SourceScope: "s", MaxRecordBytes: 64})
	if !errors.As(err, &perr) || perr.Offset != int64(len(small)+1) {
		t.Fatalf("want incomplete-tail error at %d, got %v", len(small)+1, err)
	}
	if !strings.Contains(perr.Reason, "incomplete record already exceeds") {
		t.Errorf("reason %q", perr.Reason)
	}
	if len(res.Records) != 1 {
		t.Fatalf("records before the tail: %d", len(res.Records))
	}

	// Unset or non-positive limits select the documented default.
	for _, limit := range []int64{0, -1} {
		res, err := Parse(data, 0, Options{SourceScope: "s", MaxRecordBytes: limit})
		if err != nil || len(res.Records) != 2 {
			t.Errorf("limit %d: %v, %d records", limit, err, len(res.Records))
		}
	}
}

func TestParseUnknownAndMalformedRecords(t *testing.T) {
	tests := []struct {
		name   string
		line   string
		kind   string
		reason string
	}{
		{name: "unrecognized type", line: `{"type":"brand-new-thing","sessionId":"s1"}`, kind: "unknown"},
		{name: "missing type", line: `{"sessionId":"s1"}`, kind: "unknown"},
		{name: "non-string type", line: `{"type":7,"sessionId":"s1"}`, kind: "unknown"},
		{name: "json array", line: `[1,2]`, reason: "record is not a JSON object"},
		{name: "json null", line: `null`, reason: "record is not a JSON object"},
		{name: "json string", line: `"hello"`, reason: "record is not a JSON object"},
		{name: "empty line", line: ``, reason: "record is not valid JSON"},
		{name: "truncated json", line: `{"sessionId":"s1"`, reason: "record is not valid JSON"},
		{name: "two objects", line: `{"sessionId":"s1"} {"sessionId":"s2"}`, reason: "record has content after the JSON object"},
		{name: "trailing garbage", line: `{"sessionId":"s1"}z`, reason: "record has content after the JSON object"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Parse([]byte(tc.line+"\n"), 0, Options{SourceScope: "s"})
			if tc.reason != "" {
				var perr *ParseError
				if !errors.As(err, &perr) || perr.Reason != tc.reason || perr.Offset != 0 {
					t.Fatalf("want error %q at 0, got %v", tc.reason, err)
				}
				if len(res.Records) != 0 || res.Cursor != 0 {
					t.Fatalf("no record may be accepted: %d records, cursor %d", len(res.Records), res.Cursor)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Records) != 1 || res.Records[0].Kind != tc.kind {
				t.Fatalf("got %+v", res.Records)
			}
			md := metadataOf(t, res.Records[0])
			if _, ok := md["observedType"]; tc.name == "unrecognized type" && !ok {
				t.Errorf("metadata must record the observed type: %v", md)
			}
			if _, ok := md["observedType"]; tc.name != "unrecognized type" && ok {
				t.Errorf("metadata must not invent a type: %v", md)
			}
		})
	}

	invalid := []byte("{\"type\":\"user\",\"sessionId\":\"s\xff\"}\n")
	if _, err := Parse(invalid, 0, Options{SourceScope: "s"}); err == nil {
		t.Error("invalid UTF-8 must be rejected before it reaches SQLite TEXT")
	} else {
		var perr *ParseError
		if !errors.As(err, &perr) || perr.Reason != "record is not valid UTF-8" {
			t.Errorf("reason %v", err)
		}
	}
}

func TestParseCompoundRecord(t *testing.T) {
	line := `{"type":"assistant","sessionId":"s1","message":{"role":"assistant","id":"m1","content":[` +
		`{"type":"text","text":"first"},` +
		`{"type":"thinking","thinking":"second"},` +
		`{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}},` +
		`{"type":"tool_use","id":"t2","name":"Read","input":{"file_path":"x"}},` +
		`{"type":"mcp_tool_result","content":"fifth"},` +
		`{"type":"new-block","payload":1}]}}` + "\n"
	res, err := Parse([]byte(line), 0, Options{SourceScope: "s"})
	if err != nil {
		t.Fatal(err)
	}
	r := res.Records[0]
	want := "first\nsecond\nBash {\"command\":\"ls\"}\nRead {\"file_path\":\"x\"}\nfifth"
	if r.Text != want {
		t.Errorf("text = %q, want %q", r.Text, want)
	}
	if r.ToolName != "" {
		t.Errorf("two tools make the tool field ambiguous, got %q", r.ToolName)
	}
	md := metadataOf(t, r)
	blocks := md["blocks"].([]any)
	if len(blocks) != 6 {
		t.Fatalf("blocks: %v", blocks)
	}
	if blocks[5].(map[string]any)["type"] != "new-block" {
		t.Errorf("unknown block must stay in metadata: %v", blocks[5])
	}

	single := `{"type":"assistant","sessionId":"s1","message":{"role":"assistant","content":[` +
		`{"type":"text","text":"hi"},{"type":"mcp_tool_use","tool_name":"search","input":{"q":"x"}}]}}` + "\n"
	res, err = Parse([]byte(single), 0, Options{SourceScope: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Records[0].ToolName != "search" {
		t.Errorf("one tool use must set the tool field, got %q", res.Records[0].ToolName)
	}

	// A structured tool_result content array contributes its text blocks.
	structured := `{"type":"user","sessionId":"s1","message":{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"a"},{"type":"image"}]}]}}` + "\n"
	res, err = Parse([]byte(structured), 0, Options{SourceScope: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Records[0].Text != "a" {
		t.Errorf("structured tool_result text = %q", res.Records[0].Text)
	}
	if res.Records[0].ToolName != "" {
		t.Errorf("tool results carry no tool name, got %q", res.Records[0].ToolName)
	}
}

func TestParseRoleAndTimestampEdges(t *testing.T) {
	// The type of a message record stands in for a missing message.role.
	noRole := []byte(`{"type":"user","sessionId":"s1","message":{"content":"prompt"}}` + "\n")
	res, err := Parse(noRole, 0, Options{SourceScope: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Records[0].Role != "user" {
		t.Errorf("role = %q, want user", res.Records[0].Role)
	}

	// Without a message object there is no role to report.
	noMessage := []byte(`{"type":"assistant","sessionId":"s1"}` + "\n")
	res, err = Parse(noMessage, 0, Options{SourceScope: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Records[0].Role != "" || res.Records[0].Kind != "assistant" {
		t.Errorf("role = %q, kind = %q", res.Records[0].Role, res.Records[0].Kind)
	}

	// An unparseable timestamp is an absent observation, not a failure.
	badTime := []byte(`{"type":"user","sessionId":"s1","timestamp":"yesterday","message":{"content":"x"}}` + "\n")
	res, err = Parse(badTime, 0, Options{SourceScope: "s"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Records[0].HasTime || res.Records[0].TimeMS != 0 {
		t.Errorf("time = (%t, %d)", res.Records[0].HasTime, res.Records[0].TimeMS)
	}
}

func TestParseEmptyInput(t *testing.T) {
	for _, data := range [][]byte{nil, {}} {
		res, err := Parse(data, 7, Options{SourceScope: "s"})
		if err != nil || len(res.Records) != 0 || res.Cursor != 7 {
			t.Errorf("empty input: %v, %+v", err, res)
		}
	}
}

func TestParseBaseOffset(t *testing.T) {
	data := fixture(t, "subagent-session.jsonl")
	const base = 5000
	res, err := Parse(data, base, Options{SourceScope: "src-sub"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Cursor != base+2956 {
		t.Errorf("cursor %d, want %d", res.Cursor, base+2956)
	}
	checkInvariants(t, data, base, res)
	if res.Records[0].Offset != base || res.Records[0].End != base+139 {
		t.Errorf("first record bytes [%d, %d)", res.Records[0].Offset, res.Records[0].End)
	}
}

func TestParseIsDeterministic(t *testing.T) {
	for _, name := range []string{"main-session.jsonl", "subagent-session.jsonl", "incomplete-tail.jsonl", "malformed-line.jsonl"} {
		data := fixture(t, name)
		first, err1 := Parse(data, 3, Options{SourceScope: "scope"})
		second, err2 := Parse(data, 3, Options{SourceScope: "scope"})
		if !reflect.DeepEqual(first, second) || !reflect.DeepEqual(err1, err2) {
			t.Errorf("%s: parse is not deterministic", name)
		}
		if first.Cursor < 3 {
			t.Errorf("%s: cursor %d moved before the base", name, first.Cursor)
		}
		checkInvariants(t, data, 3, first)
	}
}
