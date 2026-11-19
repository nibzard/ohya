package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const syntheticNativePrefix = "ohya-synthetic:"

var ErrSourcePurged = errors.New("store: source was purged by the owner")

var ErrNotPurged = errors.New("store: source is not purged")

type PurgeSelector struct {
	Session   string
	Repo      string
	Agent     string
	HasBefore bool
	BeforeMS  int64
}

func (s PurgeSelector) beforeActive() bool {
	return s.HasBefore || s.BeforeMS != 0
}

func (s PurgeSelector) Empty() bool {
	return s.Session == "" && s.Repo == "" && s.Agent == "" && !s.beforeActive()
}

func (s PurgeSelector) Scope() string {
	var parts []string
	if s.Session != "" {
		parts = append(parts, "session="+s.Session)
	}
	if s.Repo != "" {
		parts = append(parts, "repo="+s.Repo)
	}
	if s.Agent != "" {
		parts = append(parts, "agent="+s.Agent)
	}
	if s.beforeActive() {
		parts = append(parts, "before="+time.UnixMilli(s.BeforeMS).UTC().Format(time.RFC3339))
	}
	return strings.Join(parts, " ")
}

func (s PurgeSelector) sessionFilter() (string, []any) {
	conds := []string{"s.native_id NOT LIKE ? || '%'"}
	args := []any{syntheticNativePrefix}
	if s.Session != "" {
		conds = append(conds, "s.native_id = ?")
		args = append(args, s.Session)
	}
	if s.Repo != "" {
		conds = append(conds, "s.repo = ?")
		args = append(args, s.Repo)
	}
	if s.Agent != "" {
		conds = append(conds, "s.agent = ?")
		args = append(args, s.Agent)
	}
	if s.beforeActive() {
		conds = append(conds, "COALESCE(s.last_event_at_ms, s.started_at_ms) IS NOT NULL")
		conds = append(conds, "COALESCE(s.last_event_at_ms, s.started_at_ms) < ?")
		args = append(args, s.BeforeMS)
	}
	return strings.Join(conds, " AND "), args
}

type PurgeSession struct {
	ID       string
	Agent    string
	DeviceID string
	NativeID string
	Events   int64
}

type PurgeSource struct {
	ID       string
	DeviceID string
	Agent    string
	Path     string
	Cursor   int64
	Events   int64
	NativeID string
}

type PurgePlan struct {
	Sessions []PurgeSession
	Sources  []PurgeSource
	Events   int64
	Bytes    int64
}

type PurgedSourceInfo struct {
	SourceID      string
	DeviceID      string
	Agent         string
	Path          string
	Cursor        int64
	NativeID      string
	FirstNativeID string
	Scope         string
	PurgedAtMS    int64
}

func (p PurgedSourceInfo) refusal() error {
	return fmt.Errorf("store: source %s was purged by the owner on %s (scope %s); re-admission is an owner action: run 'ohya sources reallow %s' on the hub and restart ohya join on the device: %w",
		p.SourceID, msRFC3339(p.PurgedAtMS), p.Scope, p.SourceID, ErrSourcePurged)
}

func msRFC3339(ms int64) string {
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}

func readPurged(ctx context.Context, q purgeQuerier, id string) (PurgedSourceInfo, error) {
	var p PurgedSourceInfo
	var native, firstNative sql.NullString
	err := q.QueryRowContext(ctx,
		`SELECT source_id, device_id, agent, path, cursor_bytes, native_id, first_native_id, scope, purged_at_ms
		 FROM purged_sources WHERE source_id = ?`, id).
		Scan(&p.SourceID, &p.DeviceID, &p.Agent, &p.Path, &p.Cursor, &native, &firstNative, &p.Scope, &p.PurgedAtMS)
	if errors.Is(err, sql.ErrNoRows) {
		return PurgedSourceInfo{}, nil
	}
	if err != nil {
		return PurgedSourceInfo{}, fmt.Errorf("store: read purged source %s: %w", id, err)
	}
	p.NativeID = native.String
	p.FirstNativeID = firstNative.String
	return p, nil
}

type purgeQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func (d *DB) PlanPurge(ctx context.Context, sel PurgeSelector) (PurgePlan, error) {
	if sel.Empty() {
		return PurgePlan{}, fmt.Errorf("store: purge needs at least one selector: %w", ErrBatchRejected)
	}
	tx, err := d.writer.BeginTx(ctx, nil)
	if err != nil {
		return PurgePlan{}, fmt.Errorf("store: plan purge: begin: %w", err)
	}
	defer tx.Rollback()
	return d.planPurge(ctx, tx, sel)
}

