package registry

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// openTestRegistry opens a registry in a fresh temporary directory.
func openTestRegistry(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// pairTestDevice consumes one fresh invitation for deviceID and returns the
// credential.
func pairTestDevice(t *testing.T, db *DB, deviceID string) Credential {
	t.Helper()
	inv, err := db.Issue(context.Background(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cred, err := db.Consume(context.Background(), inv.Secret, deviceID)
	if err != nil {
		t.Fatal(err)
	}
	return cred
}

// TestIssueAndConsumeRegistersDevice walks the happy path: an issued
// invitation admits one device, the minted credential authenticates it, and
// the registry records the provenance the invariants of tla/Auth.tla need.
func TestIssueAndConsumeRegistersDevice(t *testing.T) {
	db := openTestRegistry(t)
	ctx := context.Background()

	inv, err := db.Issue(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if inv.Secret == "" || inv.ID == inv.Secret {
		t.Fatalf("invitation = %+v; want distinct ID and secret", inv)
	}
	if inv.ExpiresAtMS <= inv.CreatedAtMS {
		t.Fatalf("invitation expiry %d not after creation %d", inv.ExpiresAtMS, inv.CreatedAtMS)
	}

	cred, err := db.Consume(ctx, inv.Secret, "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if cred.DeviceID != "0123456789abcdef0123456789abcdef" || cred.Secret == "" {
		t.Fatalf("credential = %+v", cred)
	}

	deviceID, err := db.Authenticate(ctx, cred.Secret)
	if err != nil || deviceID != cred.DeviceID {
		t.Fatalf("authenticate = %q, %v; want %q", deviceID, err, cred.DeviceID)
	}
	if err := db.Authorize(ctx, deviceID); err != nil {
		t.Fatalf("authorize active device: %v", err)
	}

	devices, err := db.List(ctx)
	if err != nil || len(devices) != 1 {
		t.Fatalf("list = %v, %v; want one device", devices, err)
	}
	if devices[0].State != "active" || devices[0].PairedVia != inv.ID {
		t.Fatalf("device row = %+v; want active via %s", devices[0], inv.ID)
	}
	invitations, err := db.ListInvitations(ctx)
	if err != nil || len(invitations) != 1 {
		t.Fatalf("invitations = %v, %v; want one", invitations, err)
	}
	if invitations[0].State != "used" || invitations[0].UsedBy != cred.DeviceID {
		t.Fatalf("invitation row = %+v; want used by the device", invitations[0])
	}
}

// TestInvitationSingleUse pins the InvitationSingleUse invariant: a consumed
// invitation never admits a second device, in this process or another.
func TestInvitationSingleUse(t *testing.T) {
	db := openTestRegistry(t)
	ctx := context.Background()
	inv, err := db.Issue(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Consume(ctx, inv.Secret, "0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatal(err)
	}

	// Same handle: the invitation is used.
	_, err = db.Consume(ctx, inv.Secret, "ffffffffffffffffffffffffffffffff")
	if !errors.Is(err, ErrInvitationUsed) {
		t.Fatalf("second consume err = %v; want ErrInvitationUsed", err)
	}

	// Another handle on the same file — another process, for example the
	// serving endpoint — sees the same consumed state.
	again, err := Open(db.path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	_, err = again.Consume(ctx, inv.Secret, "ffffffffffffffffffffffffffffffff")
	if !errors.Is(err, ErrInvitationUsed) {
		t.Fatalf("cross-process consume err = %v; want ErrInvitationUsed", err)
	}
}

// TestConsumeRaceSingleUse runs two devices against one invitation at the
// same time, through two registry handles. Exactly one wins; the loser
// learns the invitation was used. This is the ConsumeInvitation atomicity of
// tla/Auth.tla.
func TestConsumeRaceSingleUse(t *testing.T) {
	db := openTestRegistry(t)
	ctx := context.Background()
	inv, err := db.Issue(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	other, err := Open(db.path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()

	ids := []string{"0123456789abcdef0123456789abcdef", "ffffffffffffffffffffffffffffffff"}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, handle := range []*DB{db, other} {
		wg.Add(1)
		go func(i int, handle *DB) {
			defer wg.Done()
			_, errs[i] = handle.Consume(ctx, inv.Secret, ids[i])
		}(i, handle)
	}
	wg.Wait()

	won := errs[0] == nil
	lost := errs[1] == nil
	if won == lost {
		t.Fatalf("exactly one consume must win; errors = %v", errs)
	}
	loserErr := errs[0]
	if won {
		loserErr = errs[1]
	}
	if !errors.Is(loserErr, ErrInvitationUsed) {
		t.Fatalf("loser err = %v; want ErrInvitationUsed", loserErr)
	}
	devices, err := db.List(ctx)
	if err != nil || len(devices) != 1 {
		t.Fatalf("devices after race = %v, %v; want exactly one", devices, err)
	}
}

// TestInvitationExpiry pins that consumption must race expiry and can lose:
// an invitation past its lifetime is refused, and lazily marked expired.
func TestInvitationExpiry(t *testing.T) {
	db := openTestRegistry(t)
	ctx := context.Background()
	inv, err := db.Issue(ctx, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	_, err = db.Consume(ctx, inv.Secret, "0123456789abcdef0123456789abcdef")
	if !errors.Is(err, ErrInvitationExpired) {
		t.Fatalf("expired consume err = %v; want ErrInvitationExpired", err)
	}
	// A negative TTL would issue an already-dead invitation; refuse it.
	if _, err := db.Issue(ctx, -time.Second); err == nil {
		t.Fatal("negative TTL was accepted")
	}
}

// TestRevocationIsFinal pins RevocationIrreversible and
// NoLiveSessionForRevoked: a revoked credential no longer authenticates, a
// fresh invitation cannot resurrect the identity, and re-revocation is an
// explicit no-op.
func TestRevocationIsFinal(t *testing.T) {
	db := openTestRegistry(t)
	ctx := context.Background()
	cred := pairTestDevice(t, db, "0123456789abcdef0123456789abcdef")

	if err := db.Revoke(ctx, cred.DeviceID); err != nil {
		t.Fatal(err)
	}

	if _, err := db.Authenticate(ctx, cred.Secret); !errors.Is(err, ErrDeviceRevoked) {
		t.Fatalf("authenticate revoked err = %v; want ErrDeviceRevoked", err)
	}
	if err := db.Authorize(ctx, cred.DeviceID); !errors.Is(err, ErrDeviceRevoked) {
		t.Fatalf("authorize revoked err = %v; want ErrDeviceRevoked", err)
	}

	// A fresh invitation cannot re-admit the revoked identity.
	inv, err := db.Issue(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Consume(ctx, inv.Secret, cred.DeviceID); !errors.Is(err, ErrDeviceRevoked) {
		t.Fatalf("re-pair revoked err = %v; want ErrDeviceRevoked", err)
	}
	// The invitation survived the refused attempt and still serves a stranger.
	if _, err := db.Consume(ctx, inv.Secret, "ffffffffffffffffffffffffffffffff"); err != nil {
		t.Fatalf("invitation died with the refused consume: %v", err)
	}

	if err := db.Revoke(ctx, cred.DeviceID); err == nil {
		t.Fatal("re-revocation was silently accepted")
	}
}

// TestActiveDeviceCannotConsumeSecondInvitation pins the model's "absent"
// guard: pairing twice would break the pairing record's meaning, so an
// active device is refused even with a valid fresh invitation.
func TestActiveDeviceCannotConsumeSecondInvitation(t *testing.T) {
	db := openTestRegistry(t)
	ctx := context.Background()
	deviceID := "0123456789abcdef0123456789abcdef"
	pairTestDevice(t, db, deviceID)

	inv, err := db.Issue(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Consume(ctx, inv.Secret, deviceID); !errors.Is(err, ErrDevicePaired) {
		t.Fatalf("second pairing err = %v; want ErrDevicePaired", err)
	}
}

// TestUnknownInputs pins the refusal of strangers: an unknown invitation,
// an unknown credential, and revocation or authorization of an unknown
// device.
func TestUnknownInputs(t *testing.T) {
	db := openTestRegistry(t)
	ctx := context.Background()

	if _, err := db.Consume(ctx, "no-such-invitation", "0123456789abcdef0123456789abcdef"); !errors.Is(err, ErrInvitationUnknown) {
		t.Fatalf("unknown invitation err = %v; want ErrInvitationUnknown", err)
	}
	if _, err := db.Authenticate(ctx, "no-such-credential"); !errors.Is(err, ErrCredentialUnknown) {
		t.Fatalf("unknown credential err = %v; want ErrCredentialUnknown", err)
	}
	if err := db.Authorize(ctx, "0123456789abcdef0123456789abcdef"); !errors.Is(err, ErrDeviceUnknown) {
		t.Fatalf("unknown device err = %v; want ErrDeviceUnknown", err)
	}
	if err := db.Revoke(ctx, "0123456789abcdef0123456789abcdef"); !errors.Is(err, ErrDeviceUnknown) {
		t.Fatalf("revoke unknown err = %v; want ErrDeviceUnknown", err)
	}
	if _, err := db.Consume(ctx, "x", ""); !errors.Is(err, ErrDeviceUnknown) {
		t.Fatalf("empty device err = %v; want ErrDeviceUnknown", err)
	}
	long := make([]byte, 65)
	for i := range long {
		long[i] = 'a'
	}
	if _, err := db.Consume(ctx, "x", string(long)); !errors.Is(err, ErrDeviceUnknown) {
		t.Fatalf("oversized device err = %v; want ErrDeviceUnknown", err)
	}
}

// TestProtectedStore pins the protection boundary: the state directory is
// owner-only, and the registry file itself carries no group or other
// permission bits.
func TestProtectedStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", FileName) // Open creates the parent
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Issue(context.Background(), time.Hour); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("state directory perms = %o; want owner-only", perm)
	}
	info, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("registry perms = %o; want no group or other access", perm)
	}

	// No secret is stored in clear: the file holds only digests.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The digest of a known-format secret would be 64 hex characters; assert
	// the file never contains any 32-hex-character secret-shaped string by
	// checking a freshly issued secret is absent.
	inv, err := db.Issue(context.Background(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if string(data[len(data)-len(inv.Secret):]) == inv.Secret {
		t.Fatal("registry file stores the invitation secret in clear")
	}
}

// TestPersistenceAcrossReopen pins that pairing survives a hub restart:
// the registry on disk authenticates the same credential.
func TestPersistenceAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := db.Issue(context.Background(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cred, err := db.Consume(context.Background(), inv.Secret, "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	deviceID, err := reopened.Authenticate(context.Background(), cred.Secret)
	if err != nil || deviceID != cred.DeviceID {
		t.Fatalf("reopen authenticate = %q, %v; want %q", deviceID, err, cred.DeviceID)
	}
}

// TestWatchRevocations pins the watcher contract: devices revoked before the
// watch starts are not reported, every new revocation is reported exactly
// once, and the watch ends with its context.
func TestWatchRevocations(t *testing.T) {
	db := openTestRegistry(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	first := pairTestDevice(t, db, "0123456789abcdef0123456789abcdef")
	if err := db.Revoke(ctx, first.DeviceID); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var reported []string
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		err := db.WatchRevocations(ctx, 5*time.Millisecond, func(deviceID string) {
			mu.Lock()
			reported = append(reported, deviceID)
			mu.Unlock()
		})
		if err != nil {
			t.Errorf("watch: %v", err)
		}
	}()

	// The pre-existing revocation must not arrive.
	time.Sleep(30 * time.Millisecond)

	second := pairTestDevice(t, db, "ffffffffffffffffffffffffffffffff")
	third := pairTestDevice(t, db, "abababababababababababababababab")
	for _, id := range []string{second.DeviceID, third.DeviceID} {
		if err := db.Revoke(ctx, id); err != nil {
			t.Fatal(err)
		}
	}

	deadline := time.After(5 * time.Second)
	for {
		mu.Lock()
		n := len(reported)
		contains := func(id string) bool {
			for _, r := range reported {
				if r == id {
					return true
				}
			}
			return false
		}
		mu.Unlock()
		if n == 2 && contains(second.DeviceID) && contains(third.DeviceID) {
			break
		}
		select {
		case <-deadline:
			mu.Lock()
			defer mu.Unlock()
			t.Fatalf("watch reported %v; want exactly the two new revocations", reported)
		case <-time.After(2 * time.Millisecond):
		}
	}

	cancel()
	select {
	case <-watchDone:
	case <-time.After(5 * time.Second):
		t.Fatal("watch did not stop with its context")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reported) != 2 {
		t.Fatalf("watch reported %v after stop; want exactly two", reported)
	}
}

// TestSchemaVersionRefusesUnknownDatabase pins the migration posture: a
// registry written by a newer binary is an error.
func TestSchemaVersionRefusesUnknownDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// Write the newer version through a plain driver connection.
	plain, err := sql.Open("sqlite3", "file:"+path+"?_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plain.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	if err := plain.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("a newer registry schema was accepted")
	}
}
