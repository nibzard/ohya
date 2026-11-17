package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/nibzard/ohya/internal/device"
	"github.com/nibzard/ohya/internal/registry"
)

func skipUnlessTransportE2E(t *testing.T) {
	t.Helper()
	if os.Getenv("CI") != "" && runtime.GOOS == "darwin" {
		t.Skip("darwin CI: the pinned transport's macOS runtime behavior is unverified (backlog T046)")
	}
	if testing.Short() {
		t.Skip("end-to-end stack test")
	}
}

func invitationStates(t *testing.T, hubState string) []invitationJSON {
	t.Helper()
	var out bytes.Buffer
	if err := runDevicesList([]string{"--state", hubState, "--json"}, &out, os.Stderr); err != nil {
		t.Fatal(err)
	}
	var listed struct {
		Data struct {
			Invitations []invitationJSON `json:"invitations"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	return listed.Data.Invitations
}

func countOpenInvitations(t *testing.T, hubState string) int {
	t.Helper()
	open := 0
	for _, inv := range invitationStates(t, hubState) {
		if inv.State == "open" {
			open++
		}
	}
	return open
}

func runPairOnly(t *testing.T, stateDir, code string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	return runJoin(ctx, joinOptions{pairOnly: true, stateDir: stateDir, code: code, stderr: os.Stderr})
}

func TestJoinOldCodeFormatRejected(t *testing.T) {
	state := t.TempDir()
	err := runPairOnly(t, state, "0123456789abcdef0123456789abcdef:preauth0123456789")
	if err == nil {
		t.Fatal("an old-format code was accepted")
	}
	if !strings.Contains(err.Error(), "ohya1") || !strings.Contains(err.Error(), "ohya devices invite") {
		t.Fatalf("err = %v; want the ohya1 format and the invite remedy", err)
	}
	if _, err := os.Stat(filepath.Join(state, device.CredentialFileName)); !os.IsNotExist(err) {
		t.Fatal("a credential was written for an old-format code")
	}
}

func TestJoinExpiredInvitationNamesRemedy(t *testing.T) {
	skipUnlessTransportE2E(t)
	dir := transportTestDir(t)
	hubState := filepath.Join(dir, "hub-state")
	hubDB := filepath.Join(dir, "hub", "hub.db")
	devState := filepath.Join(dir, "dev-state")
	regionFile := writeDERPRegionFile(t, startLocalDERP(t))
	serveCmd, serveLog, _, _ := startServeChild(t, hubDB, hubState, regionFile)
	defer stopChild(t, serveCmd, serveLog, 30*time.Second)

	var out bytes.Buffer
	if err := runDevicesInvite([]string{"--state", hubState, "--ttl", "1ms"}, &out, os.Stderr); err != nil {
		t.Fatal(err)
	}
	code := strings.SplitN(strings.TrimSpace(out.String()), "\n", 2)[0]
	time.Sleep(20 * time.Millisecond)
	err := runPairOnly(t, devState, code)
	if err == nil {
		t.Fatal("an expired invitation was accepted")
	}
	if !strings.Contains(err.Error(), "ohya devices invite") {
		t.Fatalf("err = %v; want the invite remedy", err)
	}
	if _, err := os.Stat(filepath.Join(devState, device.CredentialFileName)); !os.IsNotExist(err) {
		t.Fatal("paired with an expired invitation")
	}
}

func TestJoinUsedInvitationNamesRemedy(t *testing.T) {
	skipUnlessTransportE2E(t)
	dir := transportTestDir(t)
	hubState := filepath.Join(dir, "hub-state")
	hubDB := filepath.Join(dir, "hub", "hub.db")
	firstState := filepath.Join(dir, "first-state")
	secondState := filepath.Join(dir, "second-state")
	regionFile := writeDERPRegionFile(t, startLocalDERP(t))
	serveCmd, serveLog, _, _ := startServeChild(t, hubDB, hubState, regionFile)
	defer stopChild(t, serveCmd, serveLog, 30*time.Second)

	code := inviteCode(t, hubState)
	if err := runPairOnly(t, firstState, code); err != nil {
		t.Fatalf("first pair: %v", err)
	}
	err := runPairOnly(t, secondState, code)
	if err == nil {
		t.Fatal("a used invitation was accepted twice")
	}
	if !strings.Contains(err.Error(), "ohya devices invite") {
		t.Fatalf("err = %v; want the invite remedy", err)
	}
	if _, err := os.Stat(filepath.Join(secondState, device.CredentialFileName)); !os.IsNotExist(err) {
		t.Fatal("a second device paired with one invitation")
	}
}

func TestJoinRebindKeepsDevice(t *testing.T) {
	skipUnlessTransportE2E(t)
	dir := transportTestDir(t)
	hubState := filepath.Join(dir, "hub-state")
	hubDB := filepath.Join(dir, "hub", "hub.db")
	devState := filepath.Join(dir, "dev-state")
	claudeRoot := filepath.Join(dir, "claude-projects")
	if err := os.MkdirAll(claudeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile(filepath.Join("..", "..", "testdata", "claude-code", "main-session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeRoot, "session.jsonl"), fixture, 0o644); err != nil {
		t.Fatal(err)
	}
	const wantEvents = 16

	regionFile := writeDERPRegionFile(t, startLocalDERP(t))
	serveCmd, serveLog, _, _ := startServeChild(t, hubDB, hubState, regionFile)
	code := inviteCode(t, hubState)
	joinCmd, joinLog := startJoinChild(t, devState, claudeRoot, code)
	waitForEvents(t, hubDB, wantEvents, serveLog, joinLog)
	stopChild(t, joinCmd, joinLog, 30*time.Second)

	deviceID := readDeviceID(t, devState)
	credBefore, err := device.LoadCredential(devState)
	if err != nil {
		t.Fatal(err)
	}
	savedAddr, err := device.LoadHubAddress(devState)
	if err != nil {
		t.Fatal(err)
	}

	rebindCode := inviteCode(t, hubState)
	if err := runPairOnly(t, devState, rebindCode); err != nil {
		t.Fatalf("rebind with a fresh code: %v", err)
	}
	if got := readDeviceID(t, devState); got != deviceID {
		t.Fatalf("device identity changed on rebind: %q, want %q", got, deviceID)
	}
	credAfter, err := device.LoadCredential(devState)
	if err != nil || credAfter != credBefore {
		t.Fatalf("credential changed on rebind: %+v, %v; want %+v", credAfter, err, credBefore)
	}
	addrAfter, err := device.LoadHubAddress(devState)
	if err != nil || addrAfter != savedAddr {
		t.Fatalf("hub address changed on rebind: %q, %v; want %q", addrAfter, err, savedAddr)
	}
	invitation, rebindAddr, err := parseJoinCode(rebindCode)
	if err != nil {
		t.Fatal(err)
	}
	if rebindAddr != savedAddr {
		t.Fatalf("a second hub appeared in this test state: %q vs %q", rebindAddr, savedAddr)
	}
	if state := invitationStates(t, hubState); len(state) != 2 {
		t.Fatalf("invitations = %+v; want one used and one open", state)
	}
	for _, inv := range invitationStates(t, hubState) {
		if inv.ID == invitation {
			if inv.State != "open" || inv.UsedBy != "" {
				t.Fatalf("rebind invitation = %+v; want open and unused", inv)
			}
		}
	}
	t.Log("rebind kept identity, credential, and address; the invitation stayed unused")

	resumedJoin, resumedLog := startJoinChild(t, devState, claudeRoot, "")
	deadline := time.Now().Add(30 * time.Second)
	for {
		events, sessions, _ := hubCounts(t, hubDB)
		if events == wantEvents && sessions >= 1 {
			break
		}
		if time.Now().After(deadline) {
			stopChild(t, resumedJoin, resumedLog, 30*time.Second)
			t.Fatalf("resume after rebind: hub holds %d events, %d sessions; want %d and no duplicates", events, sessions, wantEvents)
		}
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(3 * time.Second)
	stopChild(t, resumedJoin, resumedLog, 30*time.Second)
	stopChild(t, serveCmd, serveLog, 30*time.Second)
}

func TestJoinRebindRevokedRejected(t *testing.T) {
	skipUnlessTransportE2E(t)
	dir := transportTestDir(t)
	hubState := filepath.Join(dir, "hub-state")
	hubDB := filepath.Join(dir, "hub", "hub.db")
	devState := filepath.Join(dir, "dev-state")
	regionFile := writeDERPRegionFile(t, startLocalDERP(t))
	serveCmd, serveLog, _, _ := startServeChild(t, hubDB, hubState, regionFile)
	code := inviteCode(t, hubState)

	if err := runPairOnly(t, devState, code); err != nil {
		t.Fatalf("pair: %v", err)
	}
	deviceID := readDeviceID(t, devState)
	credBefore, err := device.LoadCredential(devState)
	if err != nil {
		t.Fatal(err)
	}

	var revokeOutput bytes.Buffer
	if err := runDevicesRevoke([]string{"--state", hubState, deviceID}, &revokeOutput, os.Stderr); err != nil {
		t.Fatal(err)
	}

	rebindCode := inviteCode(t, hubState)
	openBefore := countOpenInvitations(t, hubState)
	err = runPairOnly(t, devState, rebindCode)
	if err == nil {
		t.Fatal("a revoked credential was accepted on rebind")
	}
	if !strings.Contains(err.Error(), "refused this device's credential") || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("err = %v; want the refusal and the revocation reason", err)
	}
	credAfter, err := device.LoadCredential(devState)
	if err != nil || credAfter != credBefore {
		t.Fatalf("credential changed on a rejected rebind: %+v, %v", credAfter, err)
	}
	if got := readDeviceID(t, devState); got != deviceID {
		t.Fatalf("device identity changed on a rejected rebind: %q, want %q", got, deviceID)
	}
	if open := countOpenInvitations(t, hubState); open != openBefore {
		t.Fatalf("open invitations = %d after the rejected rebind; want %d", open, openBefore)
	}
	t.Log("a revoked device failed the rebind with the reason and never paired again")
	stopChild(t, serveCmd, serveLog, 30*time.Second)
}

func TestJoinRebindDifferentHubRejected(t *testing.T) {
	skipUnlessTransportE2E(t)
	dir := transportTestDir(t)
	hubAState := filepath.Join(dir, "hub-a-state")
	hubBState := filepath.Join(dir, "hub-b-state")
	hubADB := filepath.Join(dir, "hub-a", "hub.db")
	hubBDB := filepath.Join(dir, "hub-b", "hub.db")
	devState := filepath.Join(dir, "dev-state")
	regionFile := writeDERPRegionFile(t, startLocalDERP(t))
	serveA, logA, _, _ := startServeChild(t, hubADB, hubAState, regionFile)
	serveB, logB, _, _ := startServeChild(t, hubBDB, hubBState, regionFile)

	if err := runPairOnly(t, devState, inviteCode(t, hubAState)); err != nil {
		t.Fatalf("pair with hub A: %v (serve stderr:\n%s)", err, logA.String())
	}
	deviceID := readDeviceID(t, devState)

	openB := countOpenInvitations(t, hubBState)
	err := runPairOnly(t, devState, inviteCode(t, hubBState))
	if err == nil {
		t.Fatal("a code from a different hub was accepted")
	}
	if !strings.Contains(err.Error(), "refused this device's credential") {
		t.Fatalf("err = %v; want the credential refusal", err)
	}
	if got := readDeviceID(t, devState); got != deviceID {
		t.Fatalf("device identity changed on a cross-hub code: %q, want %q", got, deviceID)
	}
	if open := countOpenInvitations(t, hubBState); open != openB+1 {
		t.Fatalf("open invitations on hub B = %d; want %d (the fresh one unused)", open, openB+1)
	}
	regDB, err := sql.Open("sqlite3", "file:"+filepath.Join(hubBState, registry.FileName)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer regDB.Close()
	var paired int
	if err := regDB.QueryRow(`SELECT count(*) FROM devices`).Scan(&paired); err != nil {
		t.Fatal(err)
	}
	if paired != 0 {
		t.Fatalf("hub B holds %d paired devices; want none", paired)
	}
	t.Log("a cross-hub code failed the rebind and paired no new device")
	stopChild(t, serveA, logA, 30*time.Second)
	stopChild(t, serveB, logB, 30*time.Second)
}

func TestTransportReporting(t *testing.T) {
	skipUnlessTransportE2E(t)
	dir := transportTestDir(t)
	hubState := filepath.Join(dir, "hub-state")
	hubDB := filepath.Join(dir, "hub", "hub.db")
	devState := filepath.Join(dir, "dev-state")
	claudeRoot := filepath.Join(dir, "claude-projects")
	if err := os.MkdirAll(claudeRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile(filepath.Join("..", "..", "testdata", "claude-code", "main-session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeRoot, "session.jsonl"), fixture, 0o644); err != nil {
		t.Fatal(err)
	}
	const wantEvents = 16

	regionFile := writeDERPRegionFile(t, startLocalDERP(t))
	serveCmd, serveLog, _, _ := startServeChild(t, hubDB, hubState, regionFile)
	code := inviteCode(t, hubState)
	joinCmd, joinLog := startJoinChild(t, devState, claudeRoot, code)
	waitForEvents(t, hubDB, wantEvents, serveLog, joinLog)
	hubAddr, err := device.LoadHubAddress(devState)
	if err != nil {
		t.Fatal(err)
	}

	if logged := joinLog.String(); !strings.Contains(logged, "hub answers through a") ||
		!strings.Contains(logged, "round trip") || !strings.Contains(logged, "upload backlog:") {
		t.Fatalf("join setup report misses the path, round trip, or backlog:\n%s", logged)
	}

	var listOut bytes.Buffer
	if err := runDevicesList([]string{"--state", hubState, "--json"}, &listOut, os.Stderr); err != nil {
		t.Fatal(err)
	}
	var listed struct {
		Data struct {
			Devices []deviceJSON `json:"devices"`
		} `json:"data"`
	}
	if err := json.Unmarshal(listOut.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	deviceID := readDeviceID(t, devState)
	var seen *deviceJSON
	for _, d := range listed.Data.Devices {
		if d.ID == deviceID {
			seen = &d
		}
	}
	if seen == nil {
		t.Fatalf("devices list misses the joined device %s: %s", deviceID, listOut.String())
	}
	if seen.LastSyncAt == nil || seen.LastPath == nil {
		t.Fatalf("device entry carries no last sync: %+v", *seen)
	}
	if *seen.LastPath != "direct" && *seen.LastPath != "relay" {
		t.Fatalf("last path = %q; want direct or relay", *seen.LastPath)
	}
	if text := listOut.String(); strings.Contains(text, hubAddr) {
		t.Fatalf("devices list leaks the hub address:\n%s", text)
	}

	code2, out2, stderr2 := runCLI("doctor", "--state", devState, "--json")
	if code2 != 0 || stderr2 != "" {
		t.Fatalf("doctor on the joined device: code %d, stderr %q", code2, stderr2)
	}
	if strings.Contains(out2, hubAddr) {
		t.Fatalf("doctor leaks the hub address:\n%s", out2)
	}
	env := decodeEnvelope(t, out2)
	var data doctorJSONData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Transport == nil {
		t.Fatalf("doctor reported no transport object: %s", out2)
	}
	tr := *data.Transport
	if tr.HubAnswers == nil || !*tr.HubAnswers {
		t.Fatalf("hub_answers = %v; want true against the live hub", tr.HubAnswers)
	}
	if tr.Path != "direct" && tr.Path != "relay" {
		t.Fatalf("path = %q; want direct or relay", tr.Path)
	}
	if tr.RoundTripMS == nil || *tr.RoundTripMS <= 0 {
		t.Fatalf("round_trip_ms = %v; want a measured value", tr.RoundTripMS)
	}
	if tr.ProtocolCompatible == nil || !*tr.ProtocolCompatible {
		t.Fatalf("protocol_compatible = %v; want true", tr.ProtocolCompatible)
	}
	for _, check := range []string{"hub_transport", "hub_protocol"} {
		if f := soleFinding(t, data.Findings, check); f.Severity != sevOK {
			t.Fatalf("check %s = %+v; want ok", check, f)
		}
	}
	stopChild(t, joinCmd, joinLog, 30*time.Second)
	stopChild(t, serveCmd, serveLog, 30*time.Second)
}
