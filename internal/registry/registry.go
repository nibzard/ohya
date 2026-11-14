package registry

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const FileName = "registry.db"

const schemaVersion = 1

const DefaultInvitationTTL = 15 * time.Minute

const DefaultPollInterval = time.Second

const secretBytes = 16

var (
	ErrInvitationUnknown = errors.New("registry: invitation is not valid")
	ErrInvitationUsed    = errors.New("registry: invitation was already used")
	ErrInvitationExpired = errors.New("registry: invitation expired")
	ErrDevicePaired      = errors.New("registry: device is already paired")
	ErrDeviceRevoked     = errors.New("registry: device is revoked; re-admission needs a new device identity")
	ErrDeviceUnknown     = errors.New("registry: device is not paired")
	ErrAlreadyRevoked    = errors.New("registry: device is already revoked")
	ErrCredentialUnknown = errors.New("registry: credential does not belong to a paired device")
)

type Invitation struct {
	ID          string
	Secret      string
	CreatedAtMS int64
	ExpiresAtMS int64
}

type Credential struct {
	DeviceID string
	Secret   string
}

type DeviceInfo struct {
	DeviceID    string
	State       string
	PairedVia   string
	PairedAtMS  int64
	RevokedAtMS int64
}

type InvitationInfo struct {
	ID          string
	State       string
	UsedBy      string
	CreatedAtMS int64
	ExpiresAtMS int64
	Expired     bool
}

type DB struct {
	path   string
	writer *sql.DB
}

func Open(path string) (*DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("registry: resolve %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return nil, fmt.Errorf("registry: create state directory: %w", err)
	}
	writer, err := sql.Open("sqlite3", dsn(abs))
	if err != nil {
		return nil, fmt.Errorf("registry: open %s: %w", abs, err)
	}
	writer.SetMaxOpenConns(1)
	writer.SetMaxIdleConns(1)
	db := &DB{path: abs, writer: writer}
	if err := db.ensureSchema(context.Background()); err != nil {
		_ = writer.Close()
		return nil, err
	}
	_ = os.Chmod(abs, 0o600)
	return db, nil
}

func (d *DB) Close() error { return d.writer.Close() }

func (d *DB) ensureSchema(ctx context.Context) error {
	var version int
	if err := d.writer.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("registry: read schema version: %w", err)
	}
	if version == 0 && d.tablesExist(ctx) {
		return fmt.Errorf("registry: %s holds tables but no schema version; refusing to guess", d.path)
	}
	if version > schemaVersion {
		return fmt.Errorf("registry: %s has schema version %d, newer than this binary's %d", d.path, version, schemaVersion)
	}
	if version == schemaVersion {
		return nil
	}
	tx, err := d.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("registry: create schema: %w", err)
	}
	defer tx.Rollback()
	for _, stmt := range []string{
		`CREATE TABLE invitations (
		   id             TEXT PRIMARY KEY,
		   secret_digest  TEXT NOT NULL UNIQUE,
		   state          TEXT NOT NULL CHECK (state IN ('open','used')),
		   used_by        TEXT,
		   expires_at_ms  INTEGER NOT NULL CHECK (expires_at_ms > 0),
		   created_at_ms  INTEGER NOT NULL
		 ) STRICT`,
		`CREATE TABLE devices (
		   device_id         TEXT PRIMARY KEY,
		   state             TEXT NOT NULL CHECK (state IN ('active','revoked')),
		   credential_digest TEXT NOT NULL UNIQUE,
		   paired_via        TEXT NOT NULL REFERENCES invitations(id),
		   paired_at_ms      INTEGER NOT NULL,
		   revoked_at_ms     INTEGER
		 ) STRICT`,
		`PRAGMA user_version = ` + strconv.Itoa(schemaVersion),
	} {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("registry: create schema: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("registry: create schema: commit: %w", err)
	}
	return nil
}

