package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nibzard/ohya/internal/mcpserver"
	"github.com/nibzard/ohya/internal/setup"
	"github.com/nibzard/ohya/internal/store"
)

func seedRecallDatabase(t *testing.T) string {
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
	started := int64(1763000000000)
	for _, q := range []string{
		`INSERT INTO sessions(id,agent,device_id,native_id,repo,cwd,branch,model,first_prompt,started_at_ms,last_event_at_ms,event_count)
		 VALUES('claude-code:dev1:sess-1','claude-code','dev1','sess-1','github.com/example/app','/home/dev/app','main','claude-opus-4-6','Find primes',` + itoa(started) + `,` + itoa(started+9000) + `,5)`,
		`INSERT INTO sessions(id,agent,device_id,native_id,repo,cwd,first_prompt,started_at_ms,last_event_at_ms,event_count)
		 VALUES('codex:dev2:thread-9','codex','dev2','thread-9',NULL,'/home/dev/other','Fix the parser',` + itoa(started+5000) + `,` + itoa(started+8000) + `,1)`,
		`INSERT INTO sources(id,device_id,agent,path) VALUES('src-1','dev1','claude-code','/a.jsonl')`,
		`INSERT INTO sources(id,device_id,agent,path) VALUES('src-2','dev2','codex','/b.jsonl')`,
		`INSERT INTO events(id,session_id,source_id,source_offset,time_ms,kind,role,raw_json,text)
		 VALUES(1,'claude-code:dev1:sess-1','src-1',0,` + itoa(started+1000) + `,'message','user','{}','Please fix the authentication refresh flow')`,
		`INSERT INTO events(id,session_id,source_id,source_offset,time_ms,kind,role,raw_json,text)
		 VALUES(2,'claude-code:dev1:sess-1','src-1',1,` + itoa(started+2000) + `,'message','assistant','{}','The refresh token expires after 30 days')`,
		`INSERT INTO events(id,session_id,source_id,source_offset,time_ms,kind,role,tool_name,raw_json,text)
		 VALUES(3,'claude-code:dev1:sess-1','src-1',2,` + itoa(started+3000) + `,'message',NULL,'Edit','{}','path/to/auth.go')`,
		"INSERT INTO events(id,session_id,source_id,source_offset,time_ms,kind,role,raw_json,text) " +
			"VALUES(4,'claude-code:dev1:sess-1','src-1',3," + itoa(started+4000) + ",'message','assistant','{}','done \x1b[31mred\x1b[0m refresh')",
		`INSERT INTO events(id,session_id,source_id,source_offset,time_ms,kind,raw_json,duplicate_of)
		 VALUES(5,'claude-code:dev1:sess-1','src-1',4,` + itoa(started+2000) + `,'message','{}',2)`,
		`INSERT INTO events(id,session_id,source_id,source_offset,time_ms,kind,role,raw_json,text)
		 VALUES(6,'codex:dev2:thread-9','src-2',0,` + itoa(started+6000) + `,'response_item','assistant','{}','the parser rejects the malformed rollout')`,
	} {
		if _, err := hub.Writer().Exec(q); err != nil {
			hub.Close()
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	hub.Close()
	return dbPath
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}

func runRecallCLI(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := cliMain(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestSearchFindsAndFiltersSessions(t *testing.T) {
	db := seedRecallDatabase(t)
	hitTime := time.UnixMilli(1763000002000).UTC().Format(time.RFC3339)

	code, out, errOut := runRecallCLI(t, "search", "--db", db, "refresh")
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	for _, want := range []string{
		"session=claude-code:dev1:sess-1",
		"agent=claude-code",
		"repo=github.com/example/app",
		"time=" + hitTime,
		"The refresh token expires after 30 days",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("search output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "codex") {
		t.Fatalf("search output holds the non-matching agent:\n%s", out)
	}

	code, out, _ = runRecallCLI(t, "search", "--db", db, "parser")
	if code != 0 || !strings.Contains(out, "session=codex:dev2:thread-9") {
		t.Fatalf("agent-scoped search = %d, %q", code, out)
	}

	for _, args := range [][]string{
		{"--agent", "codex", "refresh"},
		{"--repo", "github.com/example/app", "parser"},
		{"--agent", "claude-code", "--repo", "github.com/other/x", "refresh"},
		{"absentkeyword"},
	} {
		code, out, _ := runRecallCLI(t, append([]string{"search", "--db", db}, args...)...)
		if code != 0 || !strings.Contains(out, "no matches") {
			t.Fatalf("filtered search %v = %d, %q; want no matches", args, code, out)
		}
	}
}

func TestSearchSanitizesExcerpt(t *testing.T) {
	db := seedRecallDatabase(t)
	code, out, errOut := runRecallCLI(t, "search", "--db", db, "done")
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if strings.ContainsRune(out, 0x1b) {
		t.Fatalf("search output holds a raw escape byte:\n%q", out)
	}
	if !strings.Contains(out, "done") || !strings.Contains(out, "red") {
		t.Fatalf("search output lost the excerpt words:\n%s", out)
	}
}

func TestSearchFtsSyntaxErrorIsAFailure(t *testing.T) {
	db := seedRecallDatabase(t)
	code, _, errOut := runRecallCLI(t, "search", "--db", db, "don't")
	if code != 1 {
		t.Fatalf("exit = %d; want 1 (stderr %q)", code, errOut)
	}
	if !strings.Contains(errOut, "syntax error") {
		t.Fatalf("stderr = %q; want the FTS5 syntax error", errOut)
	}
}

func TestSearchMissingDatabase(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "hub.db")
	code, _, errOut := runRecallCLI(t, "search", "--db", missing, "x")
	if code != 1 {
		t.Fatalf("exit = %d; want 1", code)
	}
	for _, want := range []string{"does not exist", "ohya ingest"} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("stderr = %q; want it to name %q", errOut, want)
		}
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatalf("search created %s; it must stay read-only", missing)
	}
}

func TestShowPrintsNewestEvents(t *testing.T) {
	db := seedRecallDatabase(t)
	session := "claude-code:dev1:sess-1"
	code, out, errOut := runRecallCLI(t, "show", "--db", db, "--last", "3", session)
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	for _, want := range []string{
		"session=claude-code:dev1:sess-1  agent=claude-code  device=dev1  native=sess-1",
		"repo=github.com/example/app  cwd=/home/dev/app  branch=main  model=claude-opus-4-6",
		"first prompt: Find primes",
		"started=" + time.UnixMilli(1763000000000).UTC().Format(time.RFC3339),
		"last=" + time.UnixMilli(1763000009000).UTC().Format(time.RFC3339),
		"events=5",
		"newest 3 events (oldest first):",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("show output lacks %q:\n%s", want, out)
		}
	}
	lines := eventLines(t, out)
	if len(lines) != 3 {
		t.Fatalf("show printed %d event lines; want 3:\n%s", len(lines), out)
	}
	ordered := strings.Index(out, "The refresh token expires") < strings.Index(out, "path/to/auth.go") &&
		strings.Index(out, "path/to/auth.go") < strings.Index(out, "done")
	if !ordered {
		t.Fatalf("show events are not oldest first:\n%s", out)
	}
	for _, line := range lines {
		if strings.Contains(line, "expires after 30 days") && strings.Count(line, "  ") < 2 {
			t.Fatalf("event line lacks its columns: %q", line)
		}
	}

	code, out, _ = runRecallCLI(t, "show", "--db", db, session)
	if code != 0 {
		t.Fatalf("default --last exit = %d", code)
	}
	if got := len(eventLines(t, out)); got != 4 {
		t.Fatalf("default show printed %d event lines; want the 4 non-duplicate events:\n%s", got, out)
	}

	_, out, _ = runRecallCLI(t, "show", "--db", db, "--last", "1", session)
	if got := len(eventLines(t, out)); got != 1 {
		t.Fatalf("--last 1 printed %d event lines:\n%s", got, out)
	}
	if !strings.Contains(out, "done") {
		t.Fatalf("--last 1 missed the newest event:\n%s", out)
	}
}

func eventLines(t *testing.T, out string) []string {
	t.Helper()
	var lines []string
	inEvents := false
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "newest ") {
			inEvents = true
			continue
		}
		if inEvents && strings.TrimSpace(line) != "" && !strings.HasPrefix(line, ";") {
			lines = append(lines, line)
		}
	}
	return lines
}

