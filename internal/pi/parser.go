package pi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const AgentName = "pi"

const SyntheticNativeIDPrefix = "ohya-synthetic:"

const OutputVersion = 2

const DefaultMaxRecordBytes = 1 << 20

var ErrNoNative = errors.New("pi: parse after offset 0 needs Options.Native from the source's session header")

var knownKinds = map[string]bool{
	"session": true, "model_change": true, "thinking_level_change": true,
	"message": true,
}

type Options struct {
	SourceScope string

	Native string

	MaxRecordBytes int64
}

type Record struct {
	Offset    int64
	End       int64
	Raw       string
	Native    string
	Synthetic bool
	Kind      string
	Role      string
	ToolName  string
	Text      string
	Prompt    string
	HasTime   bool
	TimeMS    int64
	Cwd       string
	Model     string
	EntryID   string
	MessageID string
	Usage     json.RawMessage
	Metadata  string
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
	return fmt.Sprintf("pi: source stops at offset %d: %s", e.Offset, e.Reason)
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

	t, _ := stringValue(obj, "type")
	if knownKinds[t] {
		rec.Kind = t
	} else {
		rec.Kind = "unknown"
		if t != "" {
			md["observedType"] = t
		}
	}

	if rec.Kind != "session" {
		rec.EntryID, _ = stringValue(obj, "id")
	}

	if native == "" {
		if id, ok := stringValue(obj, "id"); ok && rec.Kind == "session" && id != "" {
			rec.Native = id
		} else if scope != "" {
			rec.Native = SyntheticNativeIDPrefix + scope
			rec.Synthetic = true
		} else {
			return Record{}, &ParseError{
				Offset: offset,
				Reason: "the source's first record names no session and Options.SourceScope is empty, so no synthetic native ID can be assigned",
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

	switch rec.Kind {
	case "session":
		rec.Cwd, _ = stringValue(obj, "cwd")
		if n, ok := obj["version"].(json.Number); ok {
			md["sessionVersion"] = n
		}
	case "model_change":
		rec.Model, _ = stringValue(obj, "modelId")
		if v, ok := stringValue(obj, "provider"); ok {
			md["provider"] = v
		}
	case "thinking_level_change":
		if v, ok := stringValue(obj, "thinkingLevel"); ok {
			md["thinkingLevel"] = v
		}
	case "message":
		normalizeMessage(&rec, obj, md)
	}

	b, err := json.Marshal(md)
	if err != nil {
		return Record{}, &ParseError{Offset: offset, Reason: "record observations cannot be serialized"}
	}
	rec.Metadata = string(b)
	return rec, nil
}

func normalizeMessage(rec *Record, obj map[string]any, md map[string]any) {
	msg, _ := obj["message"].(map[string]any)
	role, _ := stringValue(msg, "role")
	rec.Role = role
	switch role {
	case "user":
		rec.Text = blockText(msg["content"])
		rec.Prompt = rec.Text
	case "assistant":
		text, tool, names := normalizeAssistantContent(msg["content"])
		rec.Text, rec.ToolName = text, tool
		rec.Model, _ = stringValue(msg, "model")
		rec.MessageID, _ = stringValue(msg, "responseId")
		if u, ok := msg["usage"]; ok {
			if b, err := json.Marshal(u); err == nil {
				rec.Usage = b
			}
		}
		for _, key := range []string{"provider", "api", "stopReason", "rawStopReason", "thinkingLevel"} {
			if v, ok := stringValue(msg, key); ok {
				md[key] = v
			}
		}
		if n, ok := msg["durationMs"].(json.Number); ok {
			md["durationMs"] = n
		}
		if len(names) > 1 {
			md["toolNames"] = names
		}
	case "toolResult":
		rec.ToolName, _ = stringValue(msg, "toolName")
		rec.Text = blockText(msg["content"])
		if v, ok := stringValue(msg, "toolCallId"); ok {
			md["toolCallId"] = v
		}
		if v, ok := msg["isError"].(bool); ok && v {
			md["isError"] = true
		}
	case "system":
		if sections, ok := msg["sections"].(map[string]any); ok && len(sections) > 0 {
			keys := make([]string, 0, len(sections))
			for k := range sections {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			md["sections"] = keys
		}
		if tools, ok := msg["toolsAdded"].([]any); ok && len(tools) > 0 {
			names := []string{}
			for _, item := range tools {
				if tool, ok := item.(map[string]any); ok {
					if name, ok := stringValue(tool, "name"); ok {
						names = append(names, name)
					}
				}
			}
			md["toolsAdded"] = names
		}
	}
}

func normalizeAssistantContent(v any) (text string, tool string, names []string) {
	blocks, ok := v.([]any)
	if !ok {
		return "", "", nil
	}
	var parts []string
	for _, item := range blocks {
		block, ok := item.(map[string]any)
		if !ok {
			continue
		}
		t, _ := stringValue(block, "type")
		switch t {
		case "text":
			if s, ok := stringValue(block, "text"); ok && s != "" {
				parts = append(parts, s)
			}
		case "toolCall":
			name, _ := stringValue(block, "name")
			if name != "" {
				names = append(names, name)
			}
			if args, ok := block["arguments"]; ok {
				if name != "" {
					parts = append(parts, name+" "+compactJSON(args))
				} else {
					parts = append(parts, compactJSON(args))
				}
			} else if name != "" {
				parts = append(parts, name)
			}
		}
	}
	if len(names) == 1 {
		tool = names[0]
	}
	return strings.Join(parts, "\n"), tool, names
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

func compactJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

func stringValue(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}
