// Package claudecode implements the deterministic parser for Claude Code
// session JSONL files (verified against Claude Code 2.1.280; see
// testdata/claude-code/README.md).
//
// Parsing is pure: the same bytes and options always produce the same
// records, with no clock, network, or model use. The original JSON text of
// each record is preserved verbatim; normalized fields are additive.
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

// AgentName identifies the agent for this source format. It is one input of
// the session identity (device, agent, native ID).
const AgentName = "claude-code"

// SyntheticNativeIDPrefix opens the reserved namespace for source-scoped
// synthetic native session IDs. A record without a usable sessionId gets
// this prefix plus the source scope, so records from different sources never
// share a session and real UUID session IDs can never collide with a
// synthetic one (UUIDs contain no colon).
const SyntheticNativeIDPrefix = "ohya-synthetic:"

// OutputVersion versions this parser's normalized output — what the Record
// fields hold for a given raw record — not the file format and not the
// database schema. Any change to what Parse derives from the same bytes must
// bump it; ingestion stores it per event row (events.parser_version), and
// store.ReDerive rebuilds rows carrying a smaller version from raw_json.
// 1 is the output of the parser as of the marker's introduction.
const OutputVersion = 1

// DefaultMaxRecordBytes bounds one record's JSON text, excluding the line
// terminator, when Options.MaxRecordBytes is unset.
const DefaultMaxRecordBytes = 1 << 20

// knownKinds lists the record types with dedicated normalization, as
// observed in Claude Code 2.1.280. Any other type is valid but stored with
// kind "unknown"; the parser never rejects a record for its type.
var knownKinds = map[string]bool{
	// Message records.
	"user": true, "assistant": true, "attachment": true, "system": true,
	// Bookkeeping records.
	"queue-operation": true, "mode": true, "permission-mode": true,
	"atis-latch": true, "last-prompt": true, "ai-title": true,
	"cost-state": true, "agent-name": true,
	// Subagent bookkeeping records.
	"started": true, "result": true, "launched": true, "failed": true,
}

// toolBlockTypes are content blocks that invoke a tool.
var toolBlockTypes = map[string]bool{
	"tool_use": true, "server_tool_use": true, "mcp_tool_use": true,
}

// toolResultBlockTypes are content blocks that carry a tool's result.
var toolResultBlockTypes = map[string]bool{
	"tool_result": true, "mcp_tool_result": true,
}

// Options configures one Parse call over one source's bytes.
type Options struct {
	// SourceScope names the source (file incarnation) the bytes come from.
	// It scopes synthetic native IDs so that sessions are never merged or
	// guessed across sources. The same source must always pass the same
	// scope.
	SourceScope string

	// MaxRecordBytes bounds one record's JSON text, excluding the line
	// terminator. A complete or still-growing record above the bound stops
	// the source. Values <= 0 select DefaultMaxRecordBytes.
	MaxRecordBytes int64
}

// Record is one complete JSONL record with deterministic normalized fields.
// Empty Role, ToolName, Text, and Metadata never shadow the original: Raw
// always holds the exact source text.
type Record struct {
	// Offset is the record's first byte offset in the source file.
	Offset int64
	// End is the byte offset just past the record's line terminator; it is
	// the cursor position after accepting this record.
	End    int64
	Raw    string // original JSON text, without the line terminator
	Native string // native session ID (sessionId), or the synthetic ID
	// Synthetic reports that Native is a source-scoped synthetic ID from
	// the reserved namespace because the record carried no sessionId.
	Synthetic bool
	Kind      string // known type, or "unknown"
	Role      string // message role; "" when the record has none
	ToolName  string // sole invoked tool; "" when absent or ambiguous
	Text      string // searchable text, concatenated in source order
	// HasTime reports that the record carried a parseable timestamp; only
	// then is TimeMS meaningful.
	HasTime   bool
	TimeMS    int64  // Unix milliseconds from the timestamp field
	Cwd       string // working directory observation
	GitBranch string // branch observation
	Model     string // model observation from message.model
	MessageID string // message.id; groups the records of one API response
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
	return fmt.Sprintf("claudecode: source stops at offset %d: %s", e.Offset, e.Reason)
}

// Parse scans data, which must start at absolute file offset base, and
// returns every complete newline-terminated record in order. Records are
// returned even when a later line fails: the caller commits them, advances
// the cursor, and surfaces the diagnostic. The incomplete tail is left for
// the next poll. Use one Options value per source for stable synthetic IDs.
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

// parseRecord validates one line and derives its normalized fields.
func parseRecord(line []byte, offset int64, o Options) (Record, error) {
	// SQLite stores TEXT; a line that is not UTF-8 cannot round-trip.
	if !utf8.Valid(line) {
		return Record{}, &ParseError{Offset: offset, Reason: "record is not valid UTF-8"}
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber() // metadata keeps numbers exactly as written
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		if _, ok := err.(*json.UnmarshalTypeError); ok {
			return Record{}, &ParseError{Offset: offset, Reason: "record is not a JSON object"}
		}
		// The error text can quote input characters; keep it out of the
		// diagnostic.
		return Record{}, &ParseError{Offset: offset, Reason: "record is not valid JSON"}
	}
	if obj == nil { // JSON null decodes to a nil map without an error
		return Record{}, &ParseError{Offset: offset, Reason: "record is not a JSON object"}
	}
	if _, err := dec.Token(); err != io.EOF {
		return Record{}, &ParseError{Offset: offset, Reason: "record has content after the JSON object"}
	}

	rec := Record{Offset: offset, Raw: string(line)}
	md := map[string]any{}

	// Native session identity: sessionId when present, otherwise a
	// source-scoped synthetic ID. Never guess or merge.
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

	// Uniform observations for every record kind.
	if v, ok := stringValue(obj, "uuid"); ok {
		md["uuid"] = v
	}
	if v, ok := stringValue(obj, "parentUuid"); ok {
		md["parentUuid"] = v
	}
	if v, ok := obj["isSidechain"].(bool); ok && v {
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

	if msg, ok := obj["message"].(map[string]any); ok {
		if r, ok := stringValue(msg, "role"); ok {
			rec.Role = r
		} else if rec.Kind == "user" || rec.Kind == "assistant" {
			// The type of a message record coincides with its role.
			rec.Role = rec.Kind
		}
		if v, ok := stringValue(msg, "id"); ok {
			rec.MessageID = v
		}
		if v, ok := stringValue(msg, "model"); ok {
			rec.Model = v
		}
		if u, ok := msg["usage"]; ok {
			if b, err := json.Marshal(u); err == nil {
				rec.Usage = b
			}
		}
		switch content := msg["content"].(type) {
		case string:
			rec.Text = content
			md["contentForm"] = "string"
		case []any:
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

	b, err := json.Marshal(md) // map keys sort, so output is deterministic
	if err != nil {
		return Record{}, &ParseError{Offset: offset, Reason: "record observations cannot be serialized"}
	}
	rec.Metadata = string(b)
	return rec, nil
}

// normalizeContent derives the searchable text, the unambiguous tool name,
// and the per-block metadata from a message content array, all in source
// order.
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

// stringValue returns the field only when it is present and a string; a
// non-string value reads as absent.
func stringValue(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// compactJSON serializes a decoded JSON value with sorted map keys.
func compactJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "" // unreachable for values decoded from JSON
	}
	return string(b)
}
