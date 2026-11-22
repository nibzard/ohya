package antigravity

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const AgentName = "antigravity"

const SyntheticNativeIDPrefix = "ohya-synthetic:"

const OutputVersion = 1

const DefaultMaxRecordBytes = 32 << 20

var ErrNoNative = errors.New("antigravity: no conversation id available from Options.Native, Options.SourcePath, or Options.SourceScope")

var knownTypes = map[string]string{
	"USER_INPUT":       "user_input",
	"PLANNER_RESPONSE": "planner_response",
	"GENERIC":          "generic",
	"SYSTEM_MESSAGE":   "system_message",
	"CHECKPOINT":       "checkpoint",
	"ERROR_MESSAGE":    "error_message",
}

type Options struct {
	SourceScope    string
	SourcePath     string
	Native         string
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
	return fmt.Sprintf("antigravity: source stops at offset %d: %s", e.Offset, e.Reason)
}

func ConversationIDFromPath(sourcePath string) string {
	if sourcePath == "" {
		return ""
	}
	slashed := filepath.ToSlash(sourcePath)
	slashed = path.Clean(slashed)
	for _, suffix := range []string{
		"/.system_generated/logs/transcript_full.jsonl",
		"/.system_generated/logs/transcript.jsonl",
	} {
		if prefix, ok := strings.CutSuffix(slashed, suffix); ok {
			base := path.Base(prefix)
			if base != "" && base != "." && base != "/" && base != ".." {
				return base
			}
		}
	}
	return ""
}

func resolveNative(o Options) (native string, synthetic bool, err error) {
	if o.Native != "" {
		return o.Native, strings.HasPrefix(o.Native, SyntheticNativeIDPrefix), nil
	}
	if id := ConversationIDFromPath(o.SourcePath); id != "" {
		return id, false, nil
	}
	if o.SourceScope != "" {
		return SyntheticNativeIDPrefix + o.SourceScope, true, nil
	}
	return "", false, ErrNoNative
}

func Parse(data []byte, base int64, o Options) (Result, error) {
	limit := o.MaxRecordBytes
	if limit <= 0 {
		limit = DefaultMaxRecordBytes
	}
	res := Result{Cursor: base}
	native, synthetic, nerr := resolveNative(o)
	if nerr != nil && len(data) > 0 {
		return res, nerr
	}
	res.Native = native
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
		rec, err := parseRecord(line, offset, native, synthetic)
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

func parseRecord(line []byte, offset int64, native string, synthetic bool) (Record, error) {
	obj, err := decodeObject(line, offset)
	if err != nil {
		return Record{}, err
	}
	rec := Record{
		Offset:    offset,
		Raw:       string(line),
		Native:    native,
		Synthetic: synthetic,
	}
	md := map[string]any{}

	rawType, _ := stringValue(obj, "type")
	if mapped, ok := knownTypes[rawType]; ok {
		rec.Kind = mapped
	} else {
		rec.Kind = "unknown"
		if rawType != "" {
			md["observedType"] = rawType
		}
	}

	if src, ok := stringValue(obj, "source"); ok && src != "" {
		md["source"] = src
	}
	if st, ok := stringValue(obj, "status"); ok && st != "" {
		md["status"] = st
	}
	if step, ok := int64Value(obj, "step_index"); ok {
		md["stepIndex"] = step
		rec.MessageID = "step-" + strconv.FormatInt(step, 10)
	}

	if ts, ok := stringValue(obj, "created_at"); ok {
		if parsed, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			rec.TimeMS = parsed.UnixMilli()
			rec.HasTime = true
		}
	}

	switch rec.Kind {
	case "user_input":
		rec.Role = "user"
		content, _ := stringValue(obj, "content")
		clean := extractUserRequest(content)
		rec.Text = clean
		rec.Prompt = clean
	case "planner_response":
		rec.Role = "assistant"
		rec.Model, _ = stringValue(obj, "model")
		content, _ := stringValue(obj, "content")
		text, tool, names := normalizePlannerContent(content, obj["tool_calls"])
		rec.Text = text
		rec.ToolName = tool
		if len(names) > 1 {
			md["toolNames"] = names
		}
		if usage := extractUsage(obj); len(usage) > 0 {
			rec.Usage = usage
			if rec.MessageID == "" {
				rec.MessageID = "offset-" + strconv.FormatInt(offset, 10)
			}
		}
	case "generic":
		rec.Role = "tool"
		rec.Text, _ = stringValue(obj, "content")
		if types := extractMediaTypes(obj["media"]); len(types) > 0 {
			md["mediaTypes"] = types
		}
	case "system_message":
		rec.Role = "system"
		if content, ok := stringValue(obj, "content"); ok {
			rec.Cwd = extractWorkingDirectory(content)
		}
	case "checkpoint":
		rec.Role = "system"
		rec.Text, _ = stringValue(obj, "content")
		if usage := extractUsage(obj); len(usage) > 0 {
			rec.Usage = usage
			if rec.MessageID == "" {
				rec.MessageID = "offset-" + strconv.FormatInt(offset, 10)
			}
		}
	case "error_message":
		rec.Role = "system"
		content, _ := stringValue(obj, "content")
		errMsg, _ := stringValue(obj, "error")
		if errMsg != "" {
			md["error"] = errMsg
		}
		if content != "" {
			rec.Text = content
		} else {
			rec.Text = errMsg
		}
	}

	b, err := json.Marshal(md)
	if err != nil {
		return Record{}, &ParseError{Offset: offset, Reason: "record observations cannot be serialized"}
	}
	rec.Metadata = string(b)
	return rec, nil
}

