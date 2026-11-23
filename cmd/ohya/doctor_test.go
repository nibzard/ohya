package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tailscale/tailcat"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"

	"github.com/nibzard/ohya/internal/claudecode"
	"github.com/nibzard/ohya/internal/device"
	"github.com/nibzard/ohya/internal/store"
	"github.com/nibzard/ohya/internal/transport/address"

	_ "github.com/mattn/go-sqlite3"
)

type doctorEnv struct {
	dbPath   string
	stateDir string
	root     string
}

func newDoctorEnv(t *testing.T) *doctorEnv {
	t.Helper()
	e := newSliceEnv(t)
	copyFixture(t, e.session("main-session.jsonl"), "main-session.jsonl")
	if _, stderr := e.runPass(t); stderr != "" {
		t.Fatalf("healthy fixture pass reported diagnostics: %s", stderr)
	}
	return &doctorEnv{dbPath: e.dbPath, stateDir: e.stateDir, root: e.root}
}

func doctorRun(t *testing.T, e *doctorEnv, extra ...string) (int, string, string) {
	t.Helper()
	return runCLI(append([]string{"doctor", "--db", e.dbPath, "--state", e.stateDir}, extra...)...)
}

func soleFinding(t *testing.T, findings []finding, check string) finding {
	t.Helper()
	var got []finding
	for _, f := range findings {
		if f.Check == check {
			got = append(got, f)
		}
	}
	if len(got) != 1 {
		t.Fatalf("check %s reported %d findings, want 1: %+v", check, len(got), got)
	}
	return got[0]
}

func rawOpen(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestDoctorHealthyFixtureExitsZero(t *testing.T) {
	e := newDoctorEnv(t)

	findings, _, err := runDoctor(context.Background(), doctorOptions{
		dbPath: e.dbPath, stateDir: e.stateDir,
	})
	if err != nil {
		t.Fatalf("healthy run returned %v", err)
	}
	for _, check := range []string{"schema_version", "parser_versions", "fts_index", "stopped_sources", "device_identity", "join_state"} {
		if f := soleFinding(t, findings, check); f.Severity != sevOK {
			t.Fatalf("check %s = %+v; want ok", check, f)
		}
	}
	deep, _, err := runDoctor(context.Background(), doctorOptions{
		dbPath: e.dbPath, stateDir: e.stateDir, deep: true,
	})
	if err != nil {
		t.Fatalf("deep run returned %v", err)
	}
	if f := soleFinding(t, deep, "fts_index"); f.Severity != sevOK || !strings.Contains(f.Detail, "passes the FTS5 integrity-check") {
		t.Fatalf("deep fts_index = %+v", f)
	}

	code, out, stderr := doctorRun(t, e)
	if code != 0 || stderr != "" {
		t.Fatalf("healthy run: code %d, stderr %q", code, stderr)
	}
	for _, want := range append([]string{
		"ok    schema_version: database schema",
		"ok    fts_index: events_fts integrity-check skipped; run ohya doctor --deep to verify it",
		"ok    stopped_sources: no source is stopped at its checkpoint (1 watched)",
		"ok    device_identity: device.json holds device",
		"ok    join_state: no credential.json in",
	}, fmt.Sprintf("ok    parser_versions: stored parser output versions: claude-code %d", claudecode.OutputVersion)) {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout misses %q:\n%s", want, out)
		}
	}
}

func TestDoctorNewerParserRowsAreAnError(t *testing.T) {
	e := newDoctorEnv(t)
	raw := rawOpen(t, e.dbPath)
	if _, err := raw.Exec(`UPDATE events SET parser_version = ?`, claudecode.OutputVersion+1); err != nil {
		t.Fatal(err)
	}

	code, out, _ := doctorRun(t, e)
	if code != 1 {
		t.Fatalf("exit code %d; want 1 (stdout %q)", code, out)
	}
	f := soleFinding(t, parseDoctorFindings(t, out), "parser_versions")
	if f.Severity != sevError {
		t.Fatalf("severity = %s; want error", f.Severity)
	}
	if !strings.Contains(f.Detail, fmt.Sprintf("above this build's %d", claudecode.OutputVersion)) {
		t.Fatalf("detail misses the versions: %s", f.Detail)
	}
	if !strings.Contains(f.Remedy, "upgrade the ohya binary, then run ohya rederive") {
		t.Fatalf("remedy misses the upgrade path: %s", f.Remedy)
	}
}

