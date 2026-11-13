// Package registry owns the hub's device credential registry: the
// authorization store of tla/Auth.tla, separate from the queryable transcript
// database. It records one-time expiring pairing invitations, the credentials
// of paired devices, and revocation, and answers the two questions the sync
// endpoint asks on every request: which device does this credential belong
// to, and is that device still active.
//
// The registry is its own SQLite database (WAL, synchronous FULL, one writer
// connection) living in a protected state directory; the transcript database
// and its sql MCP tool can never read it. Secrets are 128-bit random values
// from crypto/rand stored as SHA-256 digests — standard primitives only, no
// new cryptography. Revocation is final per device identity: a revoked device
// cannot consume a fresh invitation, so re-admission means a new device
// identity.
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

// FileName is the registry database file name inside the state directory.
const FileName = "registry.db"

// schemaVersion is the registry schema version, recorded in the database
// header through PRAGMA user_version. A database written by a newer version
// is an error, never a silent upgrade.
const schemaVersion = 1

// DefaultInvitationTTL bounds how long a fresh invitation stays consumable.
const DefaultInvitationTTL = 15 * time.Minute

// DefaultPollInterval is how often WatchRevocations looks for newly revoked
// devices when no interval is given.
const DefaultPollInterval = time.Second

// secretBytes is the entropy of an invitation or credential secret.
// Hex-encoded it is 32 characters.
const secretBytes = 16

// Errors reported by registry operations. They name the protocol reason, so
// the endpoint and the CLI can tell a stranger, an expired invitation, and a
// revoked device apart.
var (
	// ErrInvitationUnknown marks a presented invitation the registry never
	// issued.
	ErrInvitationUnknown = errors.New("registry: invitation is not valid")
	// ErrInvitationUsed marks a consumed invitation; single-use is an
	// invariant of tla/Auth.tla.
	ErrInvitationUsed = errors.New("registry: invitation was already used")
	// ErrInvitationExpired marks an invitation whose lifetime ended before
	// consumption.
	ErrInvitationExpired = errors.New("registry: invitation expired")
	// ErrDevicePaired marks an already-active device trying to consume a
	// second invitation.
	ErrDevicePaired = errors.New("registry: device is already paired")
	// ErrDeviceRevoked marks a revoked device. Revocation is final for the
	// identity: enroll a new device identity instead.
	ErrDeviceRevoked = errors.New("registry: device is revoked; re-admission needs a new device identity")
	// ErrDeviceUnknown marks a device identity the registry never paired.
	ErrDeviceUnknown = errors.New("registry: device is not paired")
	// ErrCredentialUnknown marks a presented credential no active or revoked
	// device holds.
	ErrCredentialUnknown = errors.New("registry: credential does not belong to a paired device")
)

// Invitation is one pairing invitation: a one-time, expiring admission ticket
// the operator hands to one device. Secret is shown once at issue time and
// never stored in clear.
type Invitation struct {
	ID          string
	Secret      string
	CreatedAtMS int64
	ExpiresAtMS int64
}

// Credential is the hub-minted secret of one paired device. The device keeps
// it in a protected local file (internal/device) and presents it to
// authenticate every connection.
type Credential struct {
	DeviceID string
	Secret   string
}

// DeviceInfo is one registry row as List reports it.
type DeviceInfo struct {
	DeviceID    string
	State       string // "active" or "revoked"
	PairedVia   string
	PairedAtMS  int64
	RevokedAtMS int64 // zero while active
}

// InvitationInfo is one invitation row as ListInvitations reports it. Expired
// reports the derived expiry of an unconsumed invitation.
type InvitationInfo struct {
	ID          string
	State       string // "open" or "used"
	UsedBy      string // device ID when used
	CreatedAtMS int64
	ExpiresAtMS int64
	Expired     bool
}

// DB is the hub-owned device credential registry.
type DB struct {
	path   string
	writer *sql.DB
}

// Open opens (creating if needed) the registry at path with a single writer
// connection configured for WAL journaling, FULL synchronous durability and a
// busy timeout, then ensures the schema. The parent directory is created with
// owner-only permissions; it is the protection boundary for the registry and
// its write-ahead log.
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
	// Exactly one connection: registry operations serialize on it and cannot
	// interleave, in this process and across processes through the busy
	// timeout.
	writer.SetMaxOpenConns(1)
	writer.SetMaxIdleConns(1)
	db := &DB{path: abs, writer: writer}
	if err := db.ensureSchema(context.Background()); err != nil {
		_ = writer.Close()
		return nil, err
	}
	// The database file itself is owner-only too; the write-ahead log files
	// stay protected by the owner-only directory.
	_ = os.Chmod(abs, 0o600)
	return db, nil
}

