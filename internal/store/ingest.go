package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"time"
)

var agentNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

var ErrBatchRejected = errors.New("store: batch rejected")

var ErrSourceOwnership = errors.New("store: source ownership cannot change")

var ErrSourceNotRegistered = errors.New("store: source is not registered")

type Usage struct {
	InputTokens              int64
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64
	OutputTokens             int64
}

type Event struct {
	Offset int64
	End    int64

	NativeID string
	Kind     string
	Role     string
	ToolName string
	Text     string
	RawJSON  string
	Metadata string

	HasTime bool
	TimeMS  int64

	Cwd       string
	GitBranch string
	Repo      string
	Model     string
	MessageID string
	Usage     Usage
	HasUsage  bool

	Prompt string

	HasOrdinal bool
	Ordinal    int64

	ParserVersion int
}

type Batch struct {
	SourceID string
	DeviceID string
	Events   []Event
}

type IngestResult struct {
	Inserted   int
	Duplicates int
	Cursor     int64
}

func SessionID(deviceID, agent, nativeID string) string {
	return agent + ":" + deviceID + ":" + nativeID
}

func (d *DB) RegisterSource(ctx context.Context, id, deviceID, agent, path string) (err error) {
	switch {
	case id == "":
		return fmt.Errorf("store: register source: empty source ID: %w", ErrBatchRejected)
	case deviceID == "":
		return fmt.Errorf("store: register source %s: empty device ID: %w", id, ErrBatchRejected)
	case !agentNameRE.MatchString(agent):
		return fmt.Errorf("store: register source %s: agent name %q is not a lowercase slug: %w", id, agent, ErrBatchRejected)
	case path == "":
		return fmt.Errorf("store: register source %s: empty path: %w", id, ErrBatchRejected)
	}
	txStart := time.Now()
	defer func() {
		d.observer.Observe("sqlite_transaction_duration", time.Since(txStart))
		if outcome, count := sqliteOutcome(err); count {
			d.observer.Add("sqlite_outcomes", 1, outcome)
		}
	}()
	tx, err := d.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: register source %s: %w", id, err)
	}
	defer tx.Rollback()

	var storedDevice, storedAgent, storedPath string
	err = tx.QueryRowContext(ctx,
		`SELECT device_id, agent, path FROM sources WHERE id = ?`, id).
		Scan(&storedDevice, &storedAgent, &storedPath)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO sources (id, device_id, agent, path) VALUES (?, ?, ?, ?)`,
			id, deviceID, agent, path); err != nil {
			return fmt.Errorf("store: register source %s: %w", id, err)
		}
	case err != nil:
		return fmt.Errorf("store: register source %s: read source: %w", id, err)
	case storedDevice != deviceID:
		return fmt.Errorf("store: register source %s: held by device %s, not %s: %w",
			id, storedDevice, deviceID, ErrSourceOwnership)
	case storedAgent != agent:
		return fmt.Errorf("store: register source %s: held for agent %s, not %s: %w",
			id, storedAgent, agent, ErrSourceOwnership)
	case storedPath != path:
		if _, err := tx.ExecContext(ctx,
			`UPDATE sources SET path = ? WHERE id = ?`, path, id); err != nil {
			return fmt.Errorf("store: register source %s: follow path move: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: register source %s: commit: %w", id, err)
	}
	return nil
}

type SourceState struct {
	Agent         string
	Cursor        int64
	FirstNativeID string
}

func (d *DB) SourceState(ctx context.Context, id string) (SourceState, error) {
	return sourceState(ctx, d.writer, id)
}

func sourceState(ctx context.Context, db *sql.DB, id string) (SourceState, error) {
	var st SourceState
	var native sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT src.agent, src.cursor_bytes,
		        (SELECT s.native_id FROM events e JOIN sessions s ON s.id = e.session_id
		         WHERE e.source_id = src.id AND e.source_offset = 0)
		 FROM sources src WHERE src.id = ?`, id).
		Scan(&st.Agent, &st.Cursor, &native)
	if errors.Is(err, sql.ErrNoRows) {
		return SourceState{}, fmt.Errorf("store: source %s is not registered: %w", id, ErrSourceNotRegistered)
	}
	if err != nil {
		return SourceState{}, fmt.Errorf("store: read source %s: %w", id, err)
	}
	st.FirstNativeID = native.String
	return st, nil
}

