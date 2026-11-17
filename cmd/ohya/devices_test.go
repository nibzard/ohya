package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tailscale/tailcat"
	"tailscale.com/types/key"

	"github.com/nibzard/ohya/internal/registry"
	"github.com/nibzard/ohya/internal/transport/address"
)

func writeHubIdentity(t *testing.T, stateDir string) string {
	t.Helper()
	nodeText, err := key.NewNode().MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	pskText, err := tailcat.NewPresharedKey().MarshalText()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(stateDir, "tailcat-hub")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	identity := fmt.Sprintf(`{"version":1,"node":%q,"preshared_key":%q,"region":{"RegionID":1,"RegionCode":"ohyatest","RegionName":"ohya local test relay","Nodes":[{"Name":"ohyatest1","RegionID":1,"HostName":"127.0.0.1","IPv4":"127.0.0.1","IPv6":"none","DERPPort":443,"InsecureForTests":true}]}}`,
		string(nodeText), string(pskText))
	if err := os.WriteFile(filepath.Join(dir, "identity.json"), []byte(identity), 0o600); err != nil {
		t.Fatal(err)
	}
	hubAddr, err := address.LoadHubAddr(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	return hubAddr
}

func runDevices(t *testing.T, stateDir string, args ...string) (string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	argv := append([]string{args[0], "--state", stateDir}, args[1:]...)
	err := runDevicesCommand(argv, &stdout, &stderr)
	return stdout.String(), err
}

func TestDevicesLifecycle(t *testing.T) {
	state := t.TempDir()
	writeHubIdentity(t, state)

	out, err := runDevices(t, state, "list")
	if err != nil || !strings.Contains(out, "(none paired)") {
		t.Fatalf("empty list = %q, %v", out, err)
	}

	fresh := t.TempDir()
	if _, err := runDevices(t, fresh, "invite"); err == nil ||
		!strings.Contains(err.Error(), "run ohya serve once") {
		t.Fatalf("invite without a hub identity err = %v; want the serve remedy", err)
	}

	out, err = runDevices(t, state, "invite", "--ttl", "1h")
	if err != nil {
		t.Fatal(err)
	}
	code := strings.SplitN(strings.TrimSpace(out), "\n", 2)[0]
	invitation, hubAddr, err := parseJoinCode(code)
	if err != nil {
		t.Fatalf("join code %q: %v", code, err)
	}
	if len(invitation) < 16 || !strings.HasPrefix(hubAddr, "tc") || strings.ContainsAny(code, " \t") {
		t.Fatalf("join code = %q; want one opaque single-line ohya1 code", code)
	}

	reg, err := registry.Open(filepath.Join(state, registry.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	deviceID := "0123456789abcdef0123456789abcdef"
	if _, err := reg.Consume(context.Background(), invitation, deviceID); err != nil {
		t.Fatal(err)
	}

	out, err = runDevices(t, state, "list")
	if err != nil || !strings.Contains(out, deviceID) || !strings.Contains(out, "active") {
		t.Fatalf("list after pairing = %q, %v; want the active device", out, err)
	}
	if !strings.Contains(out, "used by "+deviceID) {
		t.Fatalf("list invitations = %q; want the used invitation to name the device", out)
	}

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

	_, err = runDevices(t, state, "revoke")
	if err == nil {
		t.Fatal("revoke without a device ID succeeded")
	}
	_, err = runDevices(t, state, "invite", "--ttl", "-1m")
	if err == nil {
		t.Fatal("a negative TTL was accepted")
	}
}

func TestDevicesRegistryIsSeparate(t *testing.T) {
	state := t.TempDir()
	writeHubIdentity(t, state)
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

func TestDevicesListLastSync(t *testing.T) {
	state := t.TempDir()
	writeHubIdentity(t, state)
	reg, err := registry.Open(filepath.Join(state, registry.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	ctx := context.Background()
	inv, err := reg.Issue(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	neverID := "0123456789abcdef0123456789abcdef"
	seenID := "0123456789abcdef0123456789ffff"
	if _, err := reg.Consume(ctx, inv.Secret, neverID); err != nil {
		t.Fatal(err)
	}
	inv2, err := reg.Issue(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Consume(ctx, inv2.Secret, seenID); err != nil {
		t.Fatal(err)
	}
	at := time.UnixMilli(1765400000000)
	if err := reg.RecordSync(ctx, seenID, address.PathRelay, at.UnixMilli()); err != nil {
		t.Fatal(err)
	}

	out, err := runDevices(t, state, "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "last sync "+at.UTC().Format(time.RFC3339)+" via relay") {
		t.Fatalf("list misses the last sync line:\n%s", out)
	}
	if !strings.Contains(out, neverID+"  active  paired ") || !strings.Contains(out, ", never synced") {
		t.Fatalf("list misses the never-synced device:\n%s", out)
	}

	var stdout bytes.Buffer
	if err := runDevicesCommand([]string{"list", "--state", state, "--json"}, &stdout, os.Stderr); err != nil {
		t.Fatal(err)
	}
	env := decodeEnvelope(t, stdout.String())
	var data listJSONData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatal(err)
	}
	byID := map[string]deviceJSON{}
	for _, d := range data.Devices {
		byID[d.ID] = d
	}
	if d := byID[seenID]; d.LastPath == nil || *d.LastPath != address.PathRelay ||
		d.LastSyncAt == nil || *d.LastSyncAt != at.UTC().Format(time.RFC3339) {
		t.Fatalf("synced device JSON = %+v; want relay path and the recorded time", d)
	}
	if d := byID[neverID]; d.LastPath != nil || d.LastSyncAt != nil {
		t.Fatalf("never-synced device JSON = %+v; want null path and sync time", d)
	}
}
