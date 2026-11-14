package events

import (
	"fmt"
	"os"
	"reflect"
	"testing"

	"github.com/nibzard/ohya/internal/claudecode"
	"github.com/nibzard/ohya/internal/codex"
	"github.com/nibzard/ohya/internal/pi"
	"github.com/nibzard/ohya/internal/store"
)

// TestDeriversReproduceFreshParse pins the re-derivation contract: parsing
// each stored record on its own, with the source scope and the native ID the
// walk supplies, yields exactly the event a fresh ingestion of the same file
// produced. If this fails after a parser change, re-derivation would rewrite
// rows differently than ingestion — the two paths must stay identical.
func TestDeriversReproduceFreshParse(t *testing.T) {
	byAgent := map[string]store.ParserDeriver{}
	for _, p := range Derivers() {
		byAgent[p.Agent] = p
	}

	tests := []struct {
		agent   string
		scope   string
		fixture string
	}{
		{agent: "claude-code", scope: "src-cc", fixture: "../../testdata/claude-code/main-session.jsonl"},
		{agent: "claude-code", scope: "src-cc-sub", fixture: "../../testdata/claude-code/subagent-session.jsonl"},
		{agent: "codex", scope: "src-cx", fixture: "../../testdata/codex/main-session.jsonl"},
		{agent: "codex", scope: "src-cx-page", fixture: "../../testdata/codex/resumed-page.jsonl"},
		{agent: "pi", scope: "src-pi", fixture: "../../testdata/pi/main-session.jsonl"},
		{agent: "pi", scope: "src-pi-err", fixture: "../../testdata/pi/error-session.jsonl"},
	}
	for _, tc := range tests {
		fresh, err := parseFixtureFile(tc.agent, tc.fixture, tc.scope)
		if err != nil {
			t.Fatalf("%s %s: %v", tc.agent, tc.fixture, err)
		}
		deriver := byAgent[tc.agent]
		if deriver.OutputVersion != fresh.version {
			t.Fatalf("%s: deriver version %d, parser version %d", tc.agent, deriver.OutputVersion, fresh.version)
		}
		// The walk feeds each record the native ID of the one before it; a
		// source's first record establishes it (Codex session_meta).
		native := ""
		for i, ev := range fresh.events {
			derived, err := deriver.Derive(store.DeriveInput{
				Agent: tc.agent, SourceID: tc.scope, SourceOffset: ev.Offset,
				RawJSON: ev.RawJSON, FirstNativeID: native,
			})
			if err != nil {
				t.Fatalf("%s %s: record %d at offset %d: %v", tc.agent, tc.fixture, i, ev.Offset, err)
			}
			if !reflect.DeepEqual(derived, ev) {
				t.Fatalf("%s %s: record %d at offset %d re-derived differently:\n got %+v\nwant %+v",
					tc.agent, tc.fixture, i, ev.Offset, derived, ev)
			}
			native = ev.NativeID
		}
	}
}

// TestDeriversRejectUnfaithfulRecords covers the deriver's own failure paths:
// a stored record that no longer parses, and one that frames more than the
// single stored record.
func TestDeriversRejectUnfaithfulRecords(t *testing.T) {
	byAgent := map[string]store.ParserDeriver{}
	for _, p := range Derivers() {
		byAgent[p.Agent] = p
	}
	for agent, raw := range map[string]string{
		"claude-code": `{"type":"user",`,
		"codex":       `{"type":"response_item","payload":{"type":"mess`,
		"pi":          `{"type":"message","id":"ab0000`,
	} {
		p := byAgent[agent]
		if _, err := p.Derive(store.DeriveInput{Agent: agent, SourceID: "s", SourceOffset: 0, RawJSON: raw}); err == nil {
			t.Fatalf("%s: a record the parser refuses re-derived without error", agent)
		}
		// Two records in one RawJSON cannot be one stored record; the walk
		// would rewrite one row from two records' bytes.
		double := `{"type":"user","sessionId":"s1","message":{"role":"user","content":"a"}}` + "\n" +
			`{"type":"user","sessionId":"s1","message":{"role":"user","content":"b"}}`
		if agent == "codex" {
			double = `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"a"}]},"timestamp":"2026-11-13T09:00:00Z"}` + "\n" +
				`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"b"}]},"timestamp":"2026-11-13T09:00:01Z"}`
		}
		if agent == "pi" {
			double = `{"type":"message","id":"ab000001","parentId":null,"timestamp":"2026-11-13T09:00:00.000Z","message":{"role":"user","content":[{"type":"text","text":"a"}]}}` + "\n" +
				`{"type":"message","id":"ab000002","parentId":"ab000001","timestamp":"2026-11-13T09:00:01.000Z","message":{"role":"user","content":[{"type":"text","text":"b"}]}}`
		}
		if _, err := p.Derive(store.DeriveInput{Agent: agent, SourceID: "s", SourceOffset: 0, RawJSON: double}); err == nil {
			t.Fatalf("%s: two records in one RawJSON re-derived without error", agent)
		}
	}
}

// parsedFile holds one fixture's fresh parse plus the parser's own output
// version, so the test can check the binding keeps up with the constant.
type parsedFile struct {
	events  []store.Event
	version int
}

// parseFixtureFile parses one fixture file with the agent's parser through
// the production adapters.
func parseFixtureFile(agent, path, scope string) (parsedFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return parsedFile{}, err
	}
	switch agent {
	case "claude-code":
		res, err := claudecode.Parse(data, 0, claudecode.Options{SourceScope: scope})
		if err != nil {
			return parsedFile{}, err
		}
		return parsedFile{events: FromClaudeCodeRecords(res.Records), version: claudecode.OutputVersion}, nil
	case "codex":
		res, err := codex.Parse(data, 0, codex.Options{SourceScope: scope})
		if err != nil {
			return parsedFile{}, err
		}
		return parsedFile{events: FromCodexRecords(res.Records), version: codex.OutputVersion}, nil
	case "pi":
		res, err := pi.Parse(data, 0, pi.Options{SourceScope: scope})
		if err != nil {
			return parsedFile{}, err
		}
		return parsedFile{events: FromPiRecords(res.Records), version: pi.OutputVersion}, nil
	}
	return parsedFile{}, fmt.Errorf("no parser for %q", agent)
}
