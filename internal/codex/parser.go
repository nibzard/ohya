package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nibzard/ohya/internal/workspace"
)

const AgentName = "codex"

const SyntheticNativeIDPrefix = "ohya-synthetic:"

const OutputVersion = 3

const DefaultMaxRecordBytes = 32 << 20

var ErrNoNative = errors.New("codex: parse after offset 0 needs Options.Native from the source's first record")

var knownKinds = map[string]bool{
	"session_meta": true, "response_item": true, "event_msg": true,
	"turn_context": true, "token_usage_record": true, "world_state": true,
	"compacted": true, "inter_agent_communication_metadata": true,
}

type Options struct {
	SourceScope string

	Native string

	MaxRecordBytes int64
}

type Record struct {
	Offset     int64
	End        int64
	Raw        string
	Native     string
	Synthetic  bool
	Kind       string
	Role       string
	ToolName   string
	Text       string
	Prompt     string
	HasTime    bool
	TimeMS     int64
	HasOrdinal bool
	Ordinal    int64
	Cwd        string
	GitBranch  string
	Repo       string
	Model      string
	ResponseID string
	Usage      json.RawMessage
	Metadata   string
}

type Result struct {
	Records []Record
	Cursor  int64
	Native  string
}

type ParseError struct {
	Offset         int64
	Reason         string
	RecordTooLarge bool
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("codex: source stops at offset %d: %s", e.Offset, e.Reason)
}

func Parse(data []byte, base int64, o Options) (Result, error) {
	limit := o.MaxRecordBytes
	if limit <= 0 {
		limit = DefaultMaxRecordBytes
	}
	res := Result{Cursor: base}
	if base > 0 {
		if o.Native == "" {
			return res, ErrNoNative
		}
		res.Native = o.Native
	}
	rel := 0
	for rel < len(data) {
		idx := bytes.IndexByte(data[rel:], '\n')
		if idx < 0 {
			if int64(len(data)-rel) > limit {
				return res, &ParseError{
					Offset:         base + int64(rel),
					Reason:         fmt.Sprintf("incomplete record already exceeds the %d-byte record size limit", limit),
					RecordTooLarge: true,
				}
			}
			return res, nil
		}
		line := data[rel : rel+idx]
		offset := base + int64(rel)
		if int64(len(line)) > limit {
			return res, &ParseError{
				Offset:         offset,
				Reason:         fmt.Sprintf("record exceeds the %d-byte record size limit", limit),
				RecordTooLarge: true,
			}
		}
		rec, err := parseRecord(line, offset, res.Native, o.SourceScope)
		if err != nil {
			return res, err
		}
		rec.End = offset + int64(len(line)) + 1
		res.Native = rec.Native
		res.Records = append(res.Records, rec)
		rel += idx + 1
		res.Cursor = rec.End
	}
	return res, nil
}

func decodeObject(line []byte, offset int64) (map[string]any, error) {
	if !utf8.Valid(line) {
		return nil, &ParseError{Offset: offset, Reason: "record is not valid UTF-8"}
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		if _, ok := err.(*json.UnmarshalTypeError); ok {
			return nil, &ParseError{Offset: offset, Reason: "record is not a JSON object"}
		}
		return nil, &ParseError{Offset: offset, Reason: "record is not valid JSON"}
	}
	if obj == nil {
		return nil, &ParseError{Offset: offset, Reason: "record is not a JSON object"}
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, &ParseError{Offset: offset, Reason: "record has content after the JSON object"}
	}
	return obj, nil
}

