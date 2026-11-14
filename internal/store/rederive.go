// Re-derivation: rebuild every normalized field of stored records from the
// preserved raw JSON, inside the hub, without source-file access. The parser
// output version marker (events.parser_version) drives it: when a parser's
// output behavior changes, its OutputVersion constant bumps, and this
// operation rewrites the rows that predate the bump.

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
)

// DeriveInput carries one stored record to a parser for re-derivation. The
// raw JSON is the stored original; the store checks that the parser's output
// still frames exactly the stored bytes.
type DeriveInput struct {
	Agent string
	// SourceID is the record's source. It scopes synthetic native IDs the
	// same way ingestion does, so a synthetic session keeps its identity.
	SourceID string
	// SourceOffset is the record's first byte offset in its source file.
	SourceOffset int64
	// RawJSON is the stored original record text, without the terminator.
	RawJSON string
	// FirstNativeID is the native session ID the source's record at offset 0
	// (or, during the walk, the preceding re-derived record) carries. Parsers
	// whose records do not name their session (Codex) need it past offset 0;
	// it is "" for a source's first record.
	FirstNativeID string
}

// Deriver re-derives one stored record with its parser's current output
// behavior. The returned Event must frame exactly the stored record: Offset,
// End and RawJSON are validated against the stored row. Everything else
// replaces the row's normalized fields.
type Deriver func(DeriveInput) (Event, error)

// ParserDeriver binds one agent's parser to re-derivation. OutputVersion is
// the parser's current output version; rows of that agent carrying a smaller
// version are stale.
type ParserDeriver struct {
	Agent         string
	OutputVersion int
	Derive        Deriver
}

// ReDeriveResult reports one re-derivation pass.
type ReDeriveResult struct {
	// Events counts re-derived and rewritten event rows.
	Events int
	// Sessions counts rebuilt session rows: aggregates rewritten plus empty
	// sessions removed.
	Sessions int
}

// rebuiltSession accumulates one session's recomputed aggregates during the
// walk. Sessions are per agent by construction (the ID embeds the agent), so
// a rebuild never mixes agents in one session.
type rebuiltSession struct {
	id     string
	agent  string
	device string
	native string

	count          int
	minMS, maxMS   int64
	hasMin, hasMax bool

	// First non-empty observation in walk order, which is (source_id,
	// source_offset) order — deterministic, unlike arrival order.
	cwd, branch, model string

	prompt    *Event
	promptRef promptRef
}

