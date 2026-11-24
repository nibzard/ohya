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

type DeriveInput struct {
	Agent         string
	SourceID      string
	SourcePath    string
	SourceOffset  int64
	RawJSON       string
	FirstNativeID string
}

type Deriver func(DeriveInput) (Event, error)

type ParserDeriver struct {
	Agent         string
	OutputVersion int
	Derive        Deriver
}

type ReDeriveResult struct {
	Events   int
	Sessions int
}

type rebuiltSession struct {
	id string

	sessionDelta

	workspace string
	promptRef promptRef
}

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
	defer tx.Rollback()

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
			continue
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
		return ReDeriveResult{}, nil
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

	if _, err := tx.ExecContext(ctx, `INSERT INTO events_fts(events_fts) VALUES ('delete-all')`); err != nil {
		return ReDeriveResult{}, fmt.Errorf("store: re-derive: clear events_fts: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events_fts(rowid, text) SELECT id, text FROM events WHERE text IS NOT NULL AND length(text) > 0`); err != nil {
		return ReDeriveResult{}, fmt.Errorf("store: re-derive: rebuild events_fts: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ReDeriveResult{}, fmt.Errorf("store: re-derive: commit: %w", err)
	}
	_, _ = d.writer.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	return result, nil
}

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

type rederiveSource struct {
	id          string
	device      string
	path        string
	firstNative string
	workspace   string
}

type storedEvent struct {
	id     int64
	offset int64
	raw    string
}

const rederiveChunk = 128

func rederiveAgent(ctx context.Context, tx *sql.Tx, p ParserDeriver, states map[string]*rebuiltSession) (int, error) {
	srcRows, err := tx.QueryContext(ctx,
		`SELECT id, device_id, path, first_native_id, workspace_path FROM sources WHERE agent = ? ORDER BY id`, p.Agent)
	if err != nil {
		return 0, fmt.Errorf("store: re-derive agent %s: list sources: %w", p.Agent, err)
	}
	var sources []rederiveSource
	for srcRows.Next() {
		var s rederiveSource
		var firstNative, workspace sql.NullString
		if err := srcRows.Scan(&s.id, &s.device, &s.path, &firstNative, &workspace); err != nil {
			srcRows.Close()
			return 0, fmt.Errorf("store: re-derive agent %s: read source: %w", p.Agent, err)
		}
		s.firstNative = firstNative.String
		s.workspace = workspace.String
		sources = append(sources, s)
	}
	if err := srcRows.Err(); err != nil {
		srcRows.Close()
		return 0, fmt.Errorf("store: re-derive agent %s: list sources: %w", p.Agent, err)
	}
	srcRows.Close()

	walked := 0
	for _, src := range sources {
		native := src.firstNative
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
					Agent: p.Agent, SourceID: src.id, SourcePath: src.path, SourceOffset: e.offset,
					RawJSON: e.raw, FirstNativeID: native,
				})
				if err != nil {
					return 0, fmt.Errorf("store: re-derive source %s: record at offset %d: %w", src.id, e.offset, err)
				}
				if err := checkDerived(p, ev, e.id, e.offset, e.raw); err != nil {
					return 0, fmt.Errorf("store: re-derive source %s: %w", src.id, err)
				}
				if e.offset == 0 && ev.NativeID != src.firstNative {
					if _, err := tx.ExecContext(ctx,
						`UPDATE sources SET first_native_id = ? WHERE id = ?`,
						ev.NativeID, src.id); err != nil {
						return 0, fmt.Errorf("store: re-derive source %s: update first native ID: %w", src.id, err)
					}
					src.firstNative = ev.NativeID
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
		st = &rebuiltSession{id: sid}
		states[sid] = st
	}
	st.add(&ev)
	if st.workspace == "" {
		st.workspace = src.workspace
	}
	ref := promptRef{SourceID: src.id, SourceOffset: ev.Offset}
	if ev.Prompt != "" && (st.prompt == nil || ref.before(st.promptRef)) {
		st.prompt = &ev
		st.promptRef = ref
	}
	if err := insertUsage(ctx, tx, sid, &ev); err != nil {
		return fmt.Errorf("store: re-derive source %s: count usage at offset %d: %w", src.id, ev.Offset, err)
	}
	return nil
}

func finalizeSession(ctx context.Context, tx *sql.Tx, st *rebuiltSession) error {
	var metadata, deviceID string
	if err := tx.QueryRowContext(ctx,
		`SELECT metadata, device_id FROM sessions WHERE id = ?`, st.id).Scan(&metadata, &deviceID); err != nil {
		return fmt.Errorf("store: re-derive session %s: read metadata: %w", st.id, err)
	}
	cwd := st.cwd
	if cwd == "" {
		cwd = st.workspace
	}
	repo := sql.NullString{String: st.repo, Valid: st.repo != ""}
	if !repo.Valid && cwd != "" {
		var err error
		if repo, err = workspaceRepo(ctx, tx, deviceID, cwd); err != nil {
			return fmt.Errorf("store: re-derive session %s: read workspace repository: %w", st.id, err)
		}
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
		   cwd = ?, branch = ?, repo = ?, model = ?, first_prompt = ?, metadata = ?
		 WHERE id = ?`,
		st.count, started, last,
		nullString(cwd), nullString(st.branch), repo, nullString(st.model),
		firstPrompt, string(encodedMetadata), st.id); err != nil {
		return fmt.Errorf("store: re-derive session %s: update aggregates: %w", st.id, err)
	}
	return nil
}