// Close closes the registry.
func (d *DB) Close() error { return d.writer.Close() }

// ensureSchema creates the registry tables on first use and pins the schema
// version. A database holding any other version is an error: never a silent
// upgrade or downgrade.
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
	defer tx.Rollback() // no-op after commit
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

// tablesExist reports whether any registry table exists, so a database file
// with tables but no recorded version is refused instead of recreated.
func (d *DB) tablesExist(ctx context.Context) bool {
	var n int
	err := d.writer.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name IN ('invitations', 'devices')`).
		Scan(&n)
	return err == nil && n > 0
}

// Issue creates one open invitation that expires after ttl, which must be
// positive. It returns the invitation with its secret; only the SHA-256
// digest of the secret is stored, so the secret is shown exactly once, to
// the operator.
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

// Consume implements ConsumeInvitation of tla/Auth.tla: one atomic
// transaction consumes the invitation and admits the device, so no window
// exists where the invitation is used but the device is not registered, or
// the other way around. Only a stranger device — one the registry never
// paired — may consume an invitation: a revoked identity stays revoked, and
// an active one needs no second pairing. On success it returns the
// hub-minted credential for the device to store in its protected credential
// file.
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
	defer tx.Rollback() // no-op after commit

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

// invitationUser reports which device consumed an invitation, for a clear
// single-use diagnostic. A missing row reports the empty string.
func (d *DB) invitationUser(ctx context.Context, tx *sql.Tx, invID string) string {
	var usedBy sql.NullString
	if err := tx.QueryRowContext(ctx,
		`SELECT used_by FROM invitations WHERE id = ?`, invID).Scan(&usedBy); err != nil {
		return ""
	}
	return usedBy.String
}

// Authenticate maps one presented credential to its active device. Unknown
// credentials and revoked devices are both refused; the errors say which.
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

// Authorize is the per-request registry check every request applies
// (Authorized of tla/Auth.tla): it reports an error unless the device is
// currently active. The endpoint calls it before processing each frame.
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

// Revoke implements RevokeDevice of tla/Auth.tla: the registry flips to
// revoked, one-way for the device identity. Closing the device's connections
// and cancelling its in-flight requests is the endpoint's part, driven by
// WatchRevocations; already returned data cannot be revoked, and the device's
// sources and annotation rows stay as history.
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
	// No row moved: name the reason instead of a silent no-op.
	var state string
	err = d.writer.QueryRowContext(ctx,
		`SELECT state FROM devices WHERE device_id = ?`, deviceID).Scan(&state)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("registry: revoke: %w (device %s)", ErrDeviceUnknown, deviceID)
	case err != nil:
		return fmt.Errorf("registry: revoke %s: %w", deviceID, err)
	case state == "revoked":
		return fmt.Errorf("registry: revoke: device %s is already revoked", deviceID)
	default:
		return fmt.Errorf("registry: revoke %s: device state is %q", deviceID, state)
	}
}

// List reports every paired device, active and revoked, newest pairing
// first.
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

// ListInvitations reports every invitation, newest first, with the derived
// expiry of unconsumed ones.
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

// WatchRevocations reports each device as it becomes revoked, until ctx is
// done, so the endpoint can close that device's connections and cancel its
// in-flight requests. Devices revoked before the call are not reported: they
// cannot hold connections, because their credentials no longer authenticate.
// The revocation may come from any process sharing the registry file — the
// operator CLI, for example — so the watch polls the registry; interval
// values <= 0 select DefaultPollInterval.
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
				continue // a transient read failure must not stop the watch
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

// revokedSet reads the device IDs currently revoked.
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

// newSecret returns a fresh 128-bit secret, hex-encoded.
func newSecret() (string, error) {
	var b [secretBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("registry: generate secret: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// digest returns the hex SHA-256 of a secret. The registry stores secrets
// only in this form.
func digest(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// dsn builds the registry's SQLite URI: WAL journaling, FULL synchronous
// durability, foreign keys, a busy timeout, and immediate transactions so a
// racing consume serializes on the write lock instead of deadlocking on the
// upgrade from a read lock.
func dsn(path string) string {
	return "file:" + filepath.ToSlash(path) +
		"?_journal_mode=WAL&_synchronous=FULL&_foreign_keys=on&_busy_timeout=5000&_txlock=immediate"
}