// ReDerive rebuilds normalized rows from the stored raw records. When any row
// of a bound parser's agent carries a smaller parser output version than the
// parser reports, every row of that agent is re-derived in one transaction:
// normalized fields are rewritten from raw_json, session aggregates and token
// totals are recomputed, replay duplicates are reassigned in deterministic
// (source, offset) order, and events_fts is rebuilt from the content table
// with the external-content 'rebuild' command. Source cursors, raw records,
// event IDs and annotations are untouched. A row derived by a newer parser
// than a binding reports is refused: an older binary cannot reproduce it.
//
// With no stale rows the operation commits nothing and returns zero counts,
// so an up-to-date hub can call it freely.
func (d *DB) ReDerive(ctx context.Context, parsers []ParserDeriver) (ReDeriveResult, error) {
	byAgent := make(map[string]ParserDeriver, len(parsers))
	for _, p := range parsers {
		switch {
		case !agentNameRE.MatchString(p.Agent):
			return ReDeriveResult{}, fmt.Errorf("store: re-derive: agent name %q is not a lowercase slug", p.Agent)
		case p.OutputVersion < 1:
			return ReDeriveResult{}, fmt.Errorf("store: re-derive agent %s: parser output version %d is below 1", p.Agent, p.OutputVersion)
		case p.Derive == nil:
			return ReDeriveResult{}, fmt.Errorf("store: re-derive agent %s: no derive function", p.Agent)
		}
		if _, dup := byAgent[p.Agent]; dup {
			return ReDeriveResult{}, fmt.Errorf("store: re-derive: agent %s is bound twice", p.Agent)
		}
		byAgent[p.Agent] = p
	}
	if len(byAgent) == 0 {
		return ReDeriveResult{}, fmt.Errorf("store: re-derive: no parsers given")
	}

	tx, err := d.writer.BeginTx(ctx, nil)
	if err != nil {
		return ReDeriveResult{}, fmt.Errorf("store: re-derive: begin: %w", err)
	}
	defer tx.Rollback() // no-op after commit

	var rebuild []string
	for _, agent := range slices.Sorted(maps.Keys(byAgent)) {
		p := byAgent[agent]
		var minV, maxV sql.NullInt64
		if err := tx.QueryRowContext(ctx,
			`SELECT min(e.parser_version), max(e.parser_version)
			 FROM events e JOIN sources s ON s.id = e.source_id
			 WHERE s.agent = ?`, agent).Scan(&minV, &maxV); err != nil {
			return ReDeriveResult{}, fmt.Errorf("store: re-derive agent %s: read versions: %w", agent, err)
		}
		if !maxV.Valid {
			continue // the agent has no events
		}
		if maxV.Int64 > int64(p.OutputVersion) {
			return ReDeriveResult{}, fmt.Errorf("store: re-derive agent %s: stored rows carry parser output version %d above this build's %d; upgrade before re-deriving",
				agent, maxV.Int64, p.OutputVersion)
		}
		if minV.Int64 < int64(p.OutputVersion) {
			rebuild = append(rebuild, agent)
		}
	}
	if len(rebuild) == 0 {
		return ReDeriveResult{}, nil // nothing stale; commit nothing
	}

	for _, agent := range rebuild {
		if err := clearDerived(ctx, tx, agent); err != nil {
			return ReDeriveResult{}, err
		}
	}
	states := map[string]*rebuiltSession{}
	result := ReDeriveResult{}
	for _, agent := range rebuild {
		walked, err := rederiveAgent(ctx, tx, byAgent[agent], states)
		if err != nil {
			return ReDeriveResult{}, err
		}
		result.Events += walked
	}
	for _, sid := range slices.Sorted(maps.Keys(states)) {
		if err := finalizeSession(ctx, tx, states[sid]); err != nil {
			return ReDeriveResult{}, err
		}
		result.Sessions++
	}
	// Sessions of rebuilt agents whose events all moved away hold nothing.
	for _, agent := range rebuild {
		res, err := tx.ExecContext(ctx,
			`DELETE FROM sessions WHERE agent = ?
			   AND NOT EXISTS (SELECT 1 FROM events e WHERE e.session_id = sessions.id)`,
			agent)
		if err != nil {
			return ReDeriveResult{}, fmt.Errorf("store: re-derive agent %s: drop empty sessions: %w", agent, err)
		}
		if n, err := res.RowsAffected(); err == nil {
			result.Sessions += int(n)
		}
	}

	// One pass over the content table reconstructs the whole index from the
	// rewritten events.text; no derived text is stored anywhere else.
	if _, err := tx.ExecContext(ctx, `INSERT INTO events_fts(events_fts) VALUES ('rebuild')`); err != nil {
		return ReDeriveResult{}, fmt.Errorf("store: re-derive: rebuild events_fts: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ReDeriveResult{}, fmt.Errorf("store: re-derive: commit: %w", err)
	}
	return result, nil
}

// clearDerived empties the derived state the walk rebuilds from scratch:
// replay-original marks (so duplicates can be reassigned in walk order
// instead of arrival order) and per-session token totals.
func clearDerived(ctx context.Context, tx *sql.Tx, agent string) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE events SET duplicate_of = id
		 WHERE ordinal IS NOT NULL AND duplicate_of IS NULL
		   AND source_id IN (SELECT id FROM sources WHERE agent = ?)`, agent); err != nil {
		return fmt.Errorf("store: re-derive agent %s: reset replay originals: %w", agent, err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM session_token_usage
		 WHERE session_id IN (SELECT id FROM sessions WHERE agent = ?)`, agent); err != nil {
		return fmt.Errorf("store: re-derive agent %s: clear token totals: %w", agent, err)
	}
	return nil
}