func parseRecord(line []byte, offset int64, native, scope string) (Record, error) {
	obj, err := decodeObject(line, offset)
	if err != nil {
		return Record{}, err
	}
	rec := Record{Offset: offset, Raw: string(line), Native: native}
	md := map[string]any{}
	payload, _ := obj["payload"].(map[string]any)

	t, _ := stringValue(obj, "type")
	if knownKinds[t] {
		rec.Kind = t
	} else {
		rec.Kind = "unknown"
		if t != "" {
			md["observedType"] = t
		}
	}
	if pt, ok := stringValue(payload, "type"); ok && pt != "" {
		md["payloadType"] = pt
	}

	if native == "" {
		if id, ok := stringValue(payload, "id"); ok && rec.Kind == "session_meta" && id != "" {
			rec.Native = id
		} else if scope != "" {
			rec.Native = SyntheticNativeIDPrefix + scope
			rec.Synthetic = true
		} else {
			return Record{}, &ParseError{
				Offset: offset,
				Reason: "the source's first record names no thread and Options.SourceScope is empty, so no synthetic native ID can be assigned",
			}
		}
	} else if strings.HasPrefix(native, SyntheticNativeIDPrefix) {
		rec.Synthetic = true
	}

	if ts, ok := stringValue(obj, "timestamp"); ok {
		if parsed, err := time.Parse(time.RFC3339, ts); err == nil {
			rec.TimeMS = parsed.UnixMilli()
			rec.HasTime = true
		}
	}
	if n, ok := obj["ordinal"].(json.Number); ok && rec.Kind != "session_meta" {
		if v, err := n.Int64(); err == nil && v >= 0 {
			rec.Ordinal, rec.HasOrdinal = v, true
		}
	}

	switch rec.Kind {
	case "session_meta":
		normalizeSessionMeta(&rec, payload, md)
	case "response_item":
		normalizeResponseItem(&rec, payload, md)
	case "event_msg":
		if pt, _ := stringValue(payload, "type"); pt == "user_message" {
			rec.Prompt, _ = stringValue(payload, "message")
		}
	case "turn_context":
		rec.Cwd, _ = stringValue(payload, "cwd")
		rec.Model, _ = stringValue(payload, "model")
		if v, ok := stringValue(payload, "turn_id"); ok {
			md["turnId"] = v
		}
	case "token_usage_record":
		rec.ResponseID, _ = stringValue(payload, "response_id")
		if u, ok := payload["usage"]; ok {
			if b, err := json.Marshal(u); err == nil {
				rec.Usage = b
			}
		}
		if v, ok := stringValue(payload, "turn_id"); ok {
			md["turnId"] = v
		}
	}

	b, err := json.Marshal(md)
	if err != nil {
		return Record{}, &ParseError{Offset: offset, Reason: "record observations cannot be serialized"}
	}
	rec.Metadata = string(b)
	return rec, nil
}

func normalizeSessionMeta(rec *Record, payload map[string]any, md map[string]any) {
	id, _ := stringValue(payload, "id")
	if id != "" {
		md["threadId"] = id
	}
	if id != rec.Native {
		md["embedded"] = true
		return
	}
	rec.Cwd, _ = stringValue(payload, "cwd")
	if git, ok := payload["git"].(map[string]any); ok {
		rec.GitBranch, _ = stringValue(git, "branch")
		if u, ok := stringValue(git, "repository_url"); ok {
			rec.Repo = workspace.NormalizeRemote(u)
		}
	}
	for _, key := range []string{"originator", "cli_version", "history_mode", "parent_thread_id", "forked_from_id"} {
		if v, ok := stringValue(payload, key); ok {
			md[key] = v
		}
	}
	switch src := payload["source"].(type) {
	case string:
		md["source"] = src
	case map[string]any:
		if _, ok := src["subagent"]; ok {
			md["source"] = "subagent"
		}
	}
}

func normalizeResponseItem(rec *Record, payload map[string]any, md map[string]any) {
	pt, _ := stringValue(payload, "type")
	switch pt {
	case "message":
		rec.Role, _ = stringValue(payload, "role")
		rec.Text = blockText(payload["content"])
		if v, ok := stringValue(payload, "phase"); ok {
			md["phase"] = v
		}
	case "reasoning":
		rec.Text = joinText(blockText(payload["summary"]), blockText(payload["content"]))
	case "function_call", "custom_tool_call":
		rec.ToolName, _ = stringValue(payload, "name")
		input, _ := stringValue(payload, "arguments")
		if pt == "custom_tool_call" {
			input, _ = stringValue(payload, "input")
		}
		rec.Text = joinWords(rec.ToolName, input)
	case "function_call_output", "custom_tool_call_output":
		switch out := payload["output"].(type) {
		case string:
			rec.Text = out
		case []any:
			rec.Text = blockText(out)
		}
	case "web_search_call":
		if action, ok := payload["action"].(map[string]any); ok {
			rec.Text, _ = stringValue(action, "query")
		}
	}
	if v, ok := stringValue(payload, "call_id"); ok {
		md["callId"] = v
	}
	if v, ok := stringValue(payload, "id"); ok {
		md["itemId"] = v
	}
}

func blockText(v any) string {
	blocks, ok := v.([]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, item := range blocks {
		if block, ok := item.(map[string]any); ok {
			if s, ok := stringValue(block, "text"); ok && s != "" {
				parts = append(parts, s)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func joinText(parts ...string) string {
	return joinNonEmpty("\n", parts)
}

func joinWords(parts ...string) string {
	return joinNonEmpty(" ", parts)
}

func joinNonEmpty(sep string, parts []string) string {
	kept := parts[:0:0]
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, sep)
}

func stringValue(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}
