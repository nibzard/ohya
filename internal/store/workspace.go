package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nibzard/ohya/internal/workspace"
)

const MaxWorkspacePathBytes = 4096

var ErrObservationRejected = errors.New("store: workspace observation rejected")

type ObserveResult struct {
	Applied   int
	Unchanged int
	Ignored   int
}

func (d *DB) ObserveWorkspaces(ctx context.Context, deviceID string, observations []workspace.Observation) (ObserveResult, error) {
	var res ObserveResult
	if deviceID == "" {
		return res, fmt.Errorf("store: observe workspaces: empty device ID: %w", ErrObservationRejected)
	}
	for _, o := range observations {
		if err := validObservation(o); err != nil {
			return res, err
		}
	}
	if len(observations) == 0 {
		return res, nil
	}
	tx, err := d.writer.BeginTx(ctx, nil)
	if err != nil {
		return res, fmt.Errorf("store: observe workspaces: %w", err)
	}
	defer tx.Rollback()
	now := time.Now().UnixMilli()
	for _, o := range observations {
		outcome, err := observeWorkspace(ctx, tx, deviceID, o, now)
		if err != nil {
			return ObserveResult{}, err
		}
		switch outcome {
		case observationApplied:
			res.Applied++
		case observationUnchanged:
			res.Unchanged++
		default:
			res.Ignored++
		}
	}
	if err := tx.Commit(); err != nil {
		return ObserveResult{}, fmt.Errorf("store: observe workspaces: commit: %w", err)
	}
	return res, nil
}

type observationOutcome int

const (
	observationIgnored observationOutcome = iota
	observationApplied
	observationUnchanged
)

func observeWorkspace(ctx context.Context, tx *sql.Tx, deviceID string, o workspace.Observation, now int64) (observationOutcome, error) {
	where := fmt.Sprintf("store: observe workspace %q of device %s", o.Path, deviceID)
	var known bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM sessions WHERE device_id = ? AND cwd = ?)`,
		deviceID, o.Path).Scan(&known); err != nil {
		return 0, fmt.Errorf("%s: find sessions: %w", where, err)
	}
	if !known {
		return observationIgnored, nil
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO workspaces (device_id, path, repo, observed_at_ms) VALUES (?, ?, ?, ?)
		 ON CONFLICT (device_id, path) DO NOTHING`,
		deviceID, o.Path, nullString(o.Repo), now)
	if err != nil {
		return 0, fmt.Errorf("%s: insert: %w", where, err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("%s: count: %w", where, err)
	}
	if inserted == 0 {
		return observationUnchanged, nil
	}
	if o.Repo != "" {
		if _, err := tx.ExecContext(ctx,
			`UPDATE sessions SET repo = ? WHERE device_id = ? AND cwd = ? AND repo IS NULL`,
			o.Repo, deviceID, o.Path); err != nil {
			return 0, fmt.Errorf("%s: fill session repositories: %w", where, err)
		}
	}
	return observationApplied, nil
}

func validObservation(o workspace.Observation) error {
	switch {
	case o.Path == "":
		return fmt.Errorf("store: observe workspace: empty path: %w", ErrObservationRejected)
	case len(o.Path) > MaxWorkspacePathBytes:
		return fmt.Errorf("store: observe workspace: path of %d bytes exceeds %d: %w",
			len(o.Path), MaxWorkspacePathBytes, ErrObservationRejected)
	case !utf8.ValidString(o.Path) || strings.ContainsRune(o.Path, 0):
		return fmt.Errorf("store: observe workspace: path is not valid text: %w", ErrObservationRejected)
	case o.Repo != "" && workspace.NormalizeRemote(o.Repo) != o.Repo:
		return fmt.Errorf("store: observe workspace %q: repository %q is not in normalized form: %w",
			o.Path, o.Repo, ErrObservationRejected)
	}
	return nil
}

func (d *DB) PendingWorkspaces(ctx context.Context, deviceID, agent string, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := d.writer.QueryContext(ctx,
		`SELECT DISTINCT s.cwd FROM sessions s
		 WHERE s.device_id = ? AND s.agent = ? AND s.cwd IS NOT NULL AND s.repo IS NULL
		   AND length(CAST(s.cwd AS BLOB)) <= ?
		   AND NOT EXISTS (SELECT 1 FROM workspaces w WHERE w.device_id = s.device_id AND w.path = s.cwd)
		 ORDER BY s.cwd LIMIT ?`,
		deviceID, agent, MaxWorkspacePathBytes, limit)
	if err != nil {
		return nil, fmt.Errorf("store: pending workspaces of device %s: %w", deviceID, err)
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("store: pending workspaces of device %s: %w", deviceID, err)
		}
		paths = append(paths, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: pending workspaces of device %s: %w", deviceID, err)
	}
	return paths, nil
}

func workspaceRepo(ctx context.Context, tx *sql.Tx, deviceID, cwd string) (sql.NullString, error) {
	var repo sql.NullString
	err := tx.QueryRowContext(ctx,
		`SELECT repo FROM workspaces WHERE device_id = ? AND path = ?`, deviceID, cwd).Scan(&repo)
	if errors.Is(err, sql.ErrNoRows) {
		return sql.NullString{}, nil
	}
	return repo, err
}
