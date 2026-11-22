package antigravity

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testConversationID = "11111111-2222-4333-8444-555555555555"

func testSourcePath() string {
	return "/home/dev/.gemini/antigravity-cli/brain/" + testConversationID + "/.system_generated/logs/transcript_full.jsonl"
}

func TestConversationIDFromPath(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{testSourcePath(), testConversationID},
		{"/tmp/brain/abc-123/.system_generated/logs/transcript.jsonl", "abc-123"},
		{"/tmp/brain/abc-123/.system_generated/logs/chunks/transcript_full/00000000.jsonl", ""},
		{"/tmp/other/session.jsonl", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := ConversationIDFromPath(tc.path); got != tc.want {
			t.Errorf("ConversationIDFromPath(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestParseMainSessionFixture(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "antigravity", "main-session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := Parse(data, 0, Options{SourceScope: "src-agy-1", SourcePath: testSourcePath()})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if res.Native != testConversationID {
		t.Fatalf("native = %q, want %q", res.Native, testConversationID)
	}
	if res.Cursor != int64(len(data)) {
		t.Fatalf("cursor = %d, want %d", res.Cursor, len(data))
	}
	if len(res.Records) != 6 {
		t.Fatalf("records = %d, want 6", len(res.Records))
	}

	sys := res.Records[0]
	if sys.Kind != "system_message" || sys.Role != "system" || sys.Text != "" || sys.Cwd != "/home/dev/project" {
		t.Errorf("system record = %+v", sys)
	}

	user := res.Records[1]
	if user.Kind != "user_input" || user.Role != "user" || user.Prompt != "fix the auth bug" || user.Text != "fix the auth bug" {
		t.Errorf("user record = %+v", user)
	}

	toolCall := res.Records[2]
	if toolCall.Kind != "planner_response" || toolCall.Role != "assistant" || toolCall.Model != "gemini-4-neon-high" || toolCall.ToolName != "read_file" {
		t.Errorf("planner tool call = %+v", toolCall)
	}
	if !strings.Contains(toolCall.Text, `read_file {"Path":"/home/dev/project/auth.go"}`) {
		t.Errorf("planner tool text = %q", toolCall.Text)
	}
	if strings.Contains(toolCall.Text, "secret internal thought") {
		t.Errorf("thinking must not leak into searchable text: %q", toolCall.Text)
	}
	if toolCall.MessageID != "step-2" {
		t.Errorf("message id = %q, want step-2", toolCall.MessageID)
	}
	var u struct {
		InputTokens     int64 `json:"input_tokens"`
		CacheReadTokens int64 `json:"cache_read_tokens"`
		OutputTokens    int64 `json:"output_tokens"`
	}
	if err := json.Unmarshal(toolCall.Usage, &u); err != nil || u.InputTokens != 500 || u.CacheReadTokens != 4200 || u.OutputTokens != 120 {
		t.Errorf("usage = %s (%+v, err=%v)", string(toolCall.Usage), u, err)
	}

	toolRes := res.Records[3]
	if toolRes.Kind != "generic" || toolRes.Role != "tool" || !strings.Contains(toolRes.Text, "package auth") {
		t.Errorf("generic tool result = %+v", toolRes)
	}

	reply := res.Records[4]
	if reply.Kind != "planner_response" || reply.Role != "assistant" || reply.ToolName != "" || reply.Text != "Fixed the nil pointer in auth.go." {
		t.Errorf("planner reply = %+v", reply)
	}

	chk := res.Records[5]
	if chk.Kind != "checkpoint" || chk.Role != "system" || !strings.Contains(chk.Text, "Summary of auth fix") {
		t.Errorf("checkpoint = %+v", chk)
	}
	if len(chk.Usage) == 0 {
		t.Error("checkpoint usage must be preserved")
	}
}

func TestParseSyntheticFallbackAndIncompleteTail(t *testing.T) {
	line := `{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-11-22T20:00:00Z","content":"hello"}` + "\n"
	tail := `{"step_index":1,"source":"MODEL"`
	res, err := Parse([]byte(line+tail), 0, Options{SourceScope: "src-9"})
	if err != nil {
		t.Fatalf("incomplete tail must not error: %v", err)
	}
	if len(res.Records) != 1 || res.Cursor != int64(len(line)) {
		t.Fatalf("got %d records, cursor %d", len(res.Records), res.Cursor)
	}
	if !res.Records[0].Synthetic || res.Records[0].Native != SyntheticNativeIDPrefix+"src-9" {
		t.Fatalf("synthetic native = %+v", res.Records[0])
	}
	if _, err := Parse([]byte(line), 0, Options{}); !errors.Is(err, ErrNoNative) {
		t.Fatalf("missing all identity sources err = %v, want ErrNoNative", err)
	}
}

func TestParseStopsOnMalformedOrOversize(t *testing.T) {
	good := `{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-11-22T20:00:00Z","content":"hi"}` + "\n"
	bad := "not json\n"
	res, err := Parse([]byte(good+bad), 0, Options{SourceScope: "s"})
	var perr *ParseError
	if !errors.As(err, &perr) || perr.Offset != int64(len(good)) || perr.RecordTooLarge {
		t.Fatalf("err = %v, want ParseError at %d", err, len(good))
	}
	if len(res.Records) != 1 || res.Cursor != int64(len(good)) {
		t.Fatalf("valid prefix must survive: %+v", res)
	}

	_, err = Parse([]byte(good), 0, Options{SourceScope: "s", MaxRecordBytes: 10})
	if !errors.As(err, &perr) || !perr.RecordTooLarge {
		t.Fatalf("oversize err = %v, want RecordTooLarge", err)
	}
}

func FuzzParse(f *testing.F) {
	f.Add([]byte(`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-11-22T20:00:00Z","content":"<USER_REQUEST>\nhi\n</USER_REQUEST>"}` + "\n"))
	f.Add([]byte(`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","model":"gemini-4-neon-high","tool_calls":[{"name":"read_file","args":{"Path":"/a"}}]}` + "\n"))
	f.Add([]byte("[]\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		res, _ := Parse(data, 0, Options{SourceScope: "fuzz", SourcePath: testSourcePath(), MaxRecordBytes: 1024})
		if res.Cursor < 0 || res.Cursor > int64(len(data)) {
			t.Fatalf("cursor %d out of bounds for len %d", res.Cursor, len(data))
		}
	})
}
