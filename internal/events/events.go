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
	"github.com/nibzard/ohya/internal/pi"
	"github.com/nibzard/ohya/internal/store"
)

// Agents lists the agent names that have a parser, in a stable order.
var Agents = []string{claudecode.AgentName, codex.AgentName, pi.AgentName}

// Supported reports whether the agent has a parser.
func Supported(agent string) bool {
	return agent == claudecode.AgentName || agent == codex.AgentName || agent == pi.AgentName
}

// Options configures one Parse call over one source's bytes.
type Options struct {
	// SourceScope names the source; it scopes synthetic native IDs. Pass
	// the source ID.
	SourceScope string
	// FirstNative is the native session ID of the source's record at
	// offset 0 (store.SourceState.FirstNativeID). Codex and Pi need it to
	// parse bytes past offset 0, because their records do not name their
	// session; Claude Code ignores it.
	FirstNative string
	// MaxRecordBytes bounds one record's JSON text, excluding the line
	// terminator, for every parser. A record above the bound stops its
	// source with RecordTooLarge set on the StopError; a record exactly at
	// the bound passes. Values <= 0 select each parser's default (1 MiB).
	MaxRecordBytes int64
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
	// RecordTooLarge reports that the record size limit caused the stop, so
	// a larger Options.MaxRecordBytes remedies it; every other stop needs
	// the source file repaired. Both remedies are decisions of
	// ADR 0004, and the ingestion diagnostic states the one that applies.
	RecordTooLarge bool
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
		res, err := claudecode.Parse(data, base, claudecode.Options{
			SourceScope: o.SourceScope, MaxRecordBytes: o.MaxRecordBytes})
		out := Parsed{Events: FromClaudeCodeRecords(res.Records), Cursor: res.Cursor}
		var perr *claudecode.ParseError
		if errors.As(err, &perr) {
			return out, &StopError{Offset: perr.Offset, Err: err, RecordTooLarge: perr.RecordTooLarge}
		}
		return out, err
	case codex.AgentName:
		res, err := codex.Parse(data, base, codex.Options{
			SourceScope: o.SourceScope, Native: o.FirstNative, MaxRecordBytes: o.MaxRecordBytes})
		out := Parsed{Events: FromCodexRecords(res.Records), Cursor: res.Cursor}
		var perr *codex.ParseError
		if errors.As(err, &perr) {
			return out, &StopError{Offset: perr.Offset, Err: err, RecordTooLarge: perr.RecordTooLarge}
		}
		return out, err
	case pi.AgentName:
		res, err := pi.Parse(data, base, pi.Options{
			SourceScope: o.SourceScope, Native: o.FirstNative, MaxRecordBytes: o.MaxRecordBytes})
		out := Parsed{Events: FromPiRecords(res.Records), Cursor: res.Cursor}
		var perr *pi.ParseError
		if errors.As(err, &perr) {
			return out, &StopError{Offset: perr.Offset, Err: err, RecordTooLarge: perr.RecordTooLarge}
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
		MessageID: r.MessageID, ParserVersion: claudecode.OutputVersion,
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
// reasoning tokens are part of output_tokens. cache_write_input_tokens is 0
// in every usage record of the verified corpus (checked 2026-11-14 over all
// 22,209 records, versions 0.153.4-0.169.0), so whether it is part of
// input_tokens stays unverified and it maps nowhere.
type codexUsage struct {
	InputTokens       int64 `json:"input_tokens"`
	CachedInputTokens int64 `json:"cached_input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
}

// FromCodex maps one parsed Codex record onto the write path's Event. The
// response_id groups one API response, so a response whose record appears
// in several files of the thread counts once (first copy wins). Usage maps
// onto the Claude Code shape: input without the cached part, the cached
// part as cache reads, and output including reasoning.
// cache_write_input_tokens is 0 in every verified record (fixtures and the
// full real corpus), so its relation to input_tokens stays unverified and
// it stays out of the cache-creation count; the input total (input + cache
// reads) is exact either way. Map it only after a non-zero sample confirms
// the relation.
func FromCodex(r codex.Record) store.Event {
	e := store.Event{
		Offset: r.Offset, End: r.End, NativeID: r.Native,
		Kind: r.Kind, Role: r.Role, ToolName: r.ToolName, Text: r.Text,
		RawJSON: r.Raw, Metadata: r.Metadata,
		HasTime: r.HasTime, TimeMS: r.TimeMS,
		Cwd: r.Cwd, GitBranch: r.GitBranch, Model: r.Model,
		MessageID: r.ResponseID, Prompt: r.Prompt,
		HasOrdinal: r.HasOrdinal, Ordinal: r.Ordinal,
		ParserVersion: codex.OutputVersion,
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

// piUsage holds the token counts of one Pi assistant response (see
// testdata/pi/README.md, "Usage fields"). Every verified record satisfies
// input + cacheRead + cacheWrite + output = totalTokens, so input excludes
// cache reads and cache writes — unlike Codex, the counts map without
// subtraction. cacheWrite is non-zero in 7 of the 524 verified real records
// (anthropic-messages), strictly additive in each. reasoning has no non-zero
// sample (absent from the real corpus, 0 in the captured openai-responses
// records) and cacheWrite1h is 0 in the 3 records carrying it, so their
// subset relations stay unverified and both stay out.
type piUsage struct {
	Input       int64 `json:"input"`
	Output      int64 `json:"output"`
	CacheRead   int64 `json:"cacheRead"`
	CacheWrite  int64 `json:"cacheWrite"`
	Reasoning   int64 `json:"reasoning"`
	TotalTokens int64 `json:"totalTokens"`
}

// FromPi maps one parsed Pi record onto the write path's Event. Usage sits on
// the assistant message, one object per response — no Claude-Code-style
// dedupe across records. The usage-row key is the responseId, or the entry id
// when the provider emits none (openai-completions records never carry a
// responseId, verified over 503 successful responses). A usage whose parts do
// not sum to totalTokens contradicts the verified identity of the fields and
// counts nothing.
func FromPi(r pi.Record) store.Event {
	messageID := r.MessageID
	if messageID == "" {
		messageID = r.EntryID
	}
	e := store.Event{
		Offset: r.Offset, End: r.End, NativeID: r.Native,
		Kind: r.Kind, Role: r.Role, ToolName: r.ToolName, Text: r.Text,
		RawJSON: r.Raw, Metadata: r.Metadata,
		HasTime: r.HasTime, TimeMS: r.TimeMS,
		Cwd: r.Cwd, Model: r.Model,
		MessageID: messageID, Prompt: r.Prompt,
		ParserVersion: pi.OutputVersion,
	}
	if len(r.Usage) > 0 {
		var u piUsage
		if err := json.Unmarshal(r.Usage, &u); err == nil &&
			u.Input >= 0 && u.Output >= 0 && u.CacheRead >= 0 && u.CacheWrite >= 0 &&
			u.Input+u.CacheRead+u.CacheWrite+u.Output == u.TotalTokens {
			e.Usage = store.Usage{
				InputTokens:              u.Input,
				CacheReadInputTokens:     u.CacheRead,
				CacheCreationInputTokens: u.CacheWrite,
				OutputTokens:             u.Output,
			}
			e.HasUsage = true
		}
	}
	return e
}

// FromPiRecords maps parsed records in source order.
func FromPiRecords(records []pi.Record) []store.Event {
	events := make([]store.Event, len(records))
	for i, r := range records {
		events[i] = FromPi(r)
	}
	return events
}