// rederiveSource is one source of an agent being rebuilt: its ID and owning
// device, which fix the session identity of every record it stored.
type rederiveSource struct {
	id     string
	device string
}

// storedEvent is one stored record awaiting re-derivation.
type storedEvent struct {
	id     int64
	offset int64
	raw    string
}

// rederiveChunk bounds how many stored records one read holds in memory, the
// way ingestion bounds a batch: a chunk of records at the size limit stays
// predictable where a whole source (Codex rollouts reach gigabytes) would not.
const rederiveChunk = 128

// rederiveAgent walks one agent's sources in source order and each source's
// records in offset order, re-deriving every stored record and folding the
// session aggregates of the final event set.
func rederiveAgent(ctx context.Context, tx *sql.Tx, p ParserDeriver, states map[string]*rebuiltSession) (int, error) {
	srcRows, err := tx.QueryContext(ctx,
		`SELECT id, device_id FROM sources WHERE agent = ? ORDER BY id`, p.Agent)
	if err != nil {
		return 0, fmt.Errorf("store: re-derive agent %s: list sources: %w", p.Agent, err)
	}
	var sources []rederiveSource
	for srcRows.Next() {
		var s rederiveSource
		if err := srcRows.Scan(&s.id, &s.device); err != nil {
			srcRows.Close()
			return 0, fmt.Errorf("store: re-derive agent %s: read source: %w", p.Agent, err)
		}
		sources = append(sources, s)
	}
	if err := srcRows.Err(); err != nil {
		srcRows.Close()
		return 0, fmt.Errorf("store: re-derive agent %s: list sources: %w", p.Agent, err)
	}
	srcRows.Close()

	walked := 0
	for _, src := range sources {
		// Records are read in bounded chunks, each closed before any update,
		// so the scan never observes the table it is changing and memory
		// stays proportional to the chunk. Keyset pagination on the offset is
		// stable because re-derivation never moves a record's offset.
		//
		// The record at offset 0 establishes the source's native session; each
		// later record sees the one before it, exactly like a fresh parse of
		// the file, so native carries across chunks.
		native := ""
		lastOffset := int64(-1)
		for {
			var chunk []storedEvent
			evRows, err := tx.QueryContext(ctx,
				`SELECT id, source_offset, raw_json FROM events
				 WHERE source_id = ? AND source_offset > ? ORDER BY source_offset LIMIT ?`,
				src.id, lastOffset, rederiveChunk)
			if err != nil {
				return 0, fmt.Errorf("store: re-derive source %s: read records: %w", src.id, err)
			}
			for evRows.Next() {
				var e storedEvent
				if err := evRows.Scan(&e.id, &e.offset, &e.raw); err != nil {
					evRows.Close()
					return 0, fmt.Errorf("store: re-derive source %s: read record: %w", src.id, err)
				}
				chunk = append(chunk, e)
			}
			if err := evRows.Err(); err != nil {
				evRows.Close()
				return 0, fmt.Errorf("store: re-derive source %s: read records: %w", src.id, err)
			}
			evRows.Close()
			if len(chunk) == 0 {
				break
			}

			for _, e := range chunk {
				ev, err := p.Derive(DeriveInput{
					Agent: p.Agent, SourceID: src.id, SourceOffset: e.offset,
					RawJSON: e.raw, FirstNativeID: native,
				})
				if err != nil {
					return 0, fmt.Errorf("store: re-derive source %s: record at offset %d: %w", src.id, e.offset, err)
				}
				if err := checkDerived(p, ev, e.id, e.offset, e.raw); err != nil {
					return 0, fmt.Errorf("store: re-derive source %s: %w", src.id, err)
				}
				native = ev.NativeID
				lastOffset = e.offset
				if err := rewriteEvent(ctx, tx, src, p, e.id, ev, states); err != nil {
					return 0, err
				}
				walked++
			}
			if len(chunk) < rederiveChunk {
				break
			}
		}
	}
	return walked, nil
}