func TestDoctorSchemaBehindBinaryWarnsWithoutMigrating(t *testing.T) {
	e := newDoctorEnv(t)
	schema, err := store.SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	raw := rawOpen(t, e.dbPath)
	if _, err := raw.Exec(`DELETE FROM schema_migrations WHERE version = ?`, schema); err != nil {
		t.Fatal(err)
	}

	code, out, _ := doctorRun(t, e)
	if code != 0 {
		t.Fatalf("exit code %d; want 0 — a warning must not fail (stdout %q)", code, out)
	}
	f := soleFinding(t, parseDoctorFindings(t, out), "schema_version")
	if f.Severity != sevWarn {
		t.Fatalf("severity = %s; want warn", f.Severity)
	}
	if !strings.Contains(f.Detail, fmt.Sprintf("behind this build's migrations (%d)", schema)) {
		t.Fatalf("detail misses the versions: %s", f.Detail)
	}
	if !strings.Contains(f.Remedy, "ohya ingest --once") || !strings.Contains(f.Remedy, "doctor never does") {
		t.Fatalf("remedy misses the migrating command: %s", f.Remedy)
	}
	var n int
	if err := raw.QueryRow(`SELECT count(*) FROM schema_migrations WHERE version = ?`, schema).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("doctor applied migration %d", schema)
	}
}

func TestDoctorSchemaAheadOfBinaryIsAnError(t *testing.T) {
	e := newDoctorEnv(t)
	raw := rawOpen(t, e.dbPath)
	if _, err := raw.Exec(`INSERT INTO schema_migrations (version, name, checksum, applied_at_ms)
		VALUES (99, 'future', 'x', 0)`); err != nil {
		t.Fatal(err)
	}
	code, out, _ := doctorRun(t, e)
	if code != 1 {
		t.Fatalf("exit code %d; want 1", code)
	}
	f := soleFinding(t, parseDoctorFindings(t, out), "schema_version")
	if f.Severity != sevError || !strings.Contains(f.Remedy, "upgrade the ohya binary") {
		t.Fatalf("finding = %+v; want error with the upgrade remedy", f)
	}
}

func TestDoctorStoppedSourceCarriesADR0004Remedy(t *testing.T) {
	line := func(content string) string {
		return fmt.Sprintf(`{"type":"user","sessionId":%q,"message":{"role":"user","content":%q}}`+"\n",
			fixtureNativeID, content)
	}
	stopFixture := func(t *testing.T, name, oversized string) (*doctorEnv, string) {
		e := newSliceEnv(t)
		before := line("before")
		writeFile(t, e.session(name), before+oversized+line("after"))
		stats, _ := e.runPass(t)
		if stats.stopped != 1 || stats.inserted != 1 {
			t.Fatalf("fixture pass = %+v; want 1 event then a stop", stats)
		}
		checkpoint := filepath.Join(e.stateDir, "sources", "claude-code.json")
		return &doctorEnv{dbPath: e.dbPath, stateDir: e.stateDir, root: e.root}, checkpoint
	}

	t.Run("size stop names --max-record-bytes", func(t *testing.T) {
		oversized := line(strings.Repeat("x", claudecode.DefaultMaxRecordBytes+512))
		e, checkpoint := stopFixture(t, "oversized.jsonl", oversized)
		beforeBytes, err := os.ReadFile(checkpoint)
		if err != nil {
			t.Fatal(err)
		}
		code, out, _ := doctorRun(t, e)
		if code != 0 {
			t.Fatalf("exit code %d; want 0 — a stopped source warns, it does not fail (stdout %q)", code, out)
		}
		f := soleFinding(t, parseDoctorFindings(t, out), "stopped_sources")
		if f.Severity != sevWarn {
			t.Fatalf("severity = %s; want warn", f.Severity)
		}
		if !strings.Contains(f.Detail, fmt.Sprintf("stopped at offset %d", len(line("before")))) {
			t.Fatalf("detail misses the stop offset: %s", f.Detail)
		}
		if !strings.Contains(f.Remedy, "--max-record-bytes") ||
			!strings.Contains(f.Remedy, "docs/adr/0004-stopped-source-recovery.md") {
			t.Fatalf("remedy misses the ADR 0004 size remedy: %s", f.Remedy)
		}
		afterBytes, err := os.ReadFile(checkpoint)
		if err != nil {
			t.Fatal(err)
		}
		if string(beforeBytes) != string(afterBytes) {
			t.Fatal("doctor modified the checkpoint file")
		}
	})

	t.Run("malformed record names the repair", func(t *testing.T) {
		e, _ := stopFixture(t, "malformed.jsonl", "{not json}\n")
		code, out, _ := doctorRun(t, e)
		if code != 0 {
			t.Fatalf("exit code %d; want 0 (stdout %q)", code, out)
		}
		f := soleFinding(t, parseDoctorFindings(t, out), "stopped_sources")
		if f.Severity != sevWarn {
			t.Fatalf("severity = %s; want warn", f.Severity)
		}
		if !strings.Contains(f.Remedy, "repair the record at this offset") ||
			!strings.Contains(f.Remedy, "never skips bytes") {
			t.Fatalf("remedy misses the ADR 0004 repair remedy: %s", f.Remedy)
		}
	})

	t.Run("a growing file is not a stop", func(t *testing.T) {
		e := newDoctorEnv(t)
		appendFile(t, filepath.Join(e.root, "fixture-project", "main-session.jsonl"),
			line("appended after the pass"))
		code, out, _ := doctorRun(t, e)
		if code != 0 {
			t.Fatalf("exit code %d; want 0 (stdout %q)", code, out)
		}
		f := soleFinding(t, parseDoctorFindings(t, out), "stopped_sources")
		if f.Severity != sevOK || !strings.Contains(f.Detail, "no source is stopped") {
			t.Fatalf("finding = %+v; want ok", f)
		}
	})
}

func fakeHubAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	region := &tailcfg.DERPRegion{
		RegionID:   9,
		RegionCode: "closedport",
		Nodes: []*tailcfg.DERPNode{{
			Name:             "closedport1",
			RegionID:         9,
			HostName:         "127.0.0.1",
			IPv4:             "127.0.0.1",
			IPv6:             "none",
			DERPPort:         port,
			InsecureForTests: true,
		}},
	}
	node := key.NewNode()
	ci := tailcat.ConnInfo{
		ServerPublic:      tailcat.NodePublic{NodePublic: node.Public()},
		ServerDiscoPublic: tailcat.DiscoPublicForNode(node),
		PresharedKey:      tailcat.NewPresharedKey(),
		Region:            []*tailcfg.DERPRegion{region},
	}
	return string(ci.Addr())
}

func TestDoctorJoinState(t *testing.T) {
	t.Parallel()
	const deviceID = "0123456789abcdef0123456789abcdef"
	pairedEnv := func(t *testing.T) *doctorEnv {
		e := newDoctorEnv(t)
		if err := device.SaveCredential(e.stateDir, device.Credential{DeviceID: deviceID, Secret: "s3cret"}); err != nil {
			t.Fatal(err)
		}
		return e
	}

	t.Run("missing hub address warns", func(t *testing.T) {
		e := pairedEnv(t)
		code, out, _ := doctorRun(t, e)
		if code != 0 {
			t.Fatalf("exit code %d; want 0 — a missing hub address warns (stdout %q)", code, out)
		}
		f := soleFinding(t, parseDoctorFindings(t, out), "join_state")
		if f.Severity != sevWarn {
			t.Fatalf("severity = %s; want warn", f.Severity)
		}
		if !strings.Contains(f.Detail, "no "+device.HubAddressFileName) || !strings.Contains(f.Detail, deviceID) {
			t.Fatalf("detail misses the address file or the device: %s", f.Detail)
		}
		if !strings.Contains(f.Remedy, "ohya join") || !strings.Contains(f.Remedy, "ohya1") {
			t.Fatalf("remedy misses the rejoin hint: %s", f.Remedy)
		}
		if strings.Contains(out, "s3cret") {
			t.Fatalf("report leaks the credential secret:\n%s", out)
		}
	})

	t.Run("saved hub address is ok and never printed", func(t *testing.T) {
		e := pairedEnv(t)
		addr := fakeHubAddr(t)
		if !address.ValidHubAddr(addr) {
			t.Fatal("test address must be a valid tailcat address")
		}
		if err := device.SaveHubAddress(e.stateDir, addr); err != nil {
			t.Fatal(err)
		}
		code, out, _ := doctorRun(t, e)
		if code != 0 {
			t.Fatalf("exit code %d; want 0 — an unreachable hub warns (stdout %q)", code, out)
		}
		f := soleFinding(t, parseDoctorFindings(t, out), "join_state")
		if f.Severity != sevOK || !strings.Contains(f.Detail, "holds the hub address") {
			t.Fatalf("finding = %+v; want ok with a saved hub address", f)
		}
		if strings.Contains(out, addr) {
			t.Fatalf("report leaks the hub address, which is a secret:\n%s", out)
		}
	})

	t.Run("unparseable hub address is an error", func(t *testing.T) {
		e := pairedEnv(t)
		if err := device.SaveHubAddress(e.stateDir, "ohya2.abcdef.secret-address"); err != nil {
			t.Fatal(err)
		}
		code, out, _ := doctorRun(t, e)
		if code != 1 {
			t.Fatalf("exit code %d; want 1 (stdout %q)", code, out)
		}
		f := soleFinding(t, parseDoctorFindings(t, out), "join_state")
		if f.Severity != sevError || !strings.Contains(f.Detail, "no valid tailcat address") {
			t.Fatalf("finding = %+v; want error naming the invalid address", f)
		}
		if !strings.Contains(f.Remedy, "ohya join") {
			t.Fatalf("remedy misses the rejoin remedy: %s", f.Remedy)
		}
	})

	t.Run("unusable hub address is an error", func(t *testing.T) {
		e := pairedEnv(t)
		writeFile(t, filepath.Join(e.stateDir, device.HubAddressFileName), "{not json}")
		code, out, _ := doctorRun(t, e)
		if code != 1 {
			t.Fatalf("exit code %d; want 1 (stdout %q)", code, out)
		}
		f := soleFinding(t, parseDoctorFindings(t, out), "join_state")
		if f.Severity != sevError || !strings.Contains(f.Remedy, "ohya join") {
			t.Fatalf("finding = %+v; want error with the rejoin remedy", f)
		}
	})

	t.Run("unusable credential is an error", func(t *testing.T) {
		e := newDoctorEnv(t)
		writeFile(t, filepath.Join(e.stateDir, device.CredentialFileName), "{not json}")
		code, out, _ := doctorRun(t, e)
		if code != 1 {
			t.Fatalf("exit code %d; want 1 (stdout %q)", code, out)
		}
		f := soleFinding(t, parseDoctorFindings(t, out), "join_state")
		if f.Severity != sevError || !strings.Contains(f.Remedy, "revoke this device identity") {
			t.Fatalf("finding = %+v; want error with the revoke remedy", f)
		}
	})
}

