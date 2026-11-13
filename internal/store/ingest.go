// Transactional ingestion write path. One Batch commits atomically: events
// are inserted, session aggregates and token totals advance only for those
// inserts, FTS is maintained by the insert trigger, and the source cursor
// moves over contiguous committed bytes. Validation runs before any insert,
// so a rejected batch changes nothing; uniqueness is a safety net, not the
// proof of a safe retry.

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

// agentNameRE bounds agent names on registered sources. It repeats the rule
// from internal/watch (agent names also become state file names there), so
// the hub rejects the same names local ingestion rejects. SessionID also
// relies on colon-free agent names.
var agentNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ErrBatchRejected marks a batch the write path refused for a deterministic
// reason: retrying the same bytes cannot succeed. The sync endpoint maps it
// to a permanent rejection the device must not retry as-is.
var ErrBatchRejected = errors.New("store: batch rejected")

// ErrSourceOwnership marks an attempt to move a registered source to another
// device or another agent. Ownership is fixed when the source is registered
// and never changes.
var ErrSourceOwnership = errors.New("store: source ownership cannot change")

// ErrSourceNotRegistered marks a batch for a source the hub never registered.
// The uploader must register the source before its first batch.
var ErrSourceNotRegistered = errors.New("store: source is not registered")

// Usage holds the unambiguous token counts of one API request. Agent parsers
// map their native usage fields onto these; counts the native record omits
// stay zero. Every other usage field (tiers, costs, details) is deliberately
// absent: it has no defined sum.
type Usage struct {
	InputTokens              int64
	CacheCreationInputTokens int64
	CacheReadInputTokens     int64
	OutputTokens             int64
}

// Event is one normalized record ready for the write path. The agent parser
// produces it; the store derives the session from the source's device and
// agent plus NativeID, validates byte boundaries, and preserves RawJSON
// verbatim.
type Event struct {
	// Offset is the record's first byte offset in its source file. End is
	// the offset just past the line terminator, so End = Offset plus the
	// JSON length plus exactly one terminator byte.
	Offset int64
	End    int64

	NativeID string // native session identity; may be a synthetic ID
	Kind     string
	Role     string // "" when the record has no role
	ToolName string // "" when absent or ambiguous
	Text     string // searchable text; "" contributes nothing to FTS
	RawJSON  string // original record text, without the terminator
	Metadata string // compact JSON object of deterministic observations

	HasTime bool
	TimeMS  int64 // Unix milliseconds; meaningful only when HasTime

	Cwd       string // working-directory observation
	GitBranch string // branch observation
	Model     string // model observation
	MessageID string // groups the records of one API response
	Usage     Usage
	HasUsage  bool

	// UserPrompt marks the record as a user prompt — a fresh prompt, not a
	// tool result — so the write path can select sessions.first_prompt.
	UserPrompt bool
}

// Batch is one source's contiguous records, in source order.
type Batch struct {
	SourceID string
	// DeviceID is the identity submitting the batch. A source is bound to one
	// device at registration, so a batch claiming another device is rejected
	// instead of silently writing sessions under the wrong identity.
	DeviceID string
	Events   []Event
}

// IngestResult reports one committed batch.
type IngestResult struct {
	// Inserted counts only newly inserted events. Records that replayed
	// already-stored bytes do not count.
	Inserted int
	// Cursor is the source's committed byte cursor after the commit. It is
	// the offset the caller may acknowledge durably.
	Cursor int64
}

// SessionID encodes (device_id, agent, native_id) unambiguously as
// agent:device_id:native_id. Agent names and device IDs cannot contain a
// colon, so the first two separators always split the identity.
func SessionID(deviceID, agent, nativeID string) string {
	return agent + ":" + deviceID + ":" + nativeID
}

// RegisterSource ensures the sources row for one file incarnation exists.
// Call it before the source's first batch, as the local watcher does before
// its first ingestion. Re-registering the same source is a no-op that only
// follows a path move; a source never changes its device or agent.
func (d *DB) RegisterSource(ctx context.Context, id, deviceID, agent, path string) error {
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
	tx, err := d.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: register source %s: %w", id, err)
	}
	defer tx.Rollback() // no-op after commit

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

