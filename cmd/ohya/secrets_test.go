package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nibzard/ohya/internal/store"
)

func seedSecretsDatabase(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "hub", "hub.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	hub, err := store.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	secretA := "export GITHUB_TOKEN=ghp_" + strings.Repeat("f", 36)
	secretB := "AKIAIOSFODNN7EXAMPLE"
	secretC := "sk-ant-api03-" + strings.Repeat("C", 40)
	for _, q := range []string{
		`INSERT INTO sessions(id,agent,device_id,native_id,first_prompt)
		 VALUES('claude-code:dev1:sess-1','claude-code','dev1','sess-1','deploy')`,
		`INSERT INTO sessions(id,agent,device_id,native_id,first_prompt)
		 VALUES('codex:dev2:thread-9','codex','dev2','thread-9','keys')`,
		`INSERT INTO sources(id,device_id,agent,path) VALUES('src-1','dev1','claude-code','/a.jsonl')`,
		`INSERT INTO sources(id,device_id,agent,path) VALUES('src-2','dev2','codex','/b.jsonl')`,
		`INSERT INTO events(id,session_id,source_id,source_offset,kind,role,text,raw_json)
		 VALUES(1,'claude-code:dev1:sess-1','src-1',0,'message','user','deploy notes','{"type":"user","content":"clean record"}')`,
		`INSERT INTO events(id,session_id,source_id,source_offset,kind,role,text,raw_json)
		 VALUES(2,'claude-code:dev1:sess-1','src-1',60,'message','user','` + sqlString(secretA) + `','` +
			sqlString(`{"type":"user","content":"`+secretA+`"}`) + `')`,
		`INSERT INTO events(id,session_id,source_id,source_offset,kind,role,text,raw_json)
		 VALUES(3,'claude-code:dev1:sess-1','src-1',120,'message','user','aws key ` + sqlString(secretB) + `','` +
			sqlString(`{"type":"user","content":"aws key `+secretB+`"}`) + `')`,
		`INSERT INTO events(id,session_id,source_id,source_offset,kind,role,text,raw_json)
		 VALUES(4,'codex:dev2:thread-9','src-2',0,'message','assistant','provider key ` + sqlString(secretC) + `','` +
			sqlString(`{"type":"assistant","content":"provider key `+secretC+`"}`) + `')`,
	} {
		if _, err := hub.Writer().Exec(q); err != nil {
			hub.Close()
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	hub.Close()
	return dbPath
}

func TestSecretsScanReportsWithoutSecretMaterial(t *testing.T) {
	db := seedSecretsDatabase(t)

	var stdout, stderr bytes.Buffer
	code := cliMain([]string{"secrets", "scan", "--db", db}, &stdout, &stderr)
	if code != 0 || stderr.String() != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"3 matches in 3 events across 2 sessions",
		"patterns:",
		"github-token  1",
		"aws-access-key-id  1",
		"anthropic-api-key  1",
		"session claude-code:dev1:sess-1",
		"session codex:dev2:thread-9",
		"event 2  matches=1  patterns=github-token",
		"event 3  matches=1  patterns=aws-access-key-id",
		"event 4  matches=1  patterns=anthropic-api-key",
		"best effort",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("scan output lacks %q:\n%s", want, out)
		}
	}
	for _, secret := range []string{"ghp_", "AKIAIOSFODNN7EXAMPLE", "sk-ant-api03-"} {
		if strings.Contains(out, secret) {
			t.Fatalf("scan output holds secret material %q:\n%s", secret, out)
		}
	}
}

