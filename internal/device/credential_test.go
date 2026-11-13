package device

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCredentialRoundTrip(t *testing.T) {
	dir := t.TempDir()
	id, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	cred := Credential{DeviceID: id, Secret: "0123456789abcdef0123456789abcdef"}

	// Before pairing, LoadCredential reports the missing file plainly.
	if _, err := LoadCredential(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("unpaired load err = %v; want fs.ErrNotExist", err)
	}

	if err := SaveCredential(dir, cred); err != nil {
		t.Fatal(err)
	}
	stored, err := LoadCredential(dir)
	if err != nil || stored != cred {
		t.Fatalf("stored = %+v, %v; want %+v", stored, err, cred)
	}

	// The file is owner-only, like the identity file.
	info, err := os.Stat(filepath.Join(dir, CredentialFileName))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("credential perms = %o; want owner-only", perm)
	}

	// Saving again replaces atomically; the new secret wins.
	next := Credential{DeviceID: id, Secret: "ffffffffffffffffffffffffffffffff"}
	if err := SaveCredential(dir, next); err != nil {
		t.Fatal(err)
	}
	if stored, err := LoadCredential(dir); err != nil || stored != next {
		t.Fatalf("replaced credential = %+v, %v; want %+v", stored, err, next)
	}
}

func TestCredentialRefusesInvalidInput(t *testing.T) {
	dir := t.TempDir()
	if err := SaveCredential(dir, Credential{DeviceID: "not-an-id", Secret: "s"}); err == nil {
		t.Fatal("a credential without a valid device identity was saved")
	}
	if err := SaveCredential(dir, Credential{DeviceID: "0123456789abcdef0123456789abcdef"}); err == nil {
		t.Fatal("a credential without a secret was saved")
	}
	if err := SaveCredential(dir, Credential{
		DeviceID: "0123456789abcdef0123456789abcdef",
		Secret:   strings.Repeat("s", maxSecretLen+1)}); err == nil {
		t.Fatal("an oversized secret was saved")
	}
}

// TestCredentialUnusableNeverRepairs pins the recovery posture: a present
// but broken credential file is an error, because re-pairing the same
// identity would be refused by the hub anyway (its registry already holds
// the identity as paired).
func TestCredentialUnusableNeverRepairs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, CredentialFileName)
	for name, content := range map[string]string{
		"not json":       "}",
		"wrong version":  `{"version":99,"device_id":"0123456789abcdef0123456789abcdef","secret":"s"}`,
		"missing secret": `{"version":1,"device_id":"0123456789abcdef0123456789abcdef"}`,
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadCredential(dir); !errors.Is(err, ErrCredentialUnusable) {
			t.Fatalf("%s: load err = %v; want ErrCredentialUnusable", name, err)
		}
	}
}