func (d *DB) planPurge(ctx context.Context, q purgeQuerier, sel PurgeSelector) (PurgePlan, error) {
	filter, args := sel.sessionFilter()
	plan := PurgePlan{Sessions: []PurgeSession{}, Sources: []PurgeSource{}}

	sessionRows, err := q.QueryContext(ctx,
		`SELECT s.id, s.agent, s.device_id, s.native_id,
		        (SELECT count(*) FROM events e WHERE e.session_id = s.id)
		 FROM sessions s WHERE `+filter+` ORDER BY s.id`, args...)
	if err != nil {
		return PurgePlan{}, fmt.Errorf("store: plan purge: read sessions: %w", err)
	}
	selected := map[string]bool{}
	for sessionRows.Next() {
		var ps PurgeSession
		if err := sessionRows.Scan(&ps.ID, &ps.Agent, &ps.DeviceID, &ps.NativeID, &ps.Events); err != nil {
			sessionRows.Close()
			return PurgePlan{}, fmt.Errorf("store: plan purge: read session: %w", err)
		}
		plan.Sessions = append(plan.Sessions, ps)
		selected[ps.ID] = true
	}
	if err := sessionRows.Err(); err != nil {
		sessionRows.Close()
		return PurgePlan{}, fmt.Errorf("store: plan purge: read sessions: %w", err)
	}
	sessionRows.Close()
	if len(selected) == 0 {
		return plan, nil
	}

	sourceArgs := make([]any, 0, len(args)+1)
	sourceArgs = append(sourceArgs, syntheticNativePrefix)
	sourceArgs = append(sourceArgs, args...)
	sourceRows, err := q.QueryContext(ctx,
		`SELECT src.id, src.device_id, src.agent, src.path, src.cursor_bytes,
		        (SELECT count(*) FROM events e WHERE e.source_id = src.id),
		        (SELECT s.native_id FROM sessions s
		         WHERE s.id IN (SELECT DISTINCT e.session_id FROM events e WHERE e.source_id = src.id)
		         ORDER BY (s.native_id LIKE ? || '%'), s.native_id LIMIT 1)
		 FROM sources src
		 WHERE src.id IN (
		   SELECT DISTINCT e.source_id FROM events e
		   JOIN sessions s ON s.id = e.session_id
		   WHERE `+filter+`
		 )
		 ORDER BY src.id`,
		sourceArgs...)
	if err != nil {
		return PurgePlan{}, fmt.Errorf("store: plan purge: read sources: %w", err)
	}
	for sourceRows.Next() {
		var ps PurgeSource
		var native sql.NullString
		if err := sourceRows.Scan(&ps.ID, &ps.DeviceID, &ps.Agent, &ps.Path, &ps.Cursor, &ps.Events, &native); err != nil {
			sourceRows.Close()
			return PurgePlan{}, fmt.Errorf("store: plan purge: read source: %w", err)
		}
		ps.NativeID = native.String
		plan.Sources = append(plan.Sources, ps)
		plan.Events += ps.Events
		plan.Bytes += ps.Cursor
	}
	if err := sourceRows.Err(); err != nil {
		sourceRows.Close()
		return PurgePlan{}, fmt.Errorf("store: plan purge: read sources: %w", err)
	}
	sourceRows.Close()

	for _, src := range plan.Sources {
		if err := checkSourceSessionsSelected(ctx, q, src.ID, selected); err != nil {
			return PurgePlan{}, err
		}
	}
	return plan, nil
}