func (d *DB) Ingest(ctx context.Context, b Batch) (res IngestResult, err error) {
	if b.SourceID == "" {
		return IngestResult{}, fmt.Errorf("store: ingest: empty source ID: %w", ErrBatchRejected)
	}
	if b.DeviceID == "" {
		return IngestResult{}, fmt.Errorf("store: ingest source %s: empty device ID: %w", b.SourceID, ErrBatchRejected)
	}
	if err := validateBatch(b.Events); err != nil {
		return IngestResult{}, err
	}

	txStart := time.Now()
	defer func() {
		d.observer.Observe("sqlite_transaction_duration", time.Since(txStart))
		if outcome, count := sqliteOutcome(err); count {
			d.observer.Add("sqlite_outcomes", 1, outcome)
		}
	}()
	tx, err := d.writer.BeginTx(ctx, nil)
	if err != nil {
		return IngestResult{}, fmt.Errorf("store: ingest source %s: begin: %w", b.SourceID, err)
	}
	defer tx.Rollback()

	var cursor int64
	var deviceID, agent string
	err = tx.QueryRowContext(ctx,
		`SELECT cursor_bytes, device_id, agent FROM sources WHERE id = ?`, b.SourceID).
		Scan(&cursor, &deviceID, &agent)
	if errors.Is(err, sql.ErrNoRows) {
		return IngestResult{}, fmt.Errorf("store: ingest: source %s is not registered; register a source before its first batch: %w",
			b.SourceID, ErrSourceNotRegistered)
	}
	if err != nil {
		return IngestResult{}, fmt.Errorf("store: ingest source %s: read source: %w", b.SourceID, err)
	}
	if b.DeviceID != deviceID {
		return IngestResult{}, fmt.Errorf("store: ingest source %s: source belongs to device %s, but the batch claims device %s: %w",
			b.SourceID, deviceID, b.DeviceID, ErrSourceOwnership)
	}

	var ahead bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM events WHERE source_id = ? AND source_offset >= ?)`,
		b.SourceID, cursor).Scan(&ahead); err != nil {
		return IngestResult{}, fmt.Errorf("store: ingest source %s: check stored records: %w", b.SourceID, err)
	}
	if ahead {
		return IngestResult{}, fmt.Errorf("store: ingest source %s: stored records exist at or beyond the committed cursor %d; database state is inconsistent",
			b.SourceID, cursor)
	}

	if len(b.Events) == 0 {
		return IngestResult{Cursor: cursor}, nil
	}
	if first := b.Events[0].Offset; first > cursor {
		return IngestResult{}, fmt.Errorf("store: ingest source %s: batch starts at offset %d, ahead of the committed cursor %d: bytes would be skipped: %w",
			b.SourceID, first, cursor, ErrBatchRejected)
	}
	if err := validateOverlap(ctx, tx, b, cursor); err != nil {
		return IngestResult{}, err
	}

	inserted, duplicates, deltas, err := insertEvents(ctx, tx, b, cursor, deviceID, agent)
	if err != nil {
		return IngestResult{}, err
	}
	for _, sid := range sortedSessionIDs(deltas) {
		if err := applySessionDelta(ctx, tx, sid, deltas[sid], b.SourceID); err != nil {
			return IngestResult{}, err
		}
	}

	newCursor := cursor
	if last := b.Events[len(b.Events)-1].End; last > newCursor {
		newCursor = last
		if _, err := tx.ExecContext(ctx,
			`UPDATE sources SET cursor_bytes = ?, updated_at_ms = ? WHERE id = ?`,
			newCursor, time.Now().UnixMilli(), b.SourceID); err != nil {
			return IngestResult{}, fmt.Errorf("store: ingest source %s: advance cursor: %w", b.SourceID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return IngestResult{}, fmt.Errorf("store: ingest source %s: commit: %w", b.SourceID, err)
	}
	return IngestResult{Inserted: inserted, Duplicates: duplicates, Cursor: newCursor}, nil
}

func validateBatch(events []Event) error {
	for i := range events {
		e := &events[i]
		if e.Offset < 0 {
			return fmt.Errorf("store: batch record %d has negative offset %d: %w", i, e.Offset, ErrBatchRejected)
		}
		if e.RawJSON == "" {
			return fmt.Errorf("store: batch record %d at offset %d has no JSON text: %w", i, e.Offset, ErrBatchRejected)
		}
		if e.NativeID == "" {
			return fmt.Errorf("store: batch record %d at offset %d has no native session ID: %w", i, e.Offset, ErrBatchRejected)
		}
		if e.HasOrdinal && e.Ordinal < 0 {
			return fmt.Errorf("store: batch record %d at offset %d has negative ordinal %d: %w", i, e.Offset, e.Ordinal, ErrBatchRejected)
		}
		if e.ParserVersion < 1 {
			return fmt.Errorf("store: batch record %d at offset %d carries parser output version %d; every record must name the version of the parser that derived it: %w",
				i, e.Offset, e.ParserVersion, ErrBatchRejected)
		}
		if want := e.Offset + int64(len(e.RawJSON)) + 1; e.End != want {
			return fmt.Errorf("store: batch record %d at offset %d ends at %d, but its %d JSON bytes plus one terminator end at %d: %w",
				i, e.Offset, e.End, len(e.RawJSON), want, ErrBatchRejected)
		}
		if i > 0 && e.Offset != events[i-1].End {
			return fmt.Errorf("store: batch record %d starts at offset %d, but record %d ends at %d: delivery must be contiguous: %w",
				i, e.Offset, i-1, events[i-1].End, ErrBatchRejected)
		}
	}
	return nil
}

func validateOverlap(ctx context.Context, tx *sql.Tx, b Batch, cursor int64) error {
	replayed := 0
	for replayed < len(b.Events) && b.Events[replayed].Offset < cursor {
		replayed++
	}
	if replayed == 0 {
		return nil
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT source_offset, raw_json FROM events
		 WHERE source_id = ? AND source_offset >= ? AND source_offset < ?
		 ORDER BY source_offset`,
		b.SourceID, b.Events[0].Offset, cursor)
	if err != nil {
		return fmt.Errorf("store: ingest source %s: read stored records: %w", b.SourceID, err)
	}
	defer rows.Close()
	stored := 0
	for rows.Next() {
		if stored == replayed {
			break
		}
		var offset int64
		var raw string
		if err := rows.Scan(&offset, &raw); err != nil {
			return fmt.Errorf("store: ingest source %s: read stored record: %w", b.SourceID, err)
		}
		e := &b.Events[stored]
		if e.Offset != offset {
			return fmt.Errorf("store: ingest source %s: stored record sits at offset %d, but the retry covers offset %d below the committed cursor %d: %w",
				b.SourceID, offset, e.Offset, cursor, ErrBatchRejected)
		}
		if e.RawJSON != raw {
			return fmt.Errorf("store: ingest source %s: conflicting retry at offset %d: the stored record and the retry differ (%d and %d JSON bytes): %w",
				b.SourceID, offset, len(raw), len(e.RawJSON), ErrBatchRejected)
		}
		stored++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: ingest source %s: read stored records: %w", b.SourceID, err)
	}
	if stored < replayed {
		return fmt.Errorf("store: ingest source %s: no stored record at offset %d below the committed cursor %d: %w",
			b.SourceID, b.Events[stored].Offset, cursor, ErrBatchRejected)
	}
	return nil
}

