package main

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

// eventFromRecord maps one parsed Claude Code record onto the write path's
// Event. This is the production adapter of the local slice: it knows the
// native field names, which user records are fresh prompts (string content,
// observed as contentForm "string") and where the per-request usage lives.
func eventFromRecord(r claudecode.Record) store.Event {
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

// eventsFromRecords maps parsed records in source order.
func eventsFromRecords(records []claudecode.Record) []store.Event {
	events := make([]store.Event, len(records))
	for i, r := range records {
		events[i] = eventFromRecord(r)
	}
	return events
}
