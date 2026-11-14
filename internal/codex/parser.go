// Package codex implements the deterministic parser for OpenAI Codex CLI
// rollout files (verified against codex-cli 0.166.0; see
// testdata/codex/README.md).
//
// Parsing is pure: the same bytes and options always produce the same
// records, with no clock, network, or model use. The original JSON text of
// each record is preserved verbatim; normalized fields are additive.
//
// Unlike Claude Code records, Codex records carry no session ID. The thread
// is named once, by the session_meta record that opens the rollout file. The
// record at offset 0 therefore establishes the native session ID of every
// record in the source; a parse that starts later receives it through
// Options.Native.
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
)

// AgentName identifies the agent for this source format. It is one input of
// the session identity (device, agent, native ID).
const AgentName = "codex"

// SyntheticNativeIDPrefix opens the reserved namespace for source-scoped
// synthetic native session IDs. A source whose first record is not a
// session_meta with a thread id gets this prefix plus the source scope, so
// such records never join another source's session. Codex thread ids are
// UUIDs and contain no colon, so they never collide with a synthetic ID.
const SyntheticNativeIDPrefix = "ohya-synthetic:"

// OutputVersion versions this parser's normalized output — what the Record
// fields hold for a given raw record — not the file format and not the
// database schema. Any change to what Parse derives from the same bytes must
// bump it; ingestion stores it per event row (events.parser_version), and
// store.ReDerive rebuilds rows carrying a smaller version from raw_json.
// 1 is the output of the parser as of the marker's introduction.
const OutputVersion = 1

// DefaultMaxRecordBytes bounds one record's JSON text, excluding the line
// terminator, when Options.MaxRecordBytes is unset. Codex world_state and
// compacted records are large, so the bound matches the sync protocol's
// per-record limit.
const DefaultMaxRecordBytes = 1 << 20

// ErrNoNative reports a parse that starts after offset 0 without the native
// session ID the source's first record established. It is a caller error,
// not a property of the source bytes, so it is not a ParseError.
var ErrNoNative = errors.New("codex: parse after offset 0 needs Options.Native from the source's first record")

// knownKinds lists the top-level record types observed in codex-cli 0.166.0.
// Any other type is valid but stored with kind "unknown"; the parser never
// rejects a record for its type.
var knownKinds = map[string]bool{
	"session_meta": true, "response_item": true, "event_msg": true,
	"turn_context": true, "token_usage_record": true, "world_state": true,
	"compacted": true, "inter_agent_communication_metadata": true,
}

// Options configures one Parse call over one source's bytes.
type Options struct {
	// SourceScope names the source (file incarnation) the bytes come from.
	// It scopes the synthetic native ID of a source without a thread id.
	// The same source must always pass the same scope.
	SourceScope string

	// Native is the native session ID that the source's record at offset 0
	// established (Result.Native of the parse that covered offset 0). It is
	// required when base > 0 and ignored when base == 0, because the bytes
	// then carry the first record themselves.
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
	Native string // thread id from the source's first session_meta, or the synthetic ID
	// Synthetic reports that Native is a source-scoped synthetic ID.
	Synthetic bool
	Kind      string // top-level type, or "unknown"
	Role      string // message role; "" when the record has none
	ToolName  string // invoked tool; "" when absent
	Text      string // searchable text, concatenated in source order
	// Prompt is the user's prompt as typed, from an event_msg user_message.
	// The response_item user message carries the same text for search, mixed
	// with injected context messages; Prompt marks the human prompt alone.
	Prompt string
	// HasTime reports that the record carried a parseable timestamp; only
	// then is TimeMS meaningful.
	HasTime bool
	TimeMS  int64 // Unix milliseconds from the timestamp field
	// HasOrdinal reports that the record carries a per-thread ordinal that
	// names it within the thread across rollout files: a resumed page's
	// records continue the original file's ordinal sequence. session_meta
	// records never have one, because a page's opening meta carries the
	// ordinal one below its first record — an ordinal the original file
	// already owns.
	HasOrdinal bool
	Ordinal    int64
	Cwd        string // working directory observation
	GitBranch  string // branch observation
	Model      string // model observation from turn_context
	ResponseID string // token_usage_record response_id; groups one API response
	Usage      json.RawMessage
	Metadata   string // compact JSON object of deterministic observations
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
	// 0, or the ID the record at offset 0 established. It is "" when a parse
	// from offset 0 accepted no record.
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
	return fmt.Sprintf("codex: source stops at offset %d: %s", e.Offset, e.Reason)
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
// is the source's established thread; it is "" only for the record at
// offset 0, which establishes it.
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

	// Native session identity: the first record names the thread, and every
	// later record of the source belongs to it. Never guess or merge.
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
		// UI events mirror the transcript; response_item is the searchable
		// stream, so event_msg contributes no text. The user_message event
		// alone marks the human prompt.
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

	b, err := json.Marshal(md) // map keys sort, so output is deterministic
	if err != nil {
		return Record{}, &ParseError{Offset: offset, Reason: "record observations cannot be serialized"}
	}
	rec.Metadata = string(b)
	return rec, nil
}

// normalizeSessionMeta records the thread's observations. A session_meta
// naming another thread is embedded context (a subagent rollout repeats its
// parent's meta as line 2): its cwd and branch describe the other thread and
// stay out of this session's observations.
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

// normalizeResponseItem derives the searchable text, role and tool of one
// model transcript item.
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

// blockText concatenates the text of a content array in source order. Every
// block with a string "text" field contributes (input_text, output_text,
// summary_text, reasoning_text); other blocks, such as images, contribute
// nothing.
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

// joinText joins non-empty parts with a newline.
func joinText(parts ...string) string {
	return joinNonEmpty("\n", parts)
}

// joinWords joins non-empty parts with a space.
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
