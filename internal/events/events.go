// Package events maps parsed agent records onto the write path's
// store.Event. It holds the production adapters shared by local ingestion
// (cmd/ohya) and hub-side batch normalization (internal/hubsync), so every
// path into the database normalizes the same record identically. One mapping
// exists per agent parser; internal/store/ingest_test.go keeps a mirrored
// copy because an in-package test cannot import this package without an
// import cycle — change them together.
package events

import (
	"encoding/json"

	"github.com/nibzard/ohya/internal/claudecode"
	"github.com/nibzard/ohya/internal/store"
)

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
	e.UserPrompt = r.Kind == "user" && observed.ContentForm == "string"
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