func checkSourceSessionsSelected(ctx context.Context, q purgeQuerier, sourceID string, selected map[string]bool) error {
	rows, err := q.QueryContext(ctx,
		`SELECT DISTINCT s.id, s.native_id
		 FROM events e JOIN sessions s ON s.id = e.session_id
		 WHERE e.source_id = ?
		 ORDER BY s.id`, sourceID)
	if err != nil {
		return fmt.Errorf("store: purge source %s: check sessions: %w", sourceID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var sid, native string
		if err := rows.Scan(&sid, &native); err != nil {
			return fmt.Errorf("store: purge source %s: check session: %w", sourceID, err)
		}
		if !strings.HasPrefix(native, syntheticNativePrefix) && !selected[sid] {
			return fmt.Errorf("store: purge source %s: source also holds unselected session %s; refusing to evict it: %w",
				sourceID, sid, ErrBatchRejected)
		}
	}
	return rows.Err()
}

func (d *DB) ApplyPurge(ctx context.Context, sel PurgeSelector) (PurgePlan, PurgeResult, error) {
	if sel.Empty() {
		return PurgePlan{}, PurgeResult{}, fmt.Errorf("store: purge needs at least one selector: %w", ErrBatchRejected)
	}
	plan, err := d.PlanPurge(ctx, sel)
	if err != nil {
		return PurgePlan{}, PurgeResult{}, err
	}
	selected := make(map[string]bool, len(plan.Sessions))
	for _, s := range plan.Sessions {
		selected[s.ID] = true
	}
	result := PurgeResult{Plan: plan, PurgedAtMS: time.Now().UnixMilli()}
	for i, src := range plan.Sources {
		committedCursor, err := d.purgeOneSource(ctx, src, selected, sel.Scope(), result.PurgedAtMS)
		if err != nil {
			return plan, result, err
		}
		if committedCursor >= 0 {
			plan.Bytes += committedCursor - plan.Sources[i].Cursor
			plan.Sources[i].Cursor = committedCursor
		}
	}
	result.Plan = plan
	if err := d.dropEmptySelectedSessions(ctx, plan); err != nil {
		return plan, result, err
	}
	return plan, result, nil
}

type PurgeResult struct {
	Plan       PurgePlan
	PurgedAtMS int64
}

func (d *DB) purgeOneSource(ctx context.Context, src PurgeSource, selected map[string]bool, scope string, nowMS int64) (committedCursor int64, err error) {
	txStart := time.Now()
	defer func() {
		d.observer.Observe("sqlite_transaction_duration", time.Since(txStart))
		if outcome, count := sqliteOutcome(err); count {
			d.observer.Add("sqlite_outcomes", 1, outcome)
		}
	}()
	tx, err := d.writer.BeginTx(ctx, nil)
	if err != nil {
		return -1, fmt.Errorf("store: purge source %s: begin: %w", src.ID, err)
	}
	defer tx.Rollback()

	var (
		deviceID, agent, path string
		cursorBytes           int64
		firstNative           sql.NullString
	)
	err = tx.QueryRowContext(ctx,
		`SELECT device_id, agent, path, cursor_bytes, first_native_id
		 FROM sources WHERE id = ?`, src.ID).
		Scan(&deviceID, &agent, &path, &cursorBytes, &firstNative)
	if errors.Is(err, sql.ErrNoRows) {
		return -1, nil
	}
	if err != nil {
		return -1, fmt.Errorf("store: purge source %s: read source: %w", src.ID, err)
	}

	if err := checkSourceSessionsSelected(ctx, tx, src.ID, selected); err != nil {
		return -1, err
	}

	if !firstNative.Valid || firstNative.String == "" {
		var offsetZero sql.NullString
		err := tx.QueryRowContext(ctx,
			`SELECT s.native_id FROM events e JOIN sessions s ON s.id = e.session_id
			 WHERE e.source_id = ? AND e.source_offset = 0`, src.ID).Scan(&offsetZero)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return -1, fmt.Errorf("store: purge source %s: read first native ID: %w", src.ID, err)
		}
		if offsetZero.Valid && offsetZero.String != "" {
			firstNative = offsetZero
		}
	}

	sessionRows, err := tx.QueryContext(ctx,
		`SELECT DISTINCT session_id FROM events WHERE source_id = ? ORDER BY session_id`, src.ID)
	if err != nil {
		return -1, fmt.Errorf("store: purge source %s: read sessions: %w", src.ID, err)
	}
	var sessions []string
	for sessionRows.Next() {
		var sid string
		if err := sessionRows.Scan(&sid); err != nil {
			sessionRows.Close()
			return -1, fmt.Errorf("store: purge source %s: read session: %w", src.ID, err)
		}
		sessions = append(sessions, sid)
	}
	if err := sessionRows.Err(); err != nil {
		sessionRows.Close()
		return -1, fmt.Errorf("store: purge source %s: read sessions: %w", src.ID, err)
	}
	sessionRows.Close()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM event_annotations WHERE event_id IN
		   (SELECT id FROM events WHERE source_id = ?)`, src.ID); err != nil {
		return -1, fmt.Errorf("store: purge source %s: delete annotations: %w", src.ID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE events SET duplicate_of = NULL
		 WHERE source_id != ?
		   AND duplicate_of IN (SELECT id FROM events WHERE source_id = ?)`,
		src.ID, src.ID); err != nil {
		return -1, fmt.Errorf("store: purge source %s: detach cross-source duplicates: %w", src.ID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM events WHERE source_id = ?`, src.ID); err != nil {
		return -1, fmt.Errorf("store: purge source %s: delete events: %w", src.ID, err)
	}
	for _, sid := range sessions {
		if err := dropSessionIfEmpty(ctx, tx, sid); err != nil {
			return -1, fmt.Errorf("store: purge source %s: %w", src.ID, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sources WHERE id = ?`, src.ID); err != nil {
		return -1, fmt.Errorf("store: purge source %s: delete source: %w", src.ID, err)
	}
	var native, first any
	if src.NativeID != "" {
		native = src.NativeID
	}
	if firstNative.Valid && firstNative.String != "" {
		first = firstNative.String
	} else if src.NativeID != "" {
		first = src.NativeID
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO purged_sources
		   (source_id, device_id, agent, path, cursor_bytes, native_id, first_native_id, scope, purged_at_ms)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		src.ID, deviceID, agent, path, cursorBytes, native, first, scope, nowMS); err != nil {
		return -1, fmt.Errorf("store: purge source %s: record tombstone: %w", src.ID, err)
	}
	if err := tx.Commit(); err != nil {
		return -1, fmt.Errorf("store: purge source %s: commit: %w", src.ID, err)
	}
	return cursorBytes, nil
}