// checkDerived enforces the deriver contract: the output frames exactly the
// stored record, names a session, and reports the version it was bound with.
func checkDerived(p ParserDeriver, ev Event, id, offset int64, raw string) error {
	switch {
	case ev.Offset != offset:
		return fmt.Errorf("record %d: parser output starts at offset %d, but the stored record starts at %d: %w",
			id, ev.Offset, offset, ErrBatchRejected)
	case ev.End != offset+int64(len(raw))+1:
		return fmt.Errorf("record %d: parser output ends at %d, but the stored %d JSON bytes plus one terminator end at %d: %w",
			id, ev.End, len(raw), offset+int64(len(raw))+1, ErrBatchRejected)
	case ev.RawJSON != raw:
		return fmt.Errorf("record %d at offset %d: parser output rewrites the original record (%d and %d JSON bytes): %w",
			id, offset, len(raw), len(ev.RawJSON), ErrBatchRejected)
	case ev.NativeID == "":
		return fmt.Errorf("record %d at offset %d: parser output names no native session ID: %w", id, offset, ErrBatchRejected)
	case ev.HasOrdinal && ev.Ordinal < 0:
		return fmt.Errorf("record %d at offset %d: parser output has negative ordinal %d: %w", id, offset, ev.Ordinal, ErrBatchRejected)
	case ev.ParserVersion != p.OutputVersion:
		return fmt.Errorf("record %d at offset %d: parser output carries version %d, but the parser is bound as version %d: %w",
			id, offset, ev.ParserVersion, p.OutputVersion, ErrBatchRejected)
	}
	return nil
}