func TestDoctorHubTransport(t *testing.T) {
	t.Parallel()
	const deviceID = "0123456789abcdef0123456789abcdef"

	t.Run("unpaired state reports no transport object", func(t *testing.T) {
		e := newDoctorEnv(t)
		code, out, stderr := doctorRun(t, e, "--json")
		if code != 0 || stderr != "" {
			t.Fatalf("code %d, stderr %q", code, stderr)
		}
		env := decodeEnvelope(t, out)
		var data doctorJSONData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			t.Fatal(err)
		}
		if data.Transport != nil {
			t.Fatalf("unpaired state must not report transport: %+v", *data.Transport)
		}
	})

	t.Run("unreachable hub reports unknown values in JSON", func(t *testing.T) {
		e := newDoctorEnv(t)
		if err := device.SaveCredential(e.stateDir, device.Credential{DeviceID: deviceID, Secret: "s3cret"}); err != nil {
			t.Fatal(err)
		}
		addr := fakeHubAddr(t)
		if err := device.SaveHubAddress(e.stateDir, addr); err != nil {
			t.Fatal(err)
		}
		code, out, _ := doctorRun(t, e, "--json")
		if code != 0 {
			t.Fatalf("exit code %d; want 0 — an unreachable hub warns (stdout %q)", code, out)
		}
		env := decodeEnvelope(t, out)
		var data doctorJSONData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			t.Fatal(err)
		}
		if data.Transport == nil {
			t.Fatalf("paired state must report transport: %s", out)
		}
		got := *data.Transport
		if got.HubAnswers == nil || *got.HubAnswers {
			t.Fatalf("hub_answers = %v; want false", got.HubAnswers)
		}
		if got.Path != "unknown" {
			t.Fatalf("path = %q; want unknown", got.Path)
		}
		if got.RelayRegion != nil {
			t.Fatalf("relay_region = %v; want null", *got.RelayRegion)
		}
		if got.RoundTripMS != nil {
			t.Fatalf("round_trip_ms = %v; want null", *got.RoundTripMS)
		}
		if got.ProtocolCompatible != nil {
			t.Fatalf("protocol_compatible = %v; want null", *got.ProtocolCompatible)
		}
		if got.ProtocolVersion != 1 {
			t.Fatalf("protocol_version = %d; want 1", got.ProtocolVersion)
		}
		if strings.Contains(out, addr) {
			t.Fatalf("report leaks the hub address, which is a secret:\n%s", out)
		}
	})
}