// Ingest commits one batch atomically and returns what the commit added.
//
// Validation precedes every insert: records must be contiguous within the
// batch, the batch must start at or behind the stored cursor, bytes already
// below the cursor must replay identically, and a batch that starts ahead of
// the cursor is rejected so no bytes are ever skipped silently. A rejected
// batch leaves the cursor, the events, the aggregates and FTS untouched, and
// its error carries offsets and lengths only — never transcript content.
// Other sources are unaffected; ingest them separately.
func (d *DB) Ingest(ctx context.Context, b Batch) (IngestResult, error) {
	if b.SourceID == "" {
		return IngestResult{}, fmt.Errorf("store: ingest: empty source ID: %w", ErrBatchRejected)
	}
	if b.DeviceID == "" {
		return IngestResult{}, fmt.Errorf("store: ingest source %s: empty device ID: %w", b.SourceID, ErrBatchRejected)
	}
	if err := validateBatch(b.Events); err != nil {
		return IngestResult{}, err
	}

	tx, err := d.writer.BeginTx(ctx, nil)
	if err != nil {
		return IngestResult{}, fmt.Errorf("store: ingest source %s: begin: %w", b.SourceID, err)
	}
	defer tx.Rollback() // no-op after commit

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

	// Committed records never sit at or beyond the cursor. If they do, the
	// database contradicts its own acknowledgments; stop instead of adding.
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

	inserted, deltas, err := insertEvents(ctx, tx, b, cursor, deviceID, agent)
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
	return IngestResult{Inserted: inserted, Cursor: newCursor}, nil
}

// validateBatch checks the batch's shape before any database work: every
// record spans exactly its JSON bytes plus one terminator, and the records
// are contiguous and in source order.
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

// validateOverlap proves that records replaying bytes below the committed
// cursor match the stored records exactly. Uniqueness alone says nothing
// about payload equality, so a conflicting retry stops the source instead of
// hiding changed history.
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
			break // stored bytes continue past the replayed prefix
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

// sessionDelta accumulates the aggregate changes of one session's newly
// inserted events.
type sessionDelta struct {
	count  int
	minMS  int64 // observed minimum; valid only when hasMin
	maxMS  int64 // observed maximum; valid only when hasMax
	hasMin bool
	hasMax bool
	// First non-empty observation in batch order.
	cwd    string
	branch string
	model  string
	prompt *Event // first UserPrompt candidate with text
}