// rewriteEvent stores one re-derived record and folds its session state. The
// write mirrors ingestion: a byte-identical duplicate of an already assigned
// (session, ordinal) original keeps NULL text and contributes nothing to
// aggregates or token totals, so content-bearing text exists once and FTS
// indexes it once. A record with different bytes at the same (session,
// ordinal) re-enumerates the ordinal and is another original. storedID is the
// row's immutable ID; re-derivation updates rows in place so
// event_annotations keep their targets.
func rewriteEvent(ctx context.Context, tx *sql.Tx, src rederiveSource, p ParserDeriver, storedID int64, ev Event, states map[string]*rebuiltSession) error {
	sid := SessionID(src.device, p.Agent, ev.NativeID)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO sessions (id, agent, device_id, native_id) VALUES (?, ?, ?, ?)
		 ON CONFLICT (id) DO NOTHING`,
		sid, p.Agent, src.device, ev.NativeID); err != nil {
		return fmt.Errorf("store: re-derive source %s: ensure session: %w", src.id, err)
	}

	var duplicateOf sql.NullInt64
	if ev.HasOrdinal {
		err := tx.QueryRowContext(ctx,
			`SELECT id FROM events
			 WHERE session_id = ? AND ordinal = ? AND raw_json = ? AND duplicate_of IS NULL`,
			sid, ev.Ordinal, ev.RawJSON).Scan(&duplicateOf)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("store: re-derive source %s: look up ordinal at offset %d: %w", src.id, ev.Offset, err)
		}
	}
	text := nullString(ev.Text)
	var dup any
	if duplicateOf.Valid {
		text = nil
		dup = duplicateOf.Int64
	}
	metadata := ev.Metadata
	if metadata == "" {
		metadata = "{}"
	}
	var timeMS any
	if ev.HasTime {
		timeMS = ev.TimeMS
	}
	var ordinal any
	if ev.HasOrdinal {
		ordinal = ev.Ordinal
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE events SET
		   session_id = ?, time_ms = ?, kind = ?, role = ?, tool_name = ?, text = ?,
		   metadata = ?, ordinal = ?, duplicate_of = ?, parser_version = ?
		 WHERE id = ?`,
		sid, timeMS, ev.Kind, nullString(ev.Role), nullString(ev.ToolName), text,
		metadata, ordinal, dup, p.OutputVersion, storedID); err != nil {
		return fmt.Errorf("store: re-derive source %s: rewrite record at offset %d: %w", src.id, ev.Offset, err)
	}
	if duplicateOf.Valid {
		return nil
	}

	st := states[sid]
	if st == nil {
		st = &rebuiltSession{id: sid, agent: p.Agent, device: src.device, native: ev.NativeID}
		states[sid] = st
	}
	st.count++
	if ev.HasTime {
		if !st.hasMin || ev.TimeMS < st.minMS {
			st.minMS, st.hasMin = ev.TimeMS, true
		}
		if !st.hasMax || ev.TimeMS > st.maxMS {
			st.maxMS, st.hasMax = ev.TimeMS, true
		}
	}
	if st.cwd == "" {
		st.cwd = ev.Cwd
	}
	if st.branch == "" {
		st.branch = ev.GitBranch
	}
	if st.model == "" {
		st.model = ev.Model
	}
	ref := promptRef{SourceID: src.id, SourceOffset: ev.Offset}
	if ev.Prompt != "" && (st.prompt == nil || ref.before(st.promptRef)) {
		st.prompt = &ev
		st.promptRef = ref
	}
	if ev.HasUsage && ev.MessageID != "" {
		// Walk order is the deterministic order, so the first usage of a
		// message ID is the same one a fresh ingest of the same records
		// would keep.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO session_token_usage
			   (session_id, message_id, input_tokens, cache_creation_input_tokens, cache_read_input_tokens, output_tokens)
			 VALUES (?, ?, ?, ?, ?, ?)
			 ON CONFLICT (session_id, message_id) DO NOTHING`,
			sid, ev.MessageID, ev.Usage.InputTokens, ev.Usage.CacheCreationInputTokens,
			ev.Usage.CacheReadInputTokens, ev.Usage.OutputTokens); err != nil {
			return fmt.Errorf("store: re-derive source %s: count usage at offset %d: %w", src.id, ev.Offset, err)
		}
	}
	return nil
}

// finalizeSession writes one session's recomputed aggregates. The repo column
// and unknown metadata keys are preserved; only firstPromptRef is managed
// here, exactly like the write path does.
func finalizeSession(ctx context.Context, tx *sql.Tx, st *rebuiltSession) error {
	var metadata string
	if err := tx.QueryRowContext(ctx,
		`SELECT metadata FROM sessions WHERE id = ?`, st.id).Scan(&metadata); err != nil {
		return fmt.Errorf("store: re-derive session %s: read metadata: %w", st.id, err)
	}
	md := map[string]json.RawMessage{}
	if metadata != "" && metadata != "{}" {
		if err := json.Unmarshal([]byte(metadata), &md); err != nil {
			return fmt.Errorf("store: re-derive session %s: session metadata is not a JSON object: %w", st.id, err)
		}
	}
	var firstPrompt any
	if st.prompt != nil {
		firstPrompt = st.prompt.Prompt
		encoded, err := json.Marshal(st.promptRef)
		if err != nil {
			return fmt.Errorf("store: re-derive session %s: encode first-prompt reference: %w", st.id, err)
		}
		md["firstPromptRef"] = encoded
	} else {
		delete(md, "firstPromptRef")
	}
	encodedMetadata, err := json.Marshal(md)
	if err != nil {
		return fmt.Errorf("store: re-derive session %s: encode session metadata: %w", st.id, err)
	}
	var started, last any
	if st.hasMin {
		started = st.minMS
	}
	if st.hasMax {
		last = st.maxMS
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE sessions SET
		   event_count = ?, started_at_ms = ?, last_event_at_ms = ?,
		   cwd = ?, branch = ?, model = ?, first_prompt = ?, metadata = ?
		 WHERE id = ?`,
		st.count, started, last,
		nullString(st.cwd), nullString(st.branch), nullString(st.model),
		firstPrompt, string(encodedMetadata), st.id); err != nil {
		return fmt.Errorf("store: re-derive session %s: update aggregates: %w", st.id, err)
	}
	return nil
}
