// Package pi implements the deterministic parser for Pi agent session files
// (verified against Pi 0.84.2 and 1.5.2; see testdata/pi/README.md).
//
// Parsing is pure: the same bytes and options always produce the same
// records, with no clock, network, or model use. The original JSON text of
// each record is preserved verbatim; normalized fields are additive.
//
// Like Codex rollout files, a Pi session file names its session exactly once:
// the `session` header at offset 0 carries the id. Every later entry of the
// source belongs to it; a parse that starts later receives it through
// Options.Native.
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

// AgentName identifies the agent for this source format. It is one input of
// the session identity (device, agent, native ID).
const AgentName = "pi"

// SyntheticNativeIDPrefix opens the reserved namespace for source-scoped
// synthetic native session IDs. A source whose first record is not a session
// header with an id gets this prefix plus the source scope, so such records
// never join another source's session. Pi session ids are UUIDs and contain
// no colon, so they never collide with a synthetic ID.
const SyntheticNativeIDPrefix = "ohya-synthetic:"

// OutputVersion versions this parser's normalized output — what the Record
// fields hold for a given raw record — not the file format and not the
// database schema. Any change to what Parse derives from the same bytes must
// bump it; ingestion stores it per event row (events.parser_version), and
// store.ReDerive rebuilds rows carrying a smaller version from raw_json.
// 1 is the output of the parser as of the marker's introduction. 2 adds
// Record.EntryID, which the events adapter uses as the usage-row key when a
// provider emits no responseId (verified 2026-11-14: openai-completions
// records never carry one).
const OutputVersion = 2

// DefaultMaxRecordBytes bounds one record's JSON text, excluding the line
// terminator, when Options.MaxRecordBytes is unset. A 1.5.2 system entry
// embeds the whole harness preamble and skill list, so the bound matches the
// sync protocol's per-record limit.
const DefaultMaxRecordBytes = 1 << 20

// ErrNoNative reports a parse that starts after offset 0 without the native
// session ID the source's session header established. It is a caller error,
// not a property of the source bytes, so it is not a ParseError.
var ErrNoNative = errors.New("pi: parse after offset 0 needs Options.Native from the source's session header")

// knownKinds lists the entry types observed in Pi 0.84.2 and 1.5.2. Any other
// type is valid but stored with kind "unknown"; the parser never rejects a
// record for its type.
var knownKinds = map[string]bool{
	"session": true, "model_change": true, "thinking_level_change": true,
	"message": true,
}

// Options configures one Parse call over one source's bytes.
type Options struct {
	// SourceScope names the source (file incarnation) the bytes come from.
	// It scopes the synthetic native ID of a source whose header names no
	// session. The same source must always pass the same scope.
	SourceScope string

	// Native is the native session ID that the source's session header at
	// offset 0 established (Result.Native of the parse that covered offset
	// 0). It is required when base > 0 and ignored when base == 0, because
	// the bytes then carry the header themselves.
	Native string

	// MaxRecordBytes bounds one record's JSON text, excluding the line
	// terminator. A complete or still-growing record above the bound stops
	// the source. Values <= 0 select DefaultMaxRecordBytes.
	MaxRecordBytes int64
}

// Record is one complete JSONL record with deterministic normalized fields.
// Empty fields never shadow the original: Raw always holds the exact source
// text.
type Record struct {
	// Offset is the record's first byte offset in the source file.
	Offset int64
	// End is the byte offset just past the record's line terminator; it is
	// the cursor position after accepting this record.
	End    int64
	Raw    string // original JSON text, without the line terminator
	Native string // session id from the source's session header, or the synthetic ID
	// Synthetic reports that Native is a source-scoped synthetic ID.
	Synthetic bool
	Kind      string // entry type, or "unknown"
	Role      string // message role; "" when the record has none
	ToolName  string // sole invoked tool; "" when absent or ambiguous
	Text      string // searchable text, concatenated in source order
	// Prompt is the user's prompt as typed, from a user message. Pi
	// persists only typed input as user messages — the harness preamble is
	// a separate system entry — so every user message marks a prompt.
	Prompt string
	// HasTime reports that the record carried a parseable entry timestamp;
	// only then is TimeMS meaningful.
	HasTime bool
	TimeMS  int64  // Unix milliseconds from the entry's timestamp field
	Cwd     string // working directory observation (session header)
	Model   string // model observation (model_change, assistant messages)
	// EntryID is the entry's own 8-hex id (every entry except the session
	// header carries one; a file never repeats one). The session header
	// names no entry, so its id is the session id and stays out.
	EntryID string
	// MessageID is the assistant responseId: one API response, one usage
	// object. It groups nothing in verified files, so usage sums directly.
	// Only some APIs emit it: openai-completions records never do, and 4 of
	// 7 verified anthropic-messages records lack it, so the events adapter
	// falls back to EntryID as the usage-row key.
	MessageID string
	Usage     json.RawMessage
	Metadata  string // compact JSON object of deterministic observations
}