func TestSecretsScanJSONReport(t *testing.T) {
	db := seedSecretsDatabase(t)

	var stdout, stderr bytes.Buffer
	code := cliMain([]string{"secrets", "scan", "--db", db, "--json"}, &stdout, &stderr)
	if code != 0 || stderr.String() != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	var env struct {
		SchemaVersion int    `json:"schema_version"`
		Command       string `json:"command"`
		Data          struct {
			Matches  int `json:"matches"`
			Events   int `json:"events"`
			Patterns []struct {
				ID      string `json:"id"`
				Matches int    `json:"matches"`
			} `json:"patterns"`
			Sessions []struct {
				ID      string `json:"id"`
				Matches int    `json:"matches"`
				Events  []struct {
					ID       int64    `json:"id"`
					Matches  int      `json:"matches"`
					Patterns []string `json:"patterns"`
				} `json:"events"`
			} `json:"sessions"`
		} `json:"data"`
		Error any `json:"error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("decode JSON report: %v\n%s", err, stdout.String())
	}
	if env.SchemaVersion != 1 || env.Command != "ohya secrets scan" || env.Error != nil {
		t.Fatalf("envelope = %+v", env)
	}
	if env.Data.Matches != 3 || env.Data.Events != 3 || len(env.Data.Sessions) != 2 {
		t.Fatalf("data = %+v", env.Data)
	}
	if len(env.Data.Patterns) != 3 {
		t.Fatalf("patterns = %+v", env.Data)
	}
	if s := env.Data.Sessions[0]; s.ID != "claude-code:dev1:sess-1" || len(s.Events) != 2 || s.Matches != 2 {
		t.Fatalf("first session = %+v", s)
	}
	if strings.Contains(stdout.String(), "ghp_") || strings.Contains(stdout.String(), "AKIAIOSFODNN7EXAMPLE") {
		t.Fatalf("JSON report holds secret material:\n%s", stdout.String())
	}
}

func TestSecretsScanCleanDatabase(t *testing.T) {
	db := seedRecallDatabase(t)
	var stdout, stderr bytes.Buffer
	code := cliMain([]string{"secrets", "scan", "--db", db}, &stdout, &stderr)
	if code != 0 || stderr.String() != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "0 matches in 0 events") ||
		!strings.Contains(stdout.String(), "not proof that no secret is stored") {
		t.Fatalf("clean scan output = %q", stdout.String())
	}
}

func TestSecretsScanMissingDatabase(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "hub.db")
	var stdout, stderr bytes.Buffer
	code := cliMain([]string{"secrets", "scan", "--db", missing}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit = %d; want 1", code)
	}
	if !strings.Contains(stderr.String(), "does not exist") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("scan created %s; it must stay read-only", missing)
	}
}

func TestSecretsCommandSurface(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := cliMain([]string{"secrets"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "ohya secrets scan") {
		t.Fatalf("bare secrets = %d, %q", code, stdout.String())
	}
	stdout.Reset()
	if code := cliMain([]string{"secrets", "--help"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "report secret-shaped values") {
		t.Fatalf("parent help = %d, %q", code, stdout.String())
	}
	stdout.Reset()
	if code := cliMain([]string{"secrets", "scan", "--help"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "count secret-shaped values in stored records") {
		t.Fatalf("scan help = %d, %q", code, stdout.String())
	}
	stdout.Reset()
	if code := cliMain([]string{"secrets", "nosuch"}, &stdout, &stderr); code != 2 {
		t.Fatalf("unknown subcommand exit = %d; want 2", code)
	}
	stdout.Reset()
	if code := cliMain([]string{"secrets", "scan", "extra"}, &stdout, &stderr); code != 2 {
		t.Fatalf("unexpected argument exit = %d; want 2", code)
	}
}

func TestSecretsScanPagesBeyondOneBatch(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "hub.db")
	hub, err := store.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hub.Writer().Exec(
		`INSERT INTO sessions(id,agent,device_id,native_id,first_prompt) VALUES('claude-code:dev1:s','claude-code','dev1','s','p')`); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.Writer().Exec(
		`INSERT INTO sources(id,device_id,agent,path) VALUES('src-1','dev1','claude-code','/a.jsonl')`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < secretsScanPageSize+10; i++ {
		raw := `{"type":"user","content":"GITHUB_TOKEN=ghp_` + strings.Repeat("f", 36) + `"}`
		if _, err := hub.Writer().Exec(
			`INSERT INTO events(session_id,source_id,source_offset,kind,raw_json) VALUES('claude-code:dev1:s','src-1',?,'message',?)`,
			i, raw); err != nil {
			t.Fatal(err)
		}
	}
	hub.Close()

	var stdout, stderr bytes.Buffer
	code := cliMain([]string{"secrets", "scan", "--db", dbPath}, &stdout, &stderr)
	if code != 0 || stderr.String() != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "510 matches in 510 events across 1 sessions") {
		t.Fatalf("paged scan output = %q", stdout.String())
	}
}
