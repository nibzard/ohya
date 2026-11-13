package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nibzard/ohya/internal/registry"
)

// runDevices runs one devices subcommand against a temporary state
// directory and returns stdout plus the error. The flags follow the
// subcommand, as runDevicesCommand expects.
func runDevices(t *testing.T, stateDir string, args ...string) (string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	argv := append([]string{args[0], "--state", stateDir}, args[1:]...)
	err := runDevicesCommand(argv, &stdout, &stderr)
	return stdout.String(), err
}

func TestDevicesLifecycle(t *testing.T) {
	state := t.TempDir()

	// An empty registry lists nothing.
	out, err := runDevices(t, state, "list")
	if err != nil || !strings.Contains(out, "(none paired)") {
		t.Fatalf("empty list = %q, %v", out, err)
	}

	// Issue an invitation; its code is the first stdout line.
	out, err = runDevices(t, state, "invite", "--ttl", "1h")
	if err != nil {
		t.Fatal(err)
	}
	code := strings.SplitN(strings.TrimSpace(out), "\n", 2)[0]
	if len(code) < 16 || strings.ContainsAny(code, " \t") {
		t.Fatalf("invitation code = %q; want opaque single-line secret", code)
	}

	// A device consumes it through the registry — the same transaction the
	// endpoint's pairing frame drives.
	reg, err := registry.Open(filepath.Join(state, registry.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	deviceID := "0123456789abcdef0123456789abcdef"
	if _, err := reg.Consume(context.Background(), code, deviceID); err != nil {
		t.Fatal(err)
	}

	out, err = runDevices(t, state, "list")
	if err != nil || !strings.Contains(out, deviceID) || !strings.Contains(out, "active") {
		t.Fatalf("list after pairing = %q, %v; want the active device", out, err)
	}
	if !strings.Contains(out, "used by "+deviceID) {
		t.Fatalf("list invitations = %q; want the used invitation to name the device", out)
	}

	// Revocation is final and visible.
	if _, err := runDevices(t, state, "revoke", deviceID); err != nil {
		t.Fatal(err)
	}
	out, err = runDevices(t, state, "list")
	if err != nil || !strings.Contains(out, "revoked") {
		t.Fatalf("list after revoke = %q, %v; want the revoked state", out, err)
	}
	_, err = runDevices(t, state, "revoke", deviceID)
	if err == nil || !strings.Contains(err.Error(), "already revoked") {
		t.Fatalf("re-revoke err = %v; want already-revoked", err)
	}

	// Stray input is refused.
	_, err = runDevices(t, state, "revoke")
	if err == nil {
		t.Fatal("revoke without a device ID succeeded")
	}
	_, err = runDevices(t, state, "invite", "--ttl", "-1m")
	if err == nil {
		t.Fatal("a negative TTL was accepted")
	}
}

// TestDevicesRegistryIsSeparate pins that the devices command never touches
// the transcript database: the registry file is its own database.
func TestDevicesRegistryIsSeparate(t *testing.T) {
	state := t.TempDir()
	if _, err := runDevices(t, state, "invite"); err != nil {
		t.Fatal(err)
	}
	entries, err := filepath.Glob(filepath.Join(state, "*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if base := filepath.Base(e); base == "hub.db" {
			t.Fatal("the devices command created a transcript database")
		}
	}
	if _, err := runDevices(t, state, "nosuch"); err == nil {
		t.Fatal("an unknown devices subcommand was accepted")
	}
}