func extractUserRequest(content string) string {
	const openTag = "<USER_REQUEST>"
	const closeTag = "</USER_REQUEST>"
	start := strings.Index(content, openTag)
	if start >= 0 {
		rest := content[start+len(openTag):]
		if end := strings.Index(rest, closeTag); end >= 0 {
			return strings.TrimSpace(rest[:end])
		}
	}
	return strings.TrimSpace(content)
}

func extractWorkingDirectory(content string) string {
	const prefix = "Command Working Directory:"
	for _, line := range strings.Split(content, "\n") {
		if after, ok := strings.CutPrefix(strings.TrimSpace(line), prefix); ok {
			dir := strings.TrimSpace(after)
			if strings.HasPrefix(dir, "/") {
				return dir
			}
		}
	}
	return ""
}

func normalizePlannerContent(content string, rawCalls any) (text string, tool string, names []string) {
	var parts []string
	if trimmed := strings.TrimSpace(content); trimmed != "" {
		parts = append(parts, content)
	}
	calls, _ := rawCalls.([]any)
	for _, item := range calls {
		call, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := stringValue(call, "name")
		if name != "" {
			names = append(names, name)
		}
		if args, ok := call["args"]; ok && args != nil {
			if name != "" {
				parts = append(parts, name+" "+compactJSON(args))
			} else {
				parts = append(parts, compactJSON(args))
			}
		} else if name != "" {
			parts = append(parts, name)
		}
	}
	if len(names) == 1 {
		tool = names[0]
	}
	return strings.Join(parts, "\n"), tool, names
}

func extractUsage(obj map[string]any) json.RawMessage {
	inTok, hasIn := int64Value(obj, "input_tokens")
	cacheTok, hasCache := int64Value(obj, "cache_read_tokens")
	outTok, hasOut := int64Value(obj, "output_tokens")
	if !hasIn && !hasCache && !hasOut {
		return nil
	}
	if inTok < 0 || cacheTok < 0 || outTok < 0 {
		return nil
	}
	b, err := json.Marshal(struct {
		InputTokens     int64 `json:"input_tokens"`
		CacheReadTokens int64 `json:"cache_read_tokens"`
		OutputTokens    int64 `json:"output_tokens"`
	}{
		InputTokens:     inTok,
		CacheReadTokens: cacheTok,
		OutputTokens:    outTok,
	})
	if err != nil {
		return nil
	}
	return b
}

func extractMediaTypes(raw any) []string {
	items, ok := raw.([]any)
	if !ok || len(items) == 0 {
		return nil
	}
	var types []string
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if mime, ok := stringValue(m, "mime_type"); ok && mime != "" {
			types = append(types, mime)
		}
	}
	return types
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

func stringValue(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func int64Value(m map[string]any, key string) (int64, bool) {
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	n, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	i, err := n.Int64()
	if err != nil {
		return 0, false
	}
	return i, true
}
