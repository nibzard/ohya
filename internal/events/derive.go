// Re-derivation bindings for store.ReDerive (T025). The parser output
// version marker is separate from the schema version: when a parser's
// output behavior changes, its OutputVersion bumps and the hub re-derives
// stored rows from raw_json. These bindings run the same parse calls
// ingestion runs, so a re-derived row equals a freshly ingested one.
package events

import (
	"fmt"

	"github.com/nibzard/ohya/internal/claudecode"
	"github.com/nibzard/ohya/internal/codex"
	"github.com/nibzard/ohya/internal/pi"
	"github.com/nibzard/ohya/internal/store"
)

// Derivers binds every known parser to the hub's re-derivation operation,
// each at its current output version. Pass the result to store.ReDerive.
func Derivers() []store.ParserDeriver {
	return []store.ParserDeriver{
		{Agent: claudecode.AgentName, OutputVersion: claudecode.OutputVersion, Derive: deriveClaudeCode},
		{Agent: codex.AgentName, OutputVersion: codex.OutputVersion, Derive: deriveCodex},
		{Agent: pi.AgentName, OutputVersion: pi.OutputVersion, Derive: derivePi},
	}
}

// deriveClaudeCode re-parses one stored Claude Code record. SourceScope is
// the source ID, exactly as ingestion passes it, so a record without a
// sessionId keeps its synthetic native ID and its session never moves.
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

// deriveCodex re-parses one stored Codex record. FirstNativeID carries the
// thread the source's record at offset 0 established — the walk supplies the
// preceding record's ID — so a record past offset 0 keeps its session.
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

// derivePi re-parses one stored Pi record. FirstNativeID carries the session
// the source's session header at offset 0 established, exactly as ingestion
// takes it from store.SourceState, so a record past offset 0 keeps its
// session.
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