type sessionDelta struct {
	count  int
	minMS  int64
	maxMS  int64
	hasMin bool
	hasMax bool
	cwd    string
	branch string
	repo   string
	model  string
	prompt *Event
}

func (d *sessionDelta) add(e *Event) {
	d.count++
	if e.HasTime {
		if !d.hasMin || e.TimeMS < d.minMS {
			d.minMS, d.hasMin = e.TimeMS, true
		}
		if !d.hasMax || e.TimeMS > d.maxMS {
			d.maxMS, d.hasMax = e.TimeMS, true
		}
	}
	if d.cwd == "" {
		d.cwd = e.Cwd
	}
	if d.branch == "" {
		d.branch = e.GitBranch
	}
	if d.repo == "" {
		d.repo = e.Repo
	}
	if d.model == "" {
		d.model = e.Model
	}
}

func insertEvents(ctx context.Context, tx *sql.Tx, b Batch, cursor int64, deviceID, agent string) (int, int, map[string]*sessionDelta, error) {
	deltas := make(map[string]*sessionDelta)
	inserted := 0
	duplicates := 0
	for i := range b.Events {
		e := &b.Events[i]
		if e.Offset < cursor {
			continue
		}
		sid := SessionID(deviceID, agent, e.NativeID)
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO sessions (id, agent, device_id, native_id) VALUES (?, ?, ?, ?)
			 ON CONFLICT (id) DO NOTHING`,
			sid, agent, deviceID, e.NativeID); err != nil {
			return 0, 0, nil, fmt.Errorf("store: ingest source %s: ensure session: %w", b.SourceID, err)
		}
		metadata := e.Metadata
		if metadata == "" {
			metadata = "{}"
		}
		var timeMS, ordinal any
		if e.HasTime {
			timeMS = e.TimeMS
		}
		var duplicateOf sql.NullInt64
		if e.HasOrdinal {
			ordinal = e.Ordinal
			err := tx.QueryRowContext(ctx,
				`SELECT id FROM events
				 WHERE session_id = ? AND ordinal = ? AND raw_json = ? AND duplicate_of IS NULL`,
				sid, e.Ordinal, e.RawJSON).Scan(&duplicateOf)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return 0, 0, nil, fmt.Errorf("store: ingest source %s: look up ordinal at offset %d: %w", b.SourceID, e.Offset, err)
			}
		}
		text := nullString(e.Text)
		if duplicateOf.Valid {
			text = nil
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO events (session_id, source_id, source_offset, time_ms, kind, role, tool_name, text, raw_json, metadata, ordinal, duplicate_of, parser_version)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT (source_id, source_offset) DO NOTHING`,
			sid, b.SourceID, e.Offset, timeMS, e.Kind, nullString(e.Role),
			nullString(e.ToolName), text, e.RawJSON, metadata, ordinal, duplicateOf, e.ParserVersion)
		if err != nil {
			return 0, 0, nil, fmt.Errorf("store: ingest source %s: insert record at offset %d: %w", b.SourceID, e.Offset, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return 0, 0, nil, fmt.Errorf("store: ingest source %s: count records: %w", b.SourceID, err)
		}
		if affected == 0 {
			duplicates++
			continue
		}
		inserted++
		if duplicateOf.Valid {
			duplicates++
			continue
		}

		d := deltas[sid]
		if d == nil {
			d = &sessionDelta{}
			deltas[sid] = d
		}
		d.add(e)
		if d.prompt == nil && e.Prompt != "" {
			d.prompt = e
		}
		if err := insertUsage(ctx, tx, sid, e); err != nil {
			return 0, 0, nil, fmt.Errorf("store: ingest source %s: count usage at offset %d: %w", b.SourceID, e.Offset, err)
		}
	}
	return inserted, duplicates, deltas, nil
}