func (d *DB) tablesExist(ctx context.Context) bool {
	var n int
	err := d.writer.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name IN ('invitations', 'devices')`).
		Scan(&n)
	return err == nil && n > 0
}

func (d *DB) Issue(ctx context.Context, ttl time.Duration) (Invitation, error) {
	if ttl <= 0 {
		return Invitation{}, fmt.Errorf("registry: issue invitation: ttl %s is not positive", ttl)
	}
	id, err := newSecret()
	if err != nil {
		return Invitation{}, err
	}
	secret, err := newSecret()
	if err != nil {
		return Invitation{}, err
	}
	now := time.Now().UnixMilli()
	inv := Invitation{
		ID:          id,
		Secret:      secret,
		CreatedAtMS: now,
		ExpiresAtMS: now + ttl.Milliseconds(),
	}
	_, err = d.writer.ExecContext(ctx,
		`INSERT INTO invitations (id, secret_digest, state, expires_at_ms, created_at_ms)
		 VALUES (?, ?, 'open', ?, ?)`,
		inv.ID, digest(inv.Secret), inv.ExpiresAtMS, inv.CreatedAtMS)
	if err != nil {
		return Invitation{}, fmt.Errorf("registry: issue invitation: %w", err)
	}
	return inv, nil
}

func (d *DB) Consume(ctx context.Context, invitationSecret, deviceID string) (Credential, error) {
	if deviceID == "" {
		return Credential{}, fmt.Errorf("registry: pair: empty device ID: %w", ErrDeviceUnknown)
	}
	if len(deviceID) > 64 {
		return Credential{}, fmt.Errorf("registry: pair: device ID exceeds 64 bytes: %w", ErrDeviceUnknown)
	}
	secret, err := newSecret()
	if err != nil {
		return Credential{}, err
	}

	tx, err := d.writer.BeginTx(ctx, nil)
	if err != nil {
		return Credential{}, fmt.Errorf("registry: pair: %w", err)
	}
	defer tx.Rollback()

	var invID, state string
	var expiresAtMS int64
	err = tx.QueryRowContext(ctx,
		`SELECT id, state, expires_at_ms FROM invitations WHERE secret_digest = ?`,
		digest(invitationSecret)).Scan(&invID, &state, &expiresAtMS)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Credential{}, ErrInvitationUnknown
	case err != nil:
		return Credential{}, fmt.Errorf("registry: pair: read invitation: %w", err)
	case state != "open":
		return Credential{}, fmt.Errorf("%w (by device %q)", ErrInvitationUsed, d.invitationUser(ctx, tx, invID))
	case time.Now().UnixMilli() >= expiresAtMS:
		return Credential{}, ErrInvitationExpired
	}

	var devState string
	err = tx.QueryRowContext(ctx,
		`SELECT state FROM devices WHERE device_id = ?`, deviceID).Scan(&devState)
	switch {
	case err == nil && devState == "revoked":
		return Credential{}, ErrDeviceRevoked
	case err == nil:
		return Credential{}, ErrDevicePaired
	case !errors.Is(err, sql.ErrNoRows):
		return Credential{}, fmt.Errorf("registry: pair: read device: %w", err)
	}

	now := time.Now().UnixMilli()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO devices (device_id, state, credential_digest, paired_via, paired_at_ms)
		 VALUES (?, 'active', ?, ?, ?)`,
		deviceID, digest(secret), invID, now); err != nil {
		return Credential{}, fmt.Errorf("registry: pair: register device: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE invitations SET state = 'used', used_by = ? WHERE id = ?`, deviceID, invID); err != nil {
		return Credential{}, fmt.Errorf("registry: pair: consume invitation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Credential{}, fmt.Errorf("registry: pair: commit: %w", err)
	}
	return Credential{DeviceID: deviceID, Secret: secret}, nil
}

func (d *DB) invitationUser(ctx context.Context, tx *sql.Tx, invID string) string {
	var usedBy sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT used_by FROM invitations WHERE id = ?`, invID).Scan(&usedBy); err != nil {
		return ""
	}
	return usedBy.String
}

func (d *DB) Authenticate(ctx context.Context, credentialSecret string) (string, error) {
	var deviceID, state string
	err := d.writer.QueryRowContext(ctx,
		`SELECT device_id, state FROM devices WHERE credential_digest = ?`,
		digest(credentialSecret)).Scan(&deviceID, &state)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", ErrCredentialUnknown
	case err != nil:
		return "", fmt.Errorf("registry: authenticate: %w", err)
	case state != "active":
		return "", fmt.Errorf("%w (device %s)", ErrDeviceRevoked, deviceID)
	}
	return deviceID, nil
}

func (d *DB) Authorize(ctx context.Context, deviceID string) error {
	var state string
	err := d.writer.QueryRowContext(ctx,
		`SELECT state FROM devices WHERE device_id = ?`, deviceID).Scan(&state)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("registry: authorize: %w (device %s)", ErrDeviceUnknown, deviceID)
	case err != nil:
		return fmt.Errorf("registry: authorize: %w", err)
	case state != "active":
		return fmt.Errorf("registry: authorize: %w (device %s)", ErrDeviceRevoked, deviceID)
	}
	return nil
}

func (d *DB) Revoke(ctx context.Context, deviceID string) error {
	res, err := d.writer.ExecContext(ctx,
		`UPDATE devices SET state = 'revoked', revoked_at_ms = ? WHERE device_id = ? AND state = 'active'`,
		time.Now().UnixMilli(), deviceID)
	if err != nil {
		return fmt.Errorf("registry: revoke %s: %w", deviceID, err)
	}
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		return nil
	}
	var state string
	err = d.writer.QueryRowContext(ctx,
		`SELECT state FROM devices WHERE device_id = ?`, deviceID).Scan(&state)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("registry: revoke: %w (device %s)", ErrDeviceUnknown, deviceID)
	case err != nil:
		return fmt.Errorf("registry: revoke %s: %w", deviceID, err)
	case state == "revoked":
		return fmt.Errorf("registry: revoke: %w (device %s)", ErrAlreadyRevoked, deviceID)
	default:
		return fmt.Errorf("registry: revoke %s: device state is %q", deviceID, state)
	}
}

func (d *DB) List(ctx context.Context) ([]DeviceInfo, error) {
	rows, err := d.writer.QueryContext(ctx,
		`SELECT device_id, state, paired_via, paired_at_ms, revoked_at_ms
		 FROM devices ORDER BY paired_at_ms DESC, device_id`)
	if err != nil {
		return nil, fmt.Errorf("registry: list devices: %w", err)
	}
	defer rows.Close()
	var out []DeviceInfo
	for rows.Next() {
		var info DeviceInfo
		var revoked sql.NullInt64
		if err := rows.Scan(&info.DeviceID, &info.State, &info.PairedVia, &info.PairedAtMS, &revoked); err != nil {
			return nil, fmt.Errorf("registry: list devices: %w", err)
		}
		if revoked.Valid {
			info.RevokedAtMS = revoked.Int64
		}
		out = append(out, info)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("registry: list devices: %w", err)
	}
	return out, nil
}

func (d *DB) ListInvitations(ctx context.Context) ([]InvitationInfo, error) {
	rows, err := d.writer.QueryContext(ctx,
		`SELECT id, state, used_by, created_at_ms, expires_at_ms
		 FROM invitations ORDER BY created_at_ms DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("registry: list invitations: %w", err)
	}
	defer rows.Close()
	var out []InvitationInfo
	for rows.Next() {
		var info InvitationInfo
		var usedBy sql.NullString
		if err := rows.Scan(&info.ID, &info.State, &usedBy, &info.CreatedAtMS, &info.ExpiresAtMS); err != nil {
			return nil, fmt.Errorf("registry: list invitations: %w", err)
		}
		info.UsedBy = usedBy.String
		info.Expired = info.State == "open" && time.Now().UnixMilli() >= info.ExpiresAtMS
		out = append(out, info)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("registry: list invitations: %w", err)
	}
	return out, nil
}

func (d *DB) WatchRevocations(ctx context.Context, interval time.Duration, fn func(deviceID string)) error {
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	seen, err := d.revokedSet(ctx)
	if err != nil {
		return err
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			ids, err := d.revokedSet(ctx)
			if err != nil {
				continue
			}
			for id := range ids {
				if !seen[id] {
					seen[id] = true
					fn(id)
				}
			}
		}
	}
}

func (d *DB) revokedSet(ctx context.Context) (map[string]bool, error) {
	rows, err := d.writer.QueryContext(ctx,
		`SELECT device_id FROM devices WHERE state = 'revoked'`)
	if err != nil {
		return nil, fmt.Errorf("registry: watch revocations: %w", err)
	}
	defer rows.Close()
	seen := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("registry: watch revocations: %w", err)
		}
		seen[id] = true
	}
	return seen, rows.Err()
}

func newSecret() (string, error) {
	var b [secretBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("registry: generate secret: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func digest(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func dsn(path string) string {
	return "file:" + filepath.ToSlash(path) +
		"?_journal_mode=WAL&_synchronous=FULL&_foreign_keys=on&_busy_timeout=5000&_txlock=immediate"
}