func TestDoctorMissingDeviceIdentityWarns(t *testing.T) {
	e := newDoctorEnv(t)
	if err := os.Remove(filepath.Join(e.stateDir, device.FileName)); err != nil {
		t.Fatal(err)
	}
	code, out, _ := doctorRun(t, e)
	if code != 0 {
		t.Fatalf("exit code %d; want 0 — a missing identity warns (stdout %q)", code, out)
	}
	f := soleFinding(t, parseDoctorFindings(t, out), "device_identity")
	if f.Severity != sevWarn {
		t.Fatalf("severity = %s; want warn", f.Severity)
	}
	if !strings.Contains(f.Detail, "no device.json in") {
		t.Fatalf("detail misses the missing file: %s", f.Detail)
	}
	if !strings.Contains(f.Remedy, "ohya ingest --once") {
		t.Fatalf("remedy misses the creating command: %s", f.Remedy)
	}
	if _, err := os.Stat(filepath.Join(e.stateDir, device.FileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("doctor created a device identity: %v", err)
	}
}

func TestDoctorUnusableDeviceIdentityIsAnError(t *testing.T) {
	e := newDoctorEnv(t)
	writeFile(t, filepath.Join(e.stateDir, device.FileName), "{not json}")
	code, out, _ := doctorRun(t, e)
	if code != 1 {
		t.Fatalf("exit code %d; want 1 (stdout %q)", code, out)
	}
	f := soleFinding(t, parseDoctorFindings(t, out), "device_identity")
	if f.Severity != sevError || !strings.Contains(f.Remedy, "restore device.json from a backup") {
		t.Fatalf("finding = %+v; want error with the restore remedy", f)
	}
}

func TestDoctorFTSCorruptionIsAnError(t *testing.T) {
	e := newDoctorEnv(t)
	hub, err := store.Open(context.Background(), e.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hub.Writer().Exec(`INSERT INTO events_fts(events_fts) VALUES ('delete-all')`); err != nil {
		t.Fatal(err)
	}
	if err := hub.Close(); err != nil {
		t.Fatal(err)
	}
	code, out, _ := doctorRun(t, e)
	if code != 0 {
		t.Fatalf("default exit code %d; want 0 without the deep check (stdout %q)", code, out)
	}
	code, out, _ = doctorRun(t, e, "--deep")
	if code != 1 {
		t.Fatalf("exit code %d; want 1 (stdout %q)", code, out)
	}
	f := soleFinding(t, parseDoctorFindings(t, out), "fts_index")
	if f.Severity != sevError || !strings.Contains(f.Detail, "integrity-check") {
		t.Fatalf("finding = %+v; want error naming the integrity-check", f)
	}
}

func TestDoctorMissingDatabaseWarns(t *testing.T) {
	dir := t.TempDir()
	code, out, _ := runCLI("doctor", "--db", filepath.Join(dir, "absent.db"), "--state", dir)
	if code != 0 {
		t.Fatalf("exit code %d; want 0 (stdout %q)", code, out)
	}
	if !strings.Contains(out, "warn  database: hub database") ||
		!strings.Contains(out, "run ohya ingest --once to create and migrate it") {
		t.Fatalf("stdout misses the missing-database finding:\n%s", out)
	}
	if !strings.Contains(out, "warn  device_identity") {
		t.Fatalf("stdout misses the missing identity on a fresh machine:\n%s", out)
	}
}

func TestDoctorJSONEnvelope(t *testing.T) {
	t.Run("healthy golden", func(t *testing.T) {
		e := newDoctorEnv(t)
		code, out, stderr := doctorRun(t, e, "--json")
		if code != 0 || stderr != "" {
			t.Fatalf("code %d, stderr %q", code, stderr)
		}
		want := `{"schema_version":1,"command":"ohya doctor","data":{"findings":[` +
			`{"check":"schema_version","severity":"ok","detail":"<detail>","remedy":""},` +
			`{"check":"parser_versions","severity":"ok","detail":"<detail>","remedy":""},` +
			`{"check":"fts_index","severity":"ok","detail":"<detail>","remedy":""},` +
			`{"check":"stopped_sources","severity":"ok","detail":"<detail>","remedy":""},` +
			`{"check":"device_identity","severity":"ok","detail":"<detail>","remedy":""},` +
			`{"check":"join_state","severity":"ok","detail":"<detail>","remedy":""}]},"error":null}` + "\n"
		got := scrubJSONKeys(t, out, "detail")
		goldenCompare(t, got, want)
	})

	t.Run("error findings ride in data with exit 1", func(t *testing.T) {
		e := newDoctorEnv(t)
		raw := rawOpen(t, e.dbPath)
		if _, err := raw.Exec(`UPDATE events SET parser_version = ?`, claudecode.OutputVersion+1); err != nil {
			t.Fatal(err)
		}
		code, out, _ := doctorRun(t, e, "--json")
		if code != 1 {
			t.Fatalf("exit code %d; want 1 (stdout %q)", code, out)
		}
		env := decodeEnvelope(t, out)
		if env.Error != nil {
			t.Fatalf("error = %+v; want null — findings are data, the exit code is the verdict", env.Error)
		}
		var data doctorJSONData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			t.Fatal(err)
		}
		if len(data.Findings) == 0 {
			t.Fatal("findings empty")
		}
		var errors int
		for _, f := range data.Findings {
			if f.Severity == sevError {
				errors++
			}
		}
		if errors == 0 {
			t.Fatalf("no error finding in %+v", data.Findings)
		}
	})

	t.Run("usage errors stay text", func(t *testing.T) {
		code, out, stderr := runCLI("doctor", "extra")
		if code != 2 || out != "" {
			t.Fatalf("code %d, stdout %q; want 2 and empty stdout", code, out)
		}
		if !strings.Contains(stderr, "unexpected argument") || !strings.Contains(stderr, "ohya doctor --help") {
			t.Fatalf("stderr %q; want the usage-error line", stderr)
		}
		if code, _, _ := runCLI("doctor", "--max-record-bytes", "0"); code != 2 {
			t.Fatalf("code %d; want 2 for a non-positive bound", code)
		}
	})
}

func TestDoctorReadonlyOath(t *testing.T) {
	e := newDoctorEnv(t)
	raw := rawOpen(t, e.dbPath)
	if _, err := raw.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	hub, err := store.Open(context.Background(), e.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := hub.Close(); err != nil {
		t.Fatal(err)
	}
	sizes := func() map[string]int64 {
		out := map[string]int64{}
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if info, err := os.Stat(e.dbPath + suffix); err == nil {
				out[suffix] = info.Size()
			}
		}
		return out
	}
	before := sizes()
	if _, out, _ := doctorRun(t, e); !strings.Contains(out, "ok") {
		t.Fatalf("doctor output: %q", out)
	}
	after := sizes()
	for suffix, size := range before {
		if after[suffix] != size {
			t.Fatalf("database%s changed size: %d -> %d", suffix, size, after[suffix])
		}
	}
	if _, ok := after["-wal"]; ok && after["-wal"] != 0 {
		t.Fatalf("doctor left a -wal of %d bytes", after["-wal"])
	}
}

func parseDoctorFindings(t *testing.T, out string) []finding {
	t.Helper()
	var findings []finding
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var f finding
		rest, ok := strings.CutPrefix(line, "ok    ")
		f.Severity = sevOK
		if !ok {
			rest, ok = strings.CutPrefix(line, "warn  ")
			f.Severity = sevWarn
		}
		if !ok {
			rest, ok = strings.CutPrefix(line, "error ")
			f.Severity = sevError
		}
		if !ok {
			t.Fatalf("line lacks a severity: %q", line)
		}
		body, remedy, hasRemedy := strings.Cut(rest, "; remedy: ")
		check, detail, ok := strings.Cut(body, ": ")
		if !ok {
			t.Fatalf("line lacks check and detail: %q", line)
		}
		f.Check, f.Detail = check, detail
		if hasRemedy {
			f.Remedy = remedy
		}
		findings = append(findings, f)
	}
	return findings
}