func insertUsage(ctx context.Context, tx *sql.Tx, sid string, e *Event) error {
	if !e.HasUsage || e.MessageID == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO session_token_usage
		   (session_id, message_id, input_tokens, cache_creation_input_tokens, cache_read_input_tokens, output_tokens)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (session_id, message_id) DO NOTHING`,
		sid, e.MessageID, e.Usage.InputTokens, e.Usage.CacheCreationInputTokens,
		e.Usage.CacheReadInputTokens, e.Usage.OutputTokens)
	return err
}

type promptRef struct {
	SourceID     string `json:"sourceId"`
	SourceOffset int64  `json:"sourceOffset"`
}

func (ref promptRef) before(other promptRef) bool {
	return ref.SourceID < other.SourceID ||
		(ref.SourceID == other.SourceID && ref.SourceOffset < other.SourceOffset)
}

func applySessionDelta(ctx context.Context, tx *sql.Tx, sid string, d *sessionDelta, sourceID string) error {
	var (
		count       int
		deviceID    string
		started     sql.NullInt64
		last        sql.NullInt64
		cwd         sql.NullString
		branch      sql.NullString
		repo        sql.NullString
		model       sql.NullString
		firstPrompt sql.NullString
		metadata    string
	)
	err := tx.QueryRowContext(ctx,
		`SELECT device_id, event_count, started_at_ms, last_event_at_ms, cwd, branch, repo, model, first_prompt, metadata
		 FROM sessions WHERE id = ?`, sid).
		Scan(&deviceID, &count, &started, &last, &cwd, &branch, &repo, &model, &firstPrompt, &metadata)
	if err != nil {
		return fmt.Errorf("store: ingest session %s: read aggregates: %w", sid, err)
	}

	if d.hasMin && (!started.Valid || d.minMS < started.Int64) {
		started = sql.NullInt64{Int64: d.minMS, Valid: true}
	}
	if d.hasMax && (!last.Valid || d.maxMS > last.Int64) {
		last = sql.NullInt64{Int64: d.maxMS, Valid: true}
	}
	if !cwd.Valid && d.cwd != "" {
		cwd = sql.NullString{String: d.cwd, Valid: true}
	}
	if !branch.Valid && d.branch != "" {
		branch = sql.NullString{String: d.branch, Valid: true}
	}
	if !repo.Valid && d.repo != "" {
		repo = sql.NullString{String: d.repo, Valid: true}
	}
	if !repo.Valid && cwd.Valid {
		if repo, err = workspaceRepo(ctx, tx, deviceID, cwd.String); err != nil {
			return fmt.Errorf("store: ingest session %s: read workspace repository: %w", sid, err)
		}
	}
	if !model.Valid && d.model != "" {
		model = sql.NullString{String: d.model, Valid: true}
	}

	md := map[string]json.RawMessage{}
	if metadata != "" && metadata != "{}" {
		if err := json.Unmarshal([]byte(metadata), &md); err != nil {
			return fmt.Errorf("store: ingest session %s: session metadata is not a JSON object: %w", sid, err)
		}
	}
	if d.prompt != nil {
		candidate := promptRef{SourceID: sourceID, SourceOffset: d.prompt.Offset}
		var stored *promptRef
		if raw, ok := md["firstPromptRef"]; ok {
			var ref promptRef
			if err := json.Unmarshal(raw, &ref); err == nil {
				stored = &ref
			}
		}
		replace := !firstPrompt.Valid
		if !replace && stored != nil {
			replace = candidate.before(*stored)
		}
		if replace {
			firstPrompt = sql.NullString{String: d.prompt.Prompt, Valid: true}
			encoded, err := json.Marshal(candidate)
			if err != nil {
				return fmt.Errorf("store: ingest session %s: encode first-prompt reference: %w", sid, err)
			}
			md["firstPromptRef"] = encoded
		}
	}
	encodedMetadata, err := json.Marshal(md)
	if err != nil {
		return fmt.Errorf("store: ingest session %s: encode session metadata: %w", sid, err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE sessions SET
		   event_count = ?, started_at_ms = ?, last_event_at_ms = ?,
		   cwd = ?, branch = ?, repo = ?, model = ?, first_prompt = ?, metadata = ?
		 WHERE id = ?`,
		count+d.count, started, last, cwd, branch, repo, model, firstPrompt, string(encodedMetadata), sid); err != nil {
		return fmt.Errorf("store: ingest session %s: update aggregates: %w", sid, err)
	}
	return nil
}

func sortedSessionIDs(deltas map[string]*sessionDelta) []string {
	return slices.Sorted(maps.Keys(deltas))
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
