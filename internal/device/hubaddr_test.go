package device

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHubAddressRoundTrip(t *testing.T) {
	dir := t.TempDir()
	addr := "tcexamplehubaddressvalue0123"

	if _, err := LoadHubAddress(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("unpaired load err = %v; want fs.ErrNotExist", err)
	}
	if err := SaveHubAddress(dir, addr); err != nil {
		t.Fatal(err)
	}
	stored, err := LoadHubAddress(dir)
	if err != nil || stored != addr {
		t.Fatalf("stored = %q, %v; want %q", stored, err, addr)
	}
	if err := SaveHubAddress(dir, "  "+addr+" \n"); err != nil {
		t.Fatal(err)
	}
	if stored, err := LoadHubAddress(dir); err != nil || stored != addr {
		t.Fatalf("trimmed stored = %q, %v; want %q", stored, err, addr)
	}
	info, err := os.Stat(filepath.Join(dir, HubAddressFileName))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("hub address perms = %o; want owner-only", perm)
	}
}

func TestHubAddressRefusesInvalidInput(t *testing.T) {
	dir := t.TempDir()
	if err := SaveHubAddress(dir, "   "); err == nil {
		t.Fatal("a blank hub address was saved")
	}
	if err := SaveHubAddress(dir, strings.Repeat("t", maxHubAddressBytes+1)); err == nil {
		t.Fatal("an oversized hub address was saved")
	}
	path := filepath.Join(dir, HubAddressFileName)
	for name, content := range map[string]string{
		"not json":      "}",
		"wrong version": `{"version":99,"address":"tcexample"}`,
		"missing value": `{"version":1}`,
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadHubAddress(dir); !errors.Is(err, ErrHubAddressUnusable) {
			t.Fatalf("%s: load err = %v; want ErrHubAddressUnusable", name, err)
		}
	}
}
