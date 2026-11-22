package events

import (
	"fmt"

	"github.com/nibzard/ohya/internal/antigravity"
	"github.com/nibzard/ohya/internal/claudecode"
	"github.com/nibzard/ohya/internal/codex"
	"github.com/nibzard/ohya/internal/pi"
	"github.com/nibzard/ohya/internal/store"
)

func Derivers() []store.ParserDeriver {
	return []store.ParserDeriver{
		{Agent: claudecode.AgentName, OutputVersion: claudecode.OutputVersion, Derive: deriveClaudeCode},
		{Agent: codex.AgentName, OutputVersion: codex.OutputVersion, Derive: deriveCodex},
		{Agent: pi.AgentName, OutputVersion: pi.OutputVersion, Derive: derivePi},
		{Agent: antigravity.AgentName, OutputVersion: antigravity.OutputVersion, Derive: deriveAntigravity},
	}
}

func deriveClaudeCode(in store.DeriveInput) (store.Event, error) {
	res, err := claudecode.Parse([]byte(in.RawJSON+"\n"), in.SourceOffset,
		claudecode.Options{SourceScope: in.SourceID, MaxRecordBytes: int64(len(in.RawJSON))})
	if err != nil {
		return store.Event{}, fmt.Errorf("events: re-derive claude-code: %w", err)
	}
	if len(res.Records) != 1 {
		return store.Event{}, fmt.Errorf("events: re-derive claude-code: record at offset %d re-parsed into %d records, want 1",
			in.SourceOffset, len(res.Records))
	}
	return FromClaudeCode(res.Records[0]), nil
}

func deriveCodex(in store.DeriveInput) (store.Event, error) {
	res, err := codex.Parse([]byte(in.RawJSON+"\n"), in.SourceOffset,
		codex.Options{SourceScope: in.SourceID, Native: in.FirstNativeID, MaxRecordBytes: int64(len(in.RawJSON))})
	if err != nil {
		return store.Event{}, fmt.Errorf("events: re-derive codex: %w", err)
	}
	if len(res.Records) != 1 {
		return store.Event{}, fmt.Errorf("events: re-derive codex: record at offset %d re-parsed into %d records, want 1",
			in.SourceOffset, len(res.Records))
	}
	return FromCodex(res.Records[0]), nil
}

func derivePi(in store.DeriveInput) (store.Event, error) {
	res, err := pi.Parse([]byte(in.RawJSON+"\n"), in.SourceOffset,
		pi.Options{SourceScope: in.SourceID, Native: in.FirstNativeID, MaxRecordBytes: int64(len(in.RawJSON))})
	if err != nil {
		return store.Event{}, fmt.Errorf("events: re-derive pi: %w", err)
	}
	if len(res.Records) != 1 {
		return store.Event{}, fmt.Errorf("events: re-derive pi: record at offset %d re-parsed into %d records, want 1",
			in.SourceOffset, len(res.Records))
	}
	return FromPi(res.Records[0]), nil
}

func deriveAntigravity(in store.DeriveInput) (store.Event, error) {
	res, err := antigravity.Parse([]byte(in.RawJSON+"\n"), in.SourceOffset,
		antigravity.Options{SourceScope: in.SourceID, SourcePath: in.SourcePath, Native: in.FirstNativeID, MaxRecordBytes: int64(len(in.RawJSON))})
	if err != nil {
		return store.Event{}, fmt.Errorf("events: re-derive antigravity: %w", err)
	}
	if len(res.Records) != 1 {
		return store.Event{}, fmt.Errorf("events: re-derive antigravity: record at offset %d re-parsed into %d records, want 1",
			in.SourceOffset, len(res.Records))
	}
	return FromAntigravity(res.Records[0]), nil
}
