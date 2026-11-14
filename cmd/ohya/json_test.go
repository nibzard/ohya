package main

// The --json contract tests (T062, ADR 0005): one golden test per command
// for success and one failure — the goldens pin field names, order and the
// stable values, scrubbing only what varies per run — plus one shared
// decoder that decodes every command's output with the same envelope struct.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/nibzard/ohya/internal/claudecode"
	"github.com/nibzard/ohya/internal/codex"
	"github.com/nibzard/ohya/internal/control"
	"github.com/nibzard/ohya/internal/events"
	"github.com/nibzard/ohya/internal/pi"
	"github.com/nibzard/ohya/internal/registry"
	"github.com/nibzard/ohya/internal/store"
)

// runCLI runs one invocation through the production exit-code mapping and
// returns the exit code with both streams.
func runCLI(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := cliMain(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// scrubJSONKeys replaces the string values of the named keys with a
// "<key>" placeholder, so a golden comparison pins the shape and the stable
// values while tolerating what varies per run.
func scrubJSONKeys(t *testing.T, got string, keys ...string) string {
	t.Helper()
	for _, key := range keys {
		re, err := regexp.Compile(`("` + regexp.QuoteMeta(key) + `":")([^"\\]|\\.)*"`)
		if err != nil {
			t.Fatal(err)
		}
		got = re.ReplaceAllString(got, `${1}<`+key+`>"`)
	}
	return got
}

// scrubJSONNumbers replaces the numeric values of the named keys the same way.
func scrubJSONNumbers(t *testing.T, got string, keys ...string) string {
	t.Helper()
	for _, key := range keys {
		re, err := regexp.Compile(`("` + regexp.QuoteMeta(key) + `":)\d+`)
		if err != nil {
			t.Fatal(err)
		}
		got = re.ReplaceAllString(got, `${1}<`+key+`>`)
	}
	return got
}

// goldenCompare fails unless got equals want exactly.
func goldenCompare(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("output =\n%s\nwant\n%s", got, want)
	}
}

// dataNull reports whether the envelope's data is absent or the JSON null.
func dataNull(data []byte) bool {
	return len(data) == 0 || string(data) == "null"
}

// decodeEnvelope decodes one exactly-one-object envelope from out.
func decodeEnvelope(t *testing.T, out string) jsonEnvelope {
	t.Helper()
	var env jsonEnvelope
	dec := json.NewDecoder(strings.NewReader(out))
	if err := dec.Decode(&env); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if dec.More() {
		t.Fatalf("output %q holds more than one JSON value", out)
	}
	return env
}

// pairDevice provisions a coordination plane, issues one join code, and
// consumes it for deviceID, so a revoke or list has a paired device to name.
func pairDevice(t *testing.T, stateDir, deviceID string) {
	t.Helper()
	provisionControl(t, stateDir)
	out, err := runDevices(t, stateDir, "invite")
	if err != nil {
		t.Fatal(err)
	}
	code := strings.SplitN(strings.TrimSpace(out), "\n", 2)[0]
	invitation, _, err := control.SplitJoinCode(code)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := registry.Open(filepath.Join(stateDir, registry.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	if _, err := reg.Consume(context.Background(), invitation, deviceID); err != nil {
		t.Fatal(err)
	}
}

// jsonFixtureLine builds one Claude Code user record like the T060 tests.
func jsonFixtureLine(content string) string {
	return fmt.Sprintf(`{"type":"user","sessionId":%q,"message":{"role":"user","content":%q}}`+"\n",
		fixtureNativeID, content)
}

func TestVersionJSONGolden(t *testing.T) {
	schema, err := store.SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(`{"schema_version":1,"command":"ohya version","data":{"version":"<version>","revision":"<revision>","dirty":"<dirty>","go":"<go>","schema":%d,"parser.claude":%d,"parser.codex":%d,"parser.pi":%d},"error":null}`+"\n",
		schema, claudecode.OutputVersion, codex.OutputVersion, pi.OutputVersion)
	for _, args := range [][]string{{"version", "--json"}, {"--version", "--json"}} {
		code, out, stderr := runCLI(args...)
		if code != 0 || stderr != "" {
			t.Fatalf("%v: code %d, stderr %q", args, code, stderr)
		}
		goldenCompare(t, scrubJSONKeys(t, out, "version", "revision", "dirty", "go"), want)
	}
}

// TestVersionJSONUsageErrorStaysText pins the ADR 0005 decision that usage
// errors never produce an envelope: they keep the T059 contract — one stderr
// line, exit 2, nothing on stdout.
func TestVersionJSONUsageErrorStaysText(t *testing.T) {
	code, out, stderr := runCLI("version", "--json", "extra")
	if code != 2 || out != "" {
		t.Fatalf("code %d, stdout %q; want 2 and empty stdout", code, out)
	}
	if !strings.Contains(stderr, "unexpected argument") || !strings.Contains(stderr, "ohya version --help") {
		t.Fatalf("stderr %q; want the usage-error line", stderr)
	}
}

func TestRederiveJSONGolden(t *testing.T) {
	t.Run("success no-op", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "hub.db")
		ingestFixtureOnce(t, dbPath)
		code, out, stderr := runCLI("rederive", "--json", "--db", dbPath)
		if code != 0 || stderr != "" {
			t.Fatalf("code %d, stderr %q", code, stderr)
		}
		goldenCompare(t, scrubJSONNumbers(t, out, "duration_ms"),
			`{"schema_version":1,"command":"ohya rederive","data":{"events":0,"sessions":0,"duration_ms":<duration_ms>},"error":null}`+"\n")
	})

	t.Run("success rebuilds stale rows", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "hub.db")
		// Ingest the Pi fixture (parser output version 2), then stamp its
		// rows with version 1 to make them stale; the Claude Code parser is
		// already at version 1 and the schema refuses lower stamps.
		data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "pi", "main-session.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		hub, err := store.Open(context.Background(), dbPath)
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		deviceID := "0123456789abcdef0123456789abcdef"
		if err := hub.RegisterSource(ctx, "src-pi", deviceID, "pi", "/main-session.jsonl"); err != nil {
			t.Fatal(err)
		}
		parsed, err := events.Parse(pi.AgentName, data, 0, events.Options{SourceScope: "src-pi"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := hub.Ingest(ctx, store.Batch{
			SourceID: "src-pi", DeviceID: deviceID, Events: parsed.Events,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := hub.Writer().Exec(`UPDATE events SET parser_version = 1`); err != nil {
			t.Fatal(err)
		}
		if err := hub.Close(); err != nil {
			t.Fatal(err)
		}
		code, out, stderr := runCLI("rederive", "--json", "--db", dbPath)
		if code != 0 || stderr != "" {
			t.Fatalf("code %d, stderr %q", code, stderr)
		}
		env := decodeEnvelope(t, out)
		var report rederiveJSONData
		if err := json.Unmarshal(env.Data, &report); err != nil {
			t.Fatal(err)
		}
		if report.Events != 8 || report.Sessions != 1 {
			t.Fatalf("re-derived %d events across %d sessions; want 8 across 1", report.Events, report.Sessions)
		}
	})

	t.Run("failure", func(t *testing.T) {
		blocker := filepath.Join(t.TempDir(), "blocker")
		if err := os.WriteFile(blocker, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		code, out, _ := runCLI("rederive", "--json", "--db", filepath.Join(blocker, "hub.db"))
		if code != 1 {
			t.Fatalf("exit code %d; want 1", code)
		}
		goldenCompare(t, scrubJSONKeys(t, out, "message"),
			`{"schema_version":1,"command":"ohya rederive","data":null,"error":{"code":"database_open_failed","message":"<message>","hint":"check the --db path and its permissions; another ohya process may hold the database"}}`+"\n")
	})
}

func TestDevicesListJSONGolden(t *testing.T) {
	t.Run("success empty", func(t *testing.T) {
		code, out, stderr := runCLI("devices", "list", "--json", "--state", t.TempDir())
		if code != 0 || stderr != "" {
			t.Fatalf("code %d, stderr %q", code, stderr)
		}
		goldenCompare(t, out,
			`{"schema_version":1,"command":"ohya devices list","data":{"devices":[],"invitations":[]},"error":null}`+"\n")
	})

	t.Run("success populated", func(t *testing.T) {
		state := t.TempDir()
		const deviceID = "0123456789abcdef0123456789abcdef"
		pairDevice(t, state, deviceID)
		code, out, stderr := runCLI("devices", "list", "--json", "--state", state)
		if code != 0 || stderr != "" {
			t.Fatalf("code %d, stderr %q", code, stderr)
		}
		got := scrubJSONKeys(t, out, "id", "paired_at", "created_at", "expires_at")
		goldenCompare(t, got, `{"schema_version":1,"command":"ohya devices list","data":{"devices":[{"id":"<id>","state":"active","paired_at":"<paired_at>"}],"invitations":[{"id":"<id>","state":"used","used_by":"`+deviceID+`","created_at":"<created_at>","expires_at":"<expires_at>","expired":false}]},"error":null}`+"\n")
		env := decodeEnvelope(t, out)
		var data listJSONData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			t.Fatal(err)
		}
		if len(data.Devices) != 1 || data.Devices[0].ID != deviceID {
			t.Fatalf("devices = %+v; want the paired device %s", data.Devices, deviceID)
		}
	})

	t.Run("success revoked device carries revoked_at", func(t *testing.T) {
		state := t.TempDir()
		const deviceID = "0123456789abcdef0123456789abcdef"
		pairDevice(t, state, deviceID)
		if code, _, _ := runCLI("devices", "revoke", "--json", "--state", state, deviceID); code != 0 {
			t.Fatal("revoke failed")
		}
		code, out, stderr := runCLI("devices", "list", "--json", "--state", state)
		if code != 0 || stderr != "" {
			t.Fatalf("code %d, stderr %q", code, stderr)
		}
		goldenCompare(t, scrubJSONKeys(t, out, "id", "paired_at", "created_at", "expires_at", "revoked_at"),
			`{"schema_version":1,"command":"ohya devices list","data":{"devices":[{"id":"<id>","state":"revoked","paired_at":"<paired_at>","revoked_at":"<revoked_at>"}],"invitations":[{"id":"<id>","state":"used","used_by":"`+deviceID+`","created_at":"<created_at>","expires_at":"<expires_at>","expired":false}]},"error":null}`+"\n")
	})

	t.Run("failure", func(t *testing.T) {
		blocker := filepath.Join(t.TempDir(), "blocker")
		if err := os.WriteFile(blocker, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		code, out, _ := runCLI("devices", "list", "--json", "--state", blocker)
		if code != 1 {
			t.Fatalf("exit code %d; want 1", code)
		}
		goldenCompare(t, scrubJSONKeys(t, out, "message"),
			`{"schema_version":1,"command":"ohya devices list","data":null,"error":{"code":"registry_open_failed","message":"<message>","hint":"check that the --state directory exists and is writable"}}`+"\n")
	})
}

func TestDevicesInviteJSONGolden(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		state := t.TempDir()
		provisionControl(t, state)
		code, out, stderr := runCLI("devices", "invite", "--json", "--state", state)
		if code != 0 {
			t.Fatalf("exit code %d (stderr %q)", code, stderr)
		}
		goldenCompare(t, scrubJSONKeys(t, out, "code", "expires_at"),
			`{"schema_version":1,"command":"ohya devices invite","data":{"code":"<code>","expires_at":"<expires_at>"},"error":null}`+"\n")
		env := decodeEnvelope(t, out)
		var data inviteJSONData
		if err := json.Unmarshal(env.Data, &data); err != nil {
			t.Fatal(err)
		}
		invitation, authKey, err := control.SplitJoinCode(data.Code)
		if err != nil || len(invitation) < 16 || len(authKey) < 16 {
			t.Fatalf("join code %q does not carry two secret halves (%v)", data.Code, err)
		}
		// The code is the operator's output: it never rides on stderr.
		if strings.Contains(stderr, data.Code) {
			t.Fatalf("stderr %q leaks the join code", stderr)
		}
		if !strings.Contains(stderr, "expires") {
			t.Fatalf("stderr %q misses the expiry diagnostic", stderr)
		}
	})

	t.Run("failure", func(t *testing.T) {
		state := t.TempDir()
		code, out, _ := runCLI("devices", "invite", "--json", "--state", state)
		if code != 1 {
			t.Fatalf("exit code %d; want 1", code)
		}
		got := strings.ReplaceAll(out, state, "<state>")
		goldenCompare(t, got, `{"schema_version":1,"command":"ohya devices invite","data":null,"error":{"code":"coordination_plane_absent","message":"control: holds no coordination plane at <state>/control; run ohya serve once to provision it","hint":"run ohya serve once to provision the coordination plane, then retry"}}`+"\n")
	})
}

func TestDevicesRevokeJSONGolden(t *testing.T) {
	const deviceID = "0123456789abcdef0123456789abcdef"
	state := t.TempDir()
	pairDevice(t, state, deviceID)

	t.Run("success", func(t *testing.T) {
		code, out, stderr := runCLI("devices", "revoke", "--json", "--state", state, deviceID)
		if code != 0 || stderr != "" {
			t.Fatalf("code %d, stderr %q", code, stderr)
		}
		goldenCompare(t, out,
			`{"schema_version":1,"command":"ohya devices revoke","data":{"device_id":"`+deviceID+`"},"error":null}`+"\n")
	})

	t.Run("failure already revoked", func(t *testing.T) {
		code, out, _ := runCLI("devices", "revoke", "--json", "--state", state, deviceID)
		if code != 1 {
			t.Fatalf("exit code %d; want 1", code)
		}
		goldenCompare(t, out, `{"schema_version":1,"command":"ohya devices revoke","data":null,"error":{"code":"device_already_revoked","message":"registry: revoke: registry: device is already revoked (device `+deviceID+`)","hint":"revocation is final for the identity; re-admission needs a new device identity"}}`+"\n")
	})

	t.Run("failure unknown device", func(t *testing.T) {
		code, out, _ := runCLI("devices", "revoke", "--json", "--state", state, "ffffffffffffffffffffffffffffffff")
		if code != 1 {
			t.Fatalf("exit code %d; want 1", code)
		}
		env := decodeEnvelope(t, out)
		if env.Error == nil || env.Error.Code != "device_unknown" {
			t.Fatalf("error = %+v; want device_unknown", env.Error)
		}
	})
}

func TestIngestOnceJSONGolden(t *testing.T) {
	run := func(t *testing.T, root string) (int, string, string) {
		t.Helper()
		dir := t.TempDir()
		return runCLI("ingest", "--once", "--json",
			"--db", filepath.Join(dir, "hub.db"),
			"--state", filepath.Join(dir, "state"),
			"--claude-root", root, "--codex-root", "", "--pi-root", "")
	}

	t.Run("success", func(t *testing.T) {
		e := newSliceEnv(t)
		copyFixture(t, e.session("main-session.jsonl"), "main-session.jsonl")
		code, out, stderr := run(t, e.root)
		if code != 0 || stderr != "" {
			t.Fatalf("code %d, stderr %q", code, stderr)
		}
		goldenCompare(t, out,
			`{"schema_version":1,"command":"ohya ingest","data":{"events":16,"sources":1,"failed":0,"stopped":0,"stops":[]},"error":null}`+"\n")
	})

	t.Run("degraded pass reports through data and exits 3", func(t *testing.T) {
		e := newSliceEnv(t)
		before := jsonFixtureLine("before")
		oversized := jsonFixtureLine(strings.Repeat("x", claudecode.DefaultMaxRecordBytes+512))
		writeFile(t, e.session("oversized.jsonl"), before+oversized+jsonFixtureLine("after"))
		code, out, stderr := run(t, e.root)
		if code != 3 {
			t.Fatalf("exit code %d; want 3 (stderr %q)", code, stderr)
		}
		// Diagnostics stay on stderr; the envelope carries the facts.
		if !strings.Contains(stderr, "--max-record-bytes") {
			t.Fatalf("stderr %q misses the stop diagnostic", stderr)
		}
		got := scrubJSONKeys(t, out, "source_id", "path")
		goldenCompare(t, got, `{"schema_version":1,"command":"ohya ingest","data":{"events":1,"sources":1,"failed":0,"stopped":1,"stops":[{"source_id":"<source_id>","path":"<path>","offset":`+strconv.Itoa(len(before))+`,"remedy":"increase_max_record_bytes"}]},"error":null}`+"\n")
	})

	t.Run("failure missing root", func(t *testing.T) {
		absent := filepath.Join(t.TempDir(), "absent")
		code, out, _ := run(t, absent)
		if code != 1 {
			t.Fatalf("exit code %d; want 1", code)
		}
		got := strings.ReplaceAll(out, filepath.Dir(absent), "<dir>")
		goldenCompare(t, got, `{"schema_version":1,"command":"ohya ingest","data":null,"error":{"code":"missing_agent_root","message":"claude-code session directory <dir>/absent does not exist; pass --claude-root","hint":"pass the named --<agent>-root flag, or create the directory"}}`+"\n")
	})

	t.Run("--json without --once is a usage error", func(t *testing.T) {
		dir := t.TempDir()
		code, out, stderr := runCLI("ingest", "--json",
			"--db", filepath.Join(dir, "hub.db"), "--claude-root", dir)
		if code != 2 || out != "" {
			t.Fatalf("code %d, stdout %q; want 2 and empty stdout", code, out)
		}
		if !strings.Contains(stderr, "--json needs --once") {
			t.Fatalf("stderr %q; want the --once requirement", stderr)
		}
	})
}

// TestJSONSharedEnvelopeDecoder is the T062 acceptance: every command's
// --json output decodes with the same envelope struct — exactly one object,
// schema_version 1, the command's name, and data xor error.
func TestJSONSharedEnvelopeDecoder(t *testing.T) {
	const deviceID = "0123456789abcdef0123456789abcdef"

	emptyDB := filepath.Join(t.TempDir(), "hub.db")
	freshState := t.TempDir()
	pairedState := t.TempDir()
	pairDevice(t, pairedState, deviceID)
	otherPaired := t.TempDir()
	pairDevice(t, otherPaired, "0123456789abcdef0123456789abcdea")
	e := newSliceEnv(t)
	copyFixture(t, e.session("main-session.jsonl"), "main-session.jsonl")
	ingestDir := t.TempDir()
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	samples := []struct {
		name      string
		args      []string
		command   string
		wantError bool
	}{
		{"version", []string{"version", "--json"}, "ohya version", false},
		{"rederive", []string{"rederive", "--json", "--db", emptyDB}, "ohya rederive", false},
		{"devices list", []string{"devices", "list", "--json", "--state", freshState}, "ohya devices list", false},
		{"devices invite", []string{"devices", "invite", "--json", "--state", pairedState}, "ohya devices invite", false},
		{"devices revoke", []string{"devices", "revoke", "--json", "--state", otherPaired, "0123456789abcdef0123456789abcdea"}, "ohya devices revoke", false},
		{"ingest --once", []string{"ingest", "--once", "--json", "--db", filepath.Join(ingestDir, "hub.db"), "--state", filepath.Join(ingestDir, "state"), "--claude-root", e.root, "--codex-root", "", "--pi-root", ""}, "ohya ingest", false},
		{"doctor", []string{"doctor", "--json", "--db", filepath.Join(ingestDir, "hub.db"), "--state", filepath.Join(ingestDir, "state")}, "ohya doctor", false},
		{"rederive failure", []string{"rederive", "--json", "--db", filepath.Join(blocker, "hub.db")}, "ohya rederive", true},
		{"devices list failure", []string{"devices", "list", "--json", "--state", blocker}, "ohya devices list", true},
		{"devices invite failure", []string{"devices", "invite", "--json", "--state", t.TempDir()}, "ohya devices invite", true},
		{"devices revoke failure", []string{"devices", "revoke", "--json", "--state", freshState, "ffffffffffffffffffffffffffffffff"}, "ohya devices revoke", true},
		{"ingest failure", []string{"ingest", "--once", "--json", "--db", filepath.Join(ingestDir, "hub2.db"), "--state", filepath.Join(ingestDir, "state2"), "--claude-root", filepath.Join(ingestDir, "absent"), "--codex-root", "", "--pi-root", ""}, "ohya ingest", true},
	}
	codeRe := regexp.MustCompile(`^[a-z0-9_]+$`)
	for _, s := range samples {
		t.Run(s.name, func(t *testing.T) {
			code, out, _ := runCLI(s.args...)
			if want := map[bool]int{false: 0, true: 1}[s.wantError]; code != want {
				t.Fatalf("exit code %d; want %d (stdout %q)", code, want, out)
			}
			env := decodeEnvelope(t, out)
			if env.SchemaVersion != jsonSchemaVersion {
				t.Fatalf("schema_version = %d; want %d", env.SchemaVersion, jsonSchemaVersion)
			}
			if env.Command != s.command {
				t.Fatalf("command = %q; want %q", env.Command, s.command)
			}
			if s.wantError {
				if env.Error == nil || !codeRe.MatchString(env.Error.Code) || env.Error.Message == "" {
					t.Fatalf("error = %+v; want a stable code and a message", env.Error)
				}
				if dataNull(env.Data) {
					return
				}
				t.Fatalf("data = %s; want null on failure", env.Data)
			}
			if env.Error != nil {
				t.Fatalf("error = %+v; want null on success", env.Error)
			}
			if dataNull(env.Data) {
				t.Fatalf("data = %s; want the command's report", env.Data)
			}
		})
	}
}
