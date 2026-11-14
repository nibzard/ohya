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

var Agents = []string{claudecode.AgentName, codex.AgentName, pi.AgentName}

func Supported(agent string) bool {
	return agent == claudecode.AgentName || agent == codex.AgentName || agent == pi.AgentName
}

type Options struct {
	SourceScope    string
	FirstNative    string
	MaxRecordBytes int64
}

type Parsed struct {
	Events []store.Event
	Cursor int64
}

type StopError struct {
	Offset         int64
	Err            error
	RecordTooLarge bool
}

func (e *StopError) Error() string { return e.Err.Error() }
func (e *StopError) Unwrap() error { return e.Err }

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

type usageFields struct {
	InputTokens              int64 `json:"input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
}

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

func FromClaudeCodeRecords(records []claudecode.Record) []store.Event {
	events := make([]store.Event, len(records))
	for i, r := range records {
		events[i] = FromClaudeCode(r)
	}
	return events
}

type codexUsage struct {
	InputTokens       int64 `json:"input_tokens"`
	CachedInputTokens int64 `json:"cached_input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
}

func FromCodex(r codex.Record) store.Event {
	e := store.Event{
		Offset: r.Offset, End: r.End, NativeID: r.Native,
		Kind: r.Kind, Role: r.Role, ToolName: r.ToolName, Text: r.Text,
		RawJSON: r.Raw, Metadata: r.Metadata,
		HasTime: r.HasTime, TimeMS: r.TimeMS,
		Cwd: r.Cwd, GitBranch: r.GitBranch, Repo: r.Repo, Model: r.Model,
		MessageID: r.ResponseID, Prompt: r.Prompt,
		HasOrdinal: r.HasOrdinal, Ordinal: r.Ordinal,
		ParserVersion: codex.OutputVersion,
	}
	if len(r.Usage) > 0 {
		var u codexUsage
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

func FromCodexRecords(records []codex.Record) []store.Event {
	events := make([]store.Event, len(records))
	for i, r := range records {
		events[i] = FromCodex(r)
	}
	return events
}

type piUsage struct {
	Input       int64 `json:"input"`
	Output      int64 `json:"output"`
	CacheRead   int64 `json:"cacheRead"`
	CacheWrite  int64 `json:"cacheWrite"`
	Reasoning   int64 `json:"reasoning"`
	TotalTokens int64 `json:"totalTokens"`
}

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

func FromPiRecords(records []pi.Record) []store.Event {
	events := make([]store.Event, len(records))
	for i, r := range records {
		events[i] = FromPi(r)
	}
	return events
}