// Result is the outcome of parsing one buffer of source bytes.
type Result struct {
	Records []Record
	// Cursor is the absolute byte offset just past the last accepted
	// record's line terminator. Bytes from Cursor on are the incomplete
	// tail: Parse never consumes them, and the caller retains them by
	// reading from Cursor on the next poll.
	Cursor int64
	// Native is the source's native session ID: Options.Native when base >
	// 0, or the id the session header at offset 0 established. It is ""
	// when a parse from offset 0 accepted no record.
	Native string
}

// ParseError reports the offset at which a source must stop. Reason never
// contains transcript content.
type ParseError struct {
	Offset int64
	Reason string
	// RecordTooLarge reports that the record size limit caused this stop, so
	// a larger Options.MaxRecordBytes remedies it; every other stop needs
	// the source file repaired.
	RecordTooLarge bool
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("pi: source stops at offset %d: %s", e.Offset, e.Reason)
}

// Parse scans data, which must start at absolute file offset base, and
// returns every complete newline-terminated record in order. Records are
// returned even when a later line fails: the caller commits them, advances
// the cursor, and surfaces the diagnostic. The incomplete tail is left for
// the next poll.
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
			// Incomplete tail: an append-only source can only add bytes,
			// so a tail already above the limit can never become valid.
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

// decodeObject validates one line as exactly one UTF-8 JSON object.
func decodeObject(line []byte, offset int64) (map[string]any, error) {
	// SQLite stores TEXT; a line that is not UTF-8 cannot round-trip.
	if !utf8.Valid(line) {
		return nil, &ParseError{Offset: offset, Reason: "record is not valid UTF-8"}
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber() // metadata keeps numbers exactly as written
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		if _, ok := err.(*json.UnmarshalTypeError); ok {
			return nil, &ParseError{Offset: offset, Reason: "record is not a JSON object"}
		}
		// The error text can quote input characters; keep it out of the
		// diagnostic.
		return nil, &ParseError{Offset: offset, Reason: "record is not valid JSON"}
	}
	if obj == nil { // JSON null decodes to a nil map without an error
		return nil, &ParseError{Offset: offset, Reason: "record is not a JSON object"}
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, &ParseError{Offset: offset, Reason: "record has content after the JSON object"}
	}
	return obj, nil
}

// parseRecord validates one line and derives its normalized fields. native
// is the source's established session; it is "" only for the record at
// offset 0, which establishes it.
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

	// Every entry except the session header carries its own id; the header's
	// id names the session instead (handled below).
	if rec.Kind != "session" {
		rec.EntryID, _ = stringValue(obj, "id")
	}

	// Native session identity: the session header at offset 0 names the
	// session, and every later record of the source belongs to it. Never
	// guess or merge. A later session header (never observed) keeps the
	// established identity.
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

	// The entry timestamp (ISO 8601) sets the time; the message-level Unix
	// milliseconds mirror it and stay out.
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

	b, err := json.Marshal(md) // map keys sort, so output is deterministic
	if err != nil {
		return Record{}, &ParseError{Offset: offset, Reason: "record observations cannot be serialized"}
	}
	rec.Metadata = string(b)
	return rec, nil
}

// normalizeMessage derives one message entry's role, text, prompt, tool and
// usage observations. The payload sits under the message key; its shape per
// role is documented in testdata/pi/README.md.
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
		// The harness preamble (sections, toolsAdded): identical across a
		// session's runs and not conversation, so it contributes no search
		// text. Its shape lands in metadata for inspection.
		if sections, ok := msg["sections"].(map[string]any); ok && len(sections) > 0 {
			keys := make([]string, 0, len(sections))
			for k := range sections {
				keys = append(keys, k)
			}
			sort.Strings(keys) // map iteration order never reaches metadata
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

// normalizeAssistantContent derives the searchable text and the unambiguous
// tool name from an assistant content array. A text block contributes its
// text (textSignature is a signature, not content); a toolCall block
// contributes its name plus the compact sorted-key JSON of its arguments,
// like the Claude Code tool_use convention.
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

// blockText concatenates the text of a content array in source order. Only
// blocks with a string text field contribute; images and other block types
// contribute nothing.
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

// compactJSON serializes a decoded JSON value with sorted map keys.
func compactJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "" // unreachable for values decoded from JSON
	}
	return string(b)
}

// stringValue returns the field only when it is present and a string; a
// non-string value reads as absent. A nil map reads as empty.
func stringValue(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}
