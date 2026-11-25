package claudecode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

const AgentName = "claude-code"

const SyntheticNativeIDPrefix = "ohya-synthetic:"

const OutputVersion = 3

const DefaultMaxRecordBytes = 32 << 20

var knownKinds = map[string]bool{
	"user": true, "assistant": true, "attachment": true, "system": true,
	"queue-operation": true, "mode": true, "permission-mode": true,
	"atis-latch": true, "last-prompt": true, "ai-title": true,
	"cost-state": true, "agent-name": true,
	"started": true, "result": true, "launched": true, "failed": true,
	"api-request": true, "api-request-blob": true, "api-request-shape": true,
	"session-owner": true, "tools-used": true, "pr-link": true,
	"file-history-snapshot": true, "file-history-delta": true,
}

var messageKinds = map[string]bool{
	"user": true, "assistant": true,
}

var toolBlockTypes = map[string]bool{
	"tool_use": true, "server_tool_use": true, "mcp_tool_use": true,
}

var toolResultBlockTypes = map[string]bool{
	"tool_result": true, "mcp_tool_result": true,
}

type Options struct {
	SourceScope string

	MaxRecordBytes int64
}

type Record struct {
	Offset      int64
	End         int64
	Raw         string
	Native      string
	Synthetic   bool
	Kind        string
	Role        string
	ToolName    string
	Text        string
	HasTime     bool
	TimeMS      int64
	Cwd         string
	GitBranch   string
	Model       string
	MessageID   string
	ContentForm string
	IsSidechain bool
	Usage       json.RawMessage
	Metadata    string
}

type Result struct {
	Records []Record
	Cursor  int64
}

type ParseError struct {
	Offset         int64
	Reason         string
	RecordTooLarge bool
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("claudecode: source stops at offset %d: %s", e.Offset, e.Reason)
}

func Parse(data []byte, base int64, o Options) (Result, error) {
	limit := o.MaxRecordBytes
	if limit <= 0 {
		limit = DefaultMaxRecordBytes
	}
	res := Result{Cursor: base}
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
		rec, err := parseRecord(line, offset, o)
		if err != nil {
			return res, err
		}
		rec.End = offset + int64(len(line)) + 1
		res.Records = append(res.Records, rec)
		rel += idx + 1
		res.Cursor = rec.End
	}
	return res, nil
}