// insertEvents inserts the batch's new records — those at or beyond the
// cursor — and returns how many were new, plus the per-session aggregate
// deltas of exactly those inserts. Records already stored (a retry the caller
// replayed after a lost acknowledgment) are skipped without counting.
func insertEvents(ctx context.Context, tx *sql.Tx, b Batch, cursor int64, deviceID, agent string) (int, map[string]*sessionDelta, error) {
	deltas := make(map[string]*sessionDelta)
	inserted := 0
	for i := range b.Events {
		e := &b.Events[i]
		if e.Offset < cursor {
			continue // replayed bytes; validated above, never re-inserted
		}
		sid := SessionID(deviceID, agent, e.NativeID)
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO sessions (id, agent, device_id, native_id) VALUES (?, ?, ?, ?)
			 ON CONFLICT (id) DO NOTHING`,
			sid, agent, deviceID, e.NativeID); err != nil {
			return 0, nil, fmt.Errorf("store: ingest source %s: ensure session: %w", b.SourceID, err)
		}
		metadata := e.Metadata
		if metadata == "" {
			metadata = "{}"
		}
		var timeMS any
		if e.HasTime {
			timeMS = e.TimeMS
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO events (session_id, source_id, source_offset, time_ms, kind, role, tool_name, text, raw_json, metadata)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT (source_id, source_offset) DO NOTHING`,
			sid, b.SourceID, e.Offset, timeMS, e.Kind, nullString(e.Role),
			nullString(e.ToolName), nullString(e.Text), e.RawJSON, metadata)
		if err != nil {
			return 0, nil, fmt.Errorf("store: ingest source %s: insert record at offset %d: %w", b.SourceID, e.Offset, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return 0, nil, fmt.Errorf("store: ingest source %s: count records: %w", b.SourceID, err)
		}
		if affected == 0 {
			continue // already stored; not a newly inserted record
		}
		inserted++

		d := deltas[sid]
		if d == nil {
			d = &sessionDelta{}
			deltas[sid] = d
		}
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
		if d.model == "" {
			d.model = e.Model
		}
		if d.prompt == nil && e.UserPrompt && e.Text != "" {
			d.prompt = e
		}
		if e.HasUsage && e.MessageID != "" {
			// One row per response: the first usage of a message ID is
			// counted, later repeats — even differing ones — are ignored.
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO session_token_usage
				   (session_id, message_id, input_tokens, cache_creation_input_tokens, cache_read_input_tokens, output_tokens)
				 VALUES (?, ?, ?, ?, ?, ?)
				 ON CONFLICT (session_id, message_id) DO NOTHING`,
				sid, e.MessageID, e.Usage.InputTokens, e.Usage.CacheCreationInputTokens,
				e.Usage.CacheReadInputTokens, e.Usage.OutputTokens); err != nil {
				return 0, nil, fmt.Errorf("store: ingest source %s: count usage at offset %d: %w", b.SourceID, e.Offset, err)
			}
		}
	}
	return inserted, deltas, nil
}

// promptRef records where the selected first prompt came from, so a later
// candidate can be compared by (source_id, source_offset) rather than by
// arrival order. It lives in sessions.metadata.
type promptRef struct {
	SourceID     string `json:"sourceId"`
	SourceOffset int64  `json:"sourceOffset"`
}

// before reports whether ref picks an earlier record than other under the
// deterministic source/order tie-break.
func (ref promptRef) before(other promptRef) bool {
	return ref.SourceID < other.SourceID ||
		(ref.SourceID == other.SourceID && ref.SourceOffset < other.SourceOffset)
}

// applySessionDelta folds one session's new inserts into its aggregates.
// Missing observations never overwrite known values: timestamp bounds widen
// by observed min/max only, and cwd, branch and model keep the first
// non-empty observation.
func applySessionDelta(ctx context.Context, tx *sql.Tx, sid string, d *sessionDelta, sourceID string) error {
	var (
		count       int
		started     sql.NullInt64
		last        sql.NullInt64
		cwd         sql.NullString
		branch      sql.NullString
		model       sql.NullString
		firstPrompt sql.NullString
		metadata    string
	)
	err := tx.QueryRowContext(ctx,
		`SELECT event_count, started_at_ms, last_event_at_ms, cwd, branch, model, first_prompt, metadata
		 FROM sessions WHERE id = ?`, sid).
		Scan(&count, &started, &last, &cwd, &branch, &model, &firstPrompt, &metadata)
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
	if !model.Valid && d.model != "" {
		model = sql.NullString{String: d.model, Valid: true}
	}

	// metadata holds observations from other writers too; preserve unknown
	// keys and only manage firstPromptRef.
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
			firstPrompt = sql.NullString{String: d.prompt.Text, Valid: true}
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
		   cwd = ?, branch = ?, model = ?, first_prompt = ?, metadata = ?
		 WHERE id = ?`,
		count+d.count, started, last, cwd, branch, model, firstPrompt, string(encodedMetadata), sid); err != nil {
		return fmt.Errorf("store: ingest session %s: update aggregates: %w", sid, err)
	}
	return nil
}

// sortedSessionIDs returns the delta map's keys in a stable order, so map
// iteration order never reaches the database.
func sortedSessionIDs(deltas map[string]*sessionDelta) []string {
	return slices.Sorted(maps.Keys(deltas))
}

// nullString maps an absent optional text field to SQL NULL.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