func dropSessionIfEmpty(ctx context.Context, tx *sql.Tx, sid string) error {
	var hasEvents bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM events WHERE session_id = ?)`, sid).Scan(&hasEvents); err != nil {
		return fmt.Errorf("read session %s events: %w", sid, err)
	}
	if hasEvents {
		return nil
	}
	var deviceID string
	var cwd sql.NullString
	err := tx.QueryRowContext(ctx,
		`SELECT device_id, cwd FROM sessions WHERE id = ?`, sid).Scan(&deviceID, &cwd)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read session %s workspace: %w", sid, err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM session_token_usage WHERE session_id = ?`, sid); err != nil {
		return fmt.Errorf("delete token usage of session %s: %w", sid, err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM sessions WHERE id = ?`, sid); err != nil {
		return fmt.Errorf("delete session %s: %w", sid, err)
	}
	if cwd.Valid && cwd.String != "" {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM workspaces
			 WHERE device_id = ? AND path = ?
			   AND NOT EXISTS (SELECT 1 FROM sessions WHERE device_id = ? AND cwd = ?)`,
			deviceID, cwd.String, deviceID, cwd.String); err != nil {
			return fmt.Errorf("prune workspace for session %s: %w", sid, err)
		}
	}
	return nil
}

func (d *DB) dropEmptySelectedSessions(ctx context.Context, plan PurgePlan) error {
	if len(plan.Sessions) == 0 {
		return nil
	}
	tx, err := d.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: purge: begin: %w", err)
	}
	defer tx.Rollback()
	for _, ps := range plan.Sessions {
		if err := dropSessionIfEmpty(ctx, tx, ps.ID); err != nil {
			return fmt.Errorf("store: purge: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: purge: commit: %w", err)
	}
	return nil
}

func (d *DB) purgedSource(ctx context.Context, id string) (PurgedSourceInfo, error) {
	return readPurged(ctx, d.writer, id)
}

func (d *DB) ReallowSource(ctx context.Context, id string) (info PurgedSourceInfo, err error) {
	if id == "" {
		return PurgedSourceInfo{}, fmt.Errorf("store: reallow: empty source ID: %w", ErrNotPurged)
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
		return PurgedSourceInfo{}, fmt.Errorf("store: reallow %s: begin: %w", id, err)
	}
	defer tx.Rollback()
	p, err := readPurged(ctx, tx, id)
	if err != nil {
		return PurgedSourceInfo{}, fmt.Errorf("store: reallow %s: %w", id, err)
	}
	if p.SourceID == "" {
		return PurgedSourceInfo{}, fmt.Errorf("store: reallow %s: no purged source with this ID: %w", id, ErrNotPurged)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM purged_sources WHERE source_id = ?`, id); err != nil {
		return PurgedSourceInfo{}, fmt.Errorf("store: reallow %s: remove tombstone: %w", id, err)
	}
	var firstNative any
	if p.FirstNativeID != "" {
		firstNative = p.FirstNativeID
	} else if p.NativeID != "" {
		firstNative = p.NativeID
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO sources (id, device_id, agent, path, cursor_bytes, first_native_id, updated_at_ms)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		p.SourceID, p.DeviceID, p.Agent, p.Path, p.Cursor, firstNative, time.Now().UnixMilli()); err != nil {
		return PurgedSourceInfo{}, fmt.Errorf("store: reallow %s: restore source: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return PurgedSourceInfo{}, fmt.Errorf("store: reallow %s: commit: %w", id, err)
	}
	return p, nil
}
