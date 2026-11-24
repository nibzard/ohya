package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrSourceWorkspaceRejected = errors.New("store: source workspace rejected")

type SourcePath struct {
	ID   string
	Path string
}

func (d *DB) SetSourceWorkspace(ctx context.Context, deviceID, sourceID, path string) (err error) {
	switch {
	case deviceID == "":
		return fmt.Errorf("store: set source workspace: empty device ID: %w", ErrSourceWorkspaceRejected)
	case sourceID == "":
		return fmt.Errorf("store: set source workspace: empty source ID: %w", ErrSourceWorkspaceRejected)
	}
	if err := workspacePathProblem(path); err != nil {
		return fmt.Errorf("store: set source workspace %s: %v: %w", sourceID, err, ErrSourceWorkspaceRejected)
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
		return fmt.Errorf("store: set source workspace %s: %w", sourceID, err)
	}
	defer tx.Rollback()

	var storedDevice string
	var stored sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT device_id, workspace_path FROM sources WHERE id = ?`, sourceID).
		Scan(&storedDevice, &stored)
	if errors.Is(err, sql.ErrNoRows) {
		purged, perr := readPurged(ctx, tx, sourceID)
		if perr != nil {
			return fmt.Errorf("store: set source workspace %s: %w", sourceID, perr)
		}
		if purged.SourceID != "" {
			if purged.DeviceID != deviceID {
				return fmt.Errorf("store: source %s belongs to device %s, but device %s names it: %w",
					sourceID, purged.DeviceID, deviceID, ErrSourceOwnership)
			}
			return purged.refusal()
		}
		return fmt.Errorf("store: source %s is not registered: %w", sourceID, ErrSourceNotRegistered)
	}
	if err != nil {
		return fmt.Errorf("store: read source %s: %w", sourceID, err)
	}
	if storedDevice != deviceID {
		return fmt.Errorf("store: source %s belongs to device %s, but device %s names it: %w",
			sourceID, storedDevice, deviceID, ErrSourceOwnership)
	}
	if stored.Valid {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE sessions SET
		   cwd = ?,
		   repo = COALESCE(repo, (SELECT w.repo FROM workspaces w
		                          WHERE w.device_id = sessions.device_id AND w.path = ?))
		 WHERE cwd IS NULL AND id IN (SELECT session_id FROM events WHERE source_id = ?)`,
		path, path, sourceID); err != nil {
		return fmt.Errorf("store: set source workspace %s: fill session directories: %w", sourceID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE sources SET workspace_path = ? WHERE id = ?`, path, sourceID); err != nil {
		return fmt.Errorf("store: set source workspace %s: store: %w", sourceID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: set source workspace %s: commit: %w", sourceID, err)
	}
	return nil
}

func (d *DB) SourcesWithoutWorkspace(ctx context.Context, deviceID, agent string) ([]SourcePath, error) {
	rows, err := d.writer.QueryContext(ctx,
		`SELECT id, path FROM sources
		 WHERE device_id = ? AND agent = ? AND workspace_path IS NULL
		 ORDER BY id`, deviceID, agent)
	if err != nil {
		return nil, fmt.Errorf("store: list sources without a workspace of device %s: %w", deviceID, err)
	}
	defer rows.Close()
	var sources []SourcePath
	for rows.Next() {
		var s SourcePath
		if err := rows.Scan(&s.ID, &s.Path); err != nil {
			return nil, fmt.Errorf("store: list sources without a workspace of device %s: %w", deviceID, err)
		}
		sources = append(sources, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list sources without a workspace of device %s: %w", deviceID, err)
	}
	return sources, nil
}
