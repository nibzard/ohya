package store

import (
	"context"
	"fmt"
)

func (d *DB) RecordLocalIngestDevice(ctx context.Context, deviceID string) error {
	err := d.retryWriteLock(ctx, func() error {
		_, err := d.writer.ExecContext(ctx, `INSERT INTO local_ingest_devices (device_id) VALUES (?)
			ON CONFLICT(device_id) DO NOTHING`, deviceID)
		return err
	})
	if err != nil {
		return fmt.Errorf("store: record local ingest device: %w", err)
	}
	return nil
}

func (d *DB) LocalIngestDevices(ctx context.Context, limit int) ([]string, error) {
	readers, err := d.Reader()
	if err != nil {
		return nil, err
	}
	defer readers.Close()
	rows, err := readers.QueryContext(ctx, `SELECT device_id FROM local_ingest_devices ORDER BY device_id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list local ingest devices: %w", err)
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("store: scan local ingest device: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list local ingest devices: %w", err)
	}
	return ids, nil
}