func parseRecord(line []byte, offset int64, o Options) (Record, error) {
	if !utf8.Valid(line) {
		return Record{}, &ParseError{Offset: offset, Reason: "record is not valid UTF-8"}
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		if _, ok := err.(*json.UnmarshalTypeError); ok {
			return Record{}, &ParseError{Offset: offset, Reason: "record is not a JSON object"}
		}
		return Record{}, &ParseError{Offset: offset, Reason: "record is not valid JSON"}
	}
	if obj == nil {
		return Record{}, &ParseError{Offset: offset, Reason: "record is not a JSON object"}
	}
	if _, err := dec.Token(); err != io.EOF {
		return Record{}, &ParseError{Offset: offset, Reason: "record has content after the JSON object"}
	}

	rec := Record{Offset: offset, Raw: string(line)}
	md := map[string]any{}

	if sid, ok := stringValue(obj, "sessionId"); ok && sid != "" {
		rec.Native = sid
	} else if o.SourceScope != "" {
		rec.Native = SyntheticNativeIDPrefix + o.SourceScope
		rec.Synthetic = true
	} else {
		return Record{}, &ParseError{
			Offset: offset,
			Reason: "record has no sessionId and Options.SourceScope is empty, so no synthetic native ID can be assigned",
		}
	}

	if t, ok := stringValue(obj, "type"); ok && knownKinds[t] {
		rec.Kind = t
	} else {
		rec.Kind = "unknown"
		if t, ok := stringValue(obj, "type"); ok && t != "" {
			md["observedType"] = t
		}
	}

	if ts, ok := stringValue(obj, "timestamp"); ok {
		if parsed, err := time.Parse(time.RFC3339, ts); err == nil {
			rec.TimeMS = parsed.UnixMilli()
			rec.HasTime = true
		}
	}

	if v, ok := stringValue(obj, "uuid"); ok {
		md["uuid"] = v
	}
	if v, ok := stringValue(obj, "parentUuid"); ok {
		md["parentUuid"] = v
	}
	if v, ok := obj["isSidechain"].(bool); ok && v {
		rec.IsSidechain = true
		md["isSidechain"] = true
	}
	if v, ok := stringValue(obj, "agentId"); ok {
		md["agentId"] = v
	}
	if v, ok := stringValue(obj, "cwd"); ok {
		rec.Cwd = v
	}
	if v, ok := stringValue(obj, "gitBranch"); ok {
		rec.GitBranch = v
	}
	if n, ok := obj["apiBlockIndex"].(json.Number); ok {
		md["apiBlockIndex"] = n
	}

	if msg, ok := obj["message"].(map[string]any); ok && messageKinds[rec.Kind] {
		if r, ok := stringValue(msg, "role"); ok {
			rec.Role = r
		} else {
			rec.Role = rec.Kind
		}
		if v, ok := stringValue(msg, "id"); ok {
			rec.MessageID = v
		}
		if v, ok := stringValue(msg, "model"); ok {
			rec.Model = v
		}
		if u, ok := msg["usage"].(map[string]any); ok {
			if b, err := json.Marshal(u); err == nil {
				rec.Usage = b
			}
		}
		switch content := msg["content"].(type) {
		case string:
			rec.Text = content
			rec.ContentForm = "string"
			md["contentForm"] = "string"
		case []any:
			rec.ContentForm = "array"
			md["contentForm"] = "array"
			text, tool, blocks := normalizeContent(content)
			rec.Text = text
			rec.ToolName = tool
			md["blocks"] = blocks
		}
	}

	if v, ok := obj["toolUseResult"]; ok {
		md["toolUseResult"] = v
	}
	if v, ok := stringValue(obj, "sourceToolAssistantUUID"); ok {
		md["sourceToolAssistantUUID"] = v
	}

	switch rec.Kind {
	case "attachment":
		if a, ok := obj["attachment"].(map[string]any); ok {
			if t, ok := stringValue(a, "type"); ok {
				md["attachmentType"] = t
			}
			if s, ok := stringValue(a, "text"); ok {
				rec.Text = s
			}
		}
	case "system":
		if s, ok := stringValue(obj, "content"); ok {
			rec.Text = s
		}
		if v, ok := stringValue(obj, "subtype"); ok {
			md["subtype"] = v
		}
		if v, ok := stringValue(obj, "level"); ok {
			md["level"] = v
		}
	}

	b, err := json.Marshal(md)
	if err != nil {
		return Record{}, &ParseError{Offset: offset, Reason: "record observations cannot be serialized"}
	}
	rec.Metadata = string(b)
	return rec, nil
}

func normalizeContent(blocks []any) (text string, tool string, blocksMD []any) {
	var parts []string
	blocksMD = []any{}
	toolBlocks := 0
	var names []string
	for _, item := range blocks {
		block, ok := item.(map[string]any)
		if !ok {
			blocksMD = append(blocksMD, map[string]any{"type": ""})
			continue
		}
		t, _ := stringValue(block, "type")
		entry := map[string]any{"type": t}
		switch {
		case t == "text":
			if s, ok := stringValue(block, "text"); ok {
				parts = append(parts, s)
			}
		case t == "thinking":
			if s, ok := stringValue(block, "thinking"); ok {
				parts = append(parts, s)
			}
		case toolBlockTypes[t]:
			toolBlocks++
			name, hasName := stringValue(block, "name")
			if !hasName {
				name, _ = stringValue(block, "tool_name")
			}
			if name != "" {
				names = append(names, name)
				entry["name"] = name
			}
			if id, ok := stringValue(block, "id"); ok {
				entry["id"] = id
			}
			if input, ok := block["input"]; ok {
				entry["input"] = input
				if name != "" {
					parts = append(parts, name+" "+compactJSON(input))
				} else {
					parts = append(parts, compactJSON(input))
				}
			} else if name != "" {
				parts = append(parts, name)
			}
		case toolResultBlockTypes[t]:
			if id, ok := stringValue(block, "tool_use_id"); ok {
				entry["tool_use_id"] = id
			}
			if isError, ok := block["is_error"].(bool); ok {
				entry["is_error"] = isError
			}
			switch content := block["content"].(type) {
			case string:
				parts = append(parts, content)
			case []any:
				for _, sub := range content {
					if m, ok := sub.(map[string]any); ok {
						if st, _ := stringValue(m, "type"); st == "text" {
							if s, ok := stringValue(m, "text"); ok {
								parts = append(parts, s)
							}
						}
					}
				}
			}
		}
		blocksMD = append(blocksMD, entry)
	}
	if toolBlocks == 1 && len(names) == 1 {
		tool = names[0]
	}
	return strings.Join(parts, "\n"), tool, blocksMD
}

func stringValue(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func compactJSON(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return ""
	}
	return strings.TrimSuffix(buf.String(), "\n")
}
