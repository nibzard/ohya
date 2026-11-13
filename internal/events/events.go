// Package events maps parsed agent records onto the write path's
// store.Event. It holds the production adapters shared by local ingestion
// (cmd/ohya) and hub-side batch normalization (internal/hubsync), so every
// path into the database normalizes the same record identically. One mapping
// exists per agent parser, and Parse dispatches on the agent name;
// internal/store/ingest_test.go keeps a mirrored copy of the Claude Code
// mapping because an in-package test cannot import this package without an
// import cycle — change them together.
package events

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/nibzard/ohya/internal/claudecode"
	"github.com/nibzard/ohya/internal/codex"
	"github.com/nibzard/ohya/internal/store"
)

// Agents lists the agent names that have a parser, in a stable order.
var Agents = []string{claudecode.AgentName, codex.AgentName}

// Supported reports whether the agent has a parser.
func Supported(agent string) bool {
	return agent == claudecode.AgentName || agent == codex.AgentName
}

// Options configures one Parse call over one source's bytes.
type Options struct {
	// SourceScope names the source; it scopes synthetic native IDs. Pass
	// the source ID.
	SourceScope string
	// FirstNative is the native session ID of the source's record at
	// offset 0 (store.SourceState.FirstNativeID). Codex needs it to parse
	// bytes past offset 0, because its records do not name their session;
	// Claude Code ignores it.
	FirstNative string
}

// Parsed is the outcome of parsing one buffer of source bytes.
type Parsed struct {
	Events []store.Event
	// Cursor is the offset just past the last accepted record; bytes from
	// Cursor on are the incomplete tail or the stop.
	Cursor int64
}

// StopError reports the offset at which a source must stop: the records
// before it commit, and nothing past it is skipped. Err is the parser's own
// diagnostic and never contains transcript content.
type StopError struct {
	Offset int64
	Err    error
}

func (e *StopError) Error() string { return e.Err.Error() }
func (e *StopError) Unwrap() error { return e.Err }

// Parse parses data, which starts at absolute offset base of a source of the
// named agent, and maps the records onto events in source order. A parser
// stop is a *StopError, returned with the records before it; any other error
// is a caller error (an unknown agent, or a missing FirstNative).
func Parse(agent string, data []byte, base int64, o Options) (Parsed, error) {
	switch agent {
	case claudecode.AgentName:
		res, err := claudecode.Parse(data, base, claudecode.Options{SourceScope: o.SourceScope})
		out := Parsed{Events: FromClaudeCodeRecords(res.Records), Cursor: res.Cursor}
		var perr *claudecode.ParseError
		if errors.As(err, &perr) {
			return out, &StopError{Offset: perr.Offset, Err: err}
		}
		return out, err
	case codex.AgentName:
		res, err := codex.Parse(data, base, codex.Options{SourceScope: o.SourceScope, Native: o.FirstNative})
		out := Parsed{Events: FromCodexRecords(res.Records), Cursor: res.Cursor}
		var perr *codex.ParseError
		if errors.As(err, &perr) {
			return out, &StopError{Offset: perr.Offset, Err: err}
		}
		return out, err
	default:
		return Parsed{}, fmt.Errorf("events: agent %q has no parser", agent)
	}
}

// usageFields are the unambiguous token counts of one API request, under the
// field names Claude Code uses (see testdata/claude-code/README.md, "Usage
// fields"). Every other usage field has no defined sum and stays out.
type usageFields struct {
	InputTokens              int64 `json:"input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
}

// FromClaudeCode maps one parsed Claude Code record onto the write path's
// Event. It knows the native field names, which user records are fresh
// prompts (string content, observed as contentForm "string") and where the
// per-request usage lives.
func FromClaudeCode(r claudecode.Record) store.Event {
	e := store.Event{
		Offset: r.Offset, End: r.End, NativeID: r.Native,
		Kind: r.Kind, Role: r.Role, ToolName: r.ToolName, Text: r.Text,
		RawJSON: r.Raw, Metadata: r.Metadata,
		HasTime: r.HasTime, TimeMS: r.TimeMS,
		Cwd: r.Cwd, GitBranch: r.GitBranch, Model: r.Model,
		MessageID: r.MessageID,
	}
	var observed struct {
		ContentForm string `json:"contentForm"`
	}
	// Metadata always comes from the parser's own encoder, so it decodes.
	_ = json.Unmarshal([]byte(r.Metadata), &observed)
	if r.Kind == "user" && observed.ContentForm == "string" {
		e.Prompt = r.Text
	}
	if len(r.Usage) > 0 {
		var u usageFields
		if err := json.Unmarshal(r.Usage, &u); err == nil {
			e.Usage = store.Usage{
				InputTokens:              u.InputTokens,
				CacheCreationInputTokens: u.CacheCreationInputTokens,
				CacheReadInputTokens:     u.CacheReadInputTokens,
				OutputTokens:             u.OutputTokens,
			}
			e.HasUsage = true
		}
	}
	return e
}

// FromClaudeCodeRecords maps parsed records in source order.
func FromClaudeCodeRecords(records []claudecode.Record) []store.Event {
	events := make([]store.Event, len(records))
	for i, r := range records {
		events[i] = FromClaudeCode(r)
	}
	return events
}

// codexUsage holds the token counts of one Codex response (see
// testdata/codex/README.md, "Usage fields"). total_tokens = input_tokens +
// output_tokens, so cached_input_tokens is part of input_tokens, and
// reasoning tokens are part of output_tokens.
type codexUsage struct {
	InputTokens       int64 `json:"input_tokens"`
	CachedInputTokens int64 `json:"cached_input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
}

// FromCodex maps one parsed Codex record onto the write path's Event. The
// response_id groups one API response, so a token_usage_record replayed by
// a resumed page counts once. Usage maps onto the Claude Code shape: input
// without the cached part, the cached part as cache reads, and output
// including reasoning. cache_write_input_tokens has no verified relation to
// input_tokens yet, so it stays out of the cache-creation count; the input
// total (input + cache reads) is exact either way.
func FromCodex(r codex.Record) store.Event {
	e := store.Event{
		Offset: r.Offset, End: r.End, NativeID: r.Native,
		Kind: r.Kind, Role: r.Role, ToolName: r.ToolName, Text: r.Text,
		RawJSON: r.Raw, Metadata: r.Metadata,
		HasTime: r.HasTime, TimeMS: r.TimeMS,
		Cwd: r.Cwd, GitBranch: r.GitBranch, Model: r.Model,
		MessageID: r.ResponseID, Prompt: r.Prompt,
		HasOrdinal: r.HasOrdinal, Ordinal: r.Ordinal,
	}
	if len(r.Usage) > 0 {
		var u codexUsage
		// Counts that contradict each other are ambiguous and count nothing.
		if err := json.Unmarshal(r.Usage, &u); err == nil &&
			u.CachedInputTokens >= 0 && u.OutputTokens >= 0 && u.CachedInputTokens <= u.InputTokens {
			e.Usage = store.Usage{
				InputTokens:          u.InputTokens - u.CachedInputTokens,
				CacheReadInputTokens: u.CachedInputTokens,
				OutputTokens:         u.OutputTokens,
			}
			e.HasUsage = true
		}
	}
	return e
}

// FromCodexRecords maps parsed records in source order.
func FromCodexRecords(records []codex.Record) []store.Event {
	events := make([]store.Event, len(records))
	for i, r := range records {
		events[i] = FromCodex(r)
	}
	return events
}