func TestShowUnknownSession(t *testing.T) {
	db := seedRecallDatabase(t)
	code, _, errOut := runRecallCLI(t, "show", "--db", db, "nope:sess")
	if code != 1 {
		t.Fatalf("exit = %d; want 1", code)
	}
	if !strings.Contains(errOut, "no session with id \"nope:sess\"") || !strings.Contains(errOut, "ohya search") {
		t.Fatalf("stderr = %q; want the missing-session error with the search hint", errOut)
	}
}

func TestRecallRemoteWithoutPairing(t *testing.T) {
	for _, cmd := range []string{"search", "show"} {
		args := []string{cmd, "--remote", "--state", t.TempDir()}
		if cmd == "search" {
			args = append(args, "x")
		} else {
			args = append(args, "s")
		}
		code, _, errOut := runRecallCLI(t, args...)
		if code != 1 {
			t.Fatalf("%s exit = %d; want 1", cmd, code)
		}
		if !strings.Contains(errOut, "no pairing credential") || !strings.Contains(errOut, "ohya join") {
			t.Fatalf("%s stderr = %q; want the unpaired state hint", cmd, errOut)
		}
	}
}

func TestRecallRemoteThroughSetupConnection(t *testing.T) {
	state := t.TempDir()
	if err := os.MkdirAll(filepath.Join(state, "setup"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := setup.SaveConnection(state, setup.Connection{Mode: "remote"}); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := runRecallCLI(t, "search", "--state", state, "x")
	if code != 1 {
		t.Fatalf("exit = %d; want 1", code)
	}
	if !strings.Contains(errOut, "no pairing credential") {
		t.Fatalf("stderr = %q; want the join state under <state>/join to answer", errOut)
	}
	if _, err := os.Stat(filepath.Join(state, "join")); !os.IsNotExist(err) {
		t.Fatalf("recall created %s; it must stay read-only", filepath.Join(state, "join"))
	}
}

func TestRecallParsesFlagsOnEitherSideOfPositional(t *testing.T) {
	db := seedRecallDatabase(t)
	cases := []struct {
		name    string
		args    []string
		wantOut string
	}{
		{"search flags before", []string{"search", "--db", db, "refresh"}, "refresh token"},
		{"search flags after", []string{"search", "refresh", "--db", db}, "refresh token"},
		{"show flags before", []string{"show", "--db", db, "--last", "2", "claude-code:dev1:sess-1"}, "newest 2 events"},
		{"show flags after", []string{"show", "claude-code:dev1:sess-1", "--db", db, "--last", "2"}, "newest 2 events"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errOut := runRecallCLI(t, tc.args...)
			if code != 0 || errOut != "" {
				t.Fatalf("exit %d, stderr %q", code, errOut)
			}
			if !strings.Contains(out, tc.wantOut) {
				t.Fatalf("output lacks %q:\n%s", tc.wantOut, out)
			}
		})
	}
}

func TestPrintSearchResultTruncationNote(t *testing.T) {
	var out bytes.Buffer
	printSearchResult(&out, mcpserver.Result{
		Columns: []string{"id"}, Rows: [][]any{{int64(1)}}, RowCount: 1,
		Truncated: true, TruncationReason: mcpserver.TruncationReasonRows,
	})
	if !strings.Contains(out.String(), "; result truncated ("+mcpserver.TruncationReasonRows+")") {
		t.Fatalf("output = %q; want the truncation note", out.String())
	}
}
