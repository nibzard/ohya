package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
		"claude-code:dev1:sess-1",
		"claude-code",
		"github.com/example/app",
		hitTime,
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
	if code != 0 || !strings.Contains(out, "codex:dev2:thread-9") {
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

func TestSearchSnippetCentersDeepMatch(t *testing.T) {
	db := seedRecallDatabase(t)
	hub, err := store.Open(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	longPrefix := strings.Repeat("filler word ", 40)
	_, err = hub.Writer().Exec(`INSERT INTO events(id,session_id,source_id,source_offset,time_ms,kind,role,raw_json,text)
		 VALUES(8,'claude-code:dev1:sess-1','src-1',6,`+itoa(1763000007000)+`,'message','assistant','{}',?)`,
		longPrefix+"decided on deepmatchneedle for recovery")
	hub.Close()
	if err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runRecallCLI(t, "search", "--db", db, "deepmatchneedle")
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if !strings.Contains(out, "deepmatchneedle") {
		t.Fatalf("search excerpt missed the deep match:\n%s", out)
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

func TestSearchHyphenatedTermNeedsQuotes(t *testing.T) {
	db := seedRecallDatabase(t)
	hub, err := store.Open(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	_, err = hub.Writer().Exec(`INSERT INTO events(id,session_id,source_id,source_offset,time_ms,kind,role,raw_json,text)
		 VALUES(7,'claude-code:dev1:sess-1','src-1',5,` + itoa(1763000005000) + `,'message','user','{}','rework the auth-rework branch under feature/auth-rework')`)
	hub.Close()
	if err != nil {
		t.Fatalf("seed the hyphenated event: %v", err)
	}

	code, _, errOut := runRecallCLI(t, "search", "--db", db, "auth-rework")
	if code != 1 {
		t.Fatalf("bare hyphenated term exit = %d; want 1 (stderr %q)", code, errOut)
	}
	for _, want := range []string{
		"no such column: rework",
		"double quotes",
		`ohya search '"auth-refresh"'`,
	} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("stderr = %q; want it to contain %q", errOut, want)
		}
	}

	code, out, errOut := runRecallCLI(t, "search", "--db", db, `"auth-rework"`)
	if code != 0 || errOut != "" {
		t.Fatalf("quoted hyphenated term exit = %d, stderr %q", code, errOut)
	}
	if !strings.Contains(out, "claude-code:dev1:sess-1") || !strings.Contains(out, "feature/auth-rework") {
		t.Fatalf("quoted hyphenated search missed the seeded event:\n%s", out)
	}

	code, _, errOut = runRecallCLI(t, "search", "--db", db, "feature/auth-rework")
	if code != 1 {
		t.Fatalf("bare slashed term exit = %d; want 1 (stderr %q)", code, errOut)
	}
	if !strings.Contains(errOut, "syntax error") || !strings.Contains(errOut, "double quotes") {
		t.Fatalf("stderr = %q; want the syntax error and the quoting hint", errOut)
	}
}

func TestSearchMissingDatabase(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "hub.db")
	code, _, errOut := runRecallCLI(t, "search", "--db", missing, "x")
	if code != 1 {
		t.Fatalf("exit = %d; want 1", code)
	}
	for _, want := range []string{"does not exist", "ohya setup"} {
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
		"claude-code:dev1:sess-1",
		"claude-code",
		"dev1",
		"sess-1",
		"github.com/example/app",
		"/home/dev/app",
		"main",
		"claude-opus-4-6",
		"Find primes",
		time.UnixMilli(1763000000000).UTC().Format(time.RFC3339),
		time.UnixMilli(1763000009000).UTC().Format(time.RFC3339),
		"5 events",
		"Newest 3 events, oldest first",
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
		if strings.HasPrefix(line, "Newest ") {
			inEvents = true
			continue
		}
		trimmed := strings.TrimSpace(line)
		if inEvents && trimmed != "" && !strings.HasPrefix(trimmed, ";") && !strings.HasPrefix(trimmed, "ID  ") {
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
		{"show flags before", []string{"show", "--db", db, "--last", "2", "claude-code:dev1:sess-1"}, "Newest 2 events"},
		{"show flags after", []string{"show", "claude-code:dev1:sess-1", "--db", db, "--last", "2"}, "Newest 2 events"},
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

func TestSearchJSONSuccessEmptyAndErrors(t *testing.T) {
	db := seedRecallDatabase(t)

	code, out, errOut := runRecallCLI(t, "search", "--db", db, "--json", "refresh")
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	env := decodeEnvelope(t, out)
	if env.Command != "ohya search" || env.Error != nil {
		t.Fatalf("envelope = %+v", env)
	}
	var got searchJSONData
	if err := json.Unmarshal(env.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Query != "refresh" || len(got.Matches) != 3 || got.Truncated {
		t.Fatalf("search json = %+v", got)
	}
	if got.Matches[0].SessionID != "claude-code:dev1:sess-1" || got.Matches[0].Agent != "claude-code" || got.Matches[0].TimeMS == nil {
		t.Fatalf("first match = %+v", got.Matches[0])
	}

	code, out, errOut = runRecallCLI(t, "search", "--db", db, "--json", "absentkeyword")
	if code != 0 || errOut != "" {
		t.Fatalf("empty search exit %d, stderr %q", code, errOut)
	}
	if !strings.Contains(out, `"matches":[]`) {
		t.Fatalf("empty matches must be [], got %s", out)
	}

	code, out, _ = runRecallCLI(t, "search", "--db", db, "--json", "auth-rework")
	if code != 1 {
		t.Fatalf("fts error exit = %d; want 1", code)
	}
	env = decodeEnvelope(t, out)
	if env.Error == nil || env.Error.Code != "fts_syntax_error" || !strings.Contains(env.Error.Hint, "double quotes") {
		t.Fatalf("fts error envelope = %+v", env)
	}
}

func TestShowJSONSuccessAndNotFound(t *testing.T) {
	db := seedRecallDatabase(t)

	code, out, errOut := runRecallCLI(t, "show", "--db", db, "--last", "3", "--json", "claude-code:dev1:sess-1")
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	env := decodeEnvelope(t, out)
	if env.Command != "ohya show" || env.Error != nil {
		t.Fatalf("envelope = %+v", env)
	}
	var got showJSONData
	if err := json.Unmarshal(env.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Session.ID != "claude-code:dev1:sess-1" || got.Session.EventCount != 5 || got.Session.Settled == nil || !*got.Session.Settled {
		t.Fatalf("session = %+v", got.Session)
	}
	if len(got.Events) != 3 {
		t.Fatalf("events len = %d; want 3", len(got.Events))
	}
	if !(got.Events[0].ID < got.Events[1].ID && got.Events[1].ID < got.Events[2].ID) {
		t.Fatalf("events not oldest-first: %+v", got.Events)
	}

	code, out, _ = runRecallCLI(t, "show", "--db", db, "--json", "nope:sess")
	if code != 1 {
		t.Fatalf("missing session exit = %d; want 1", code)
	}
	env = decodeEnvelope(t, out)
	if env.Error == nil || env.Error.Code != "session_not_found" {
		t.Fatalf("missing session envelope = %+v", env)
	}
}

func TestSessionsListsRecentExcludesSyntheticRedactsAndTracksSettled(t *testing.T) {
	db := seedRecallDatabase(t)
	hub, err := store.Open(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	liveMS := time.Now().UnixMilli()
	token := "ghp_" + strings.Repeat("A", 36)
	for _, q := range []string{
		`INSERT INTO sessions(id,agent,device_id,native_id,repo,cwd,branch,model,first_prompt,started_at_ms,last_event_at_ms,event_count)
		 VALUES('pi:dev1:live-1','pi','dev1','live-1','github.com/example/app','/home/dev/app','feat','claude-sonnet-4-5','Check token ` + token + `',` + itoa(liveMS-1000) + `,` + itoa(liveMS) + `,2)`,
		`INSERT INTO sessions(id,agent,device_id,native_id,cwd,first_prompt,started_at_ms,last_event_at_ms,event_count)
		 VALUES('claude-code:dev1:ohya-synthetic:src-1','claude-code','dev1','ohya-synthetic:src-1','/home/dev/app','synthetic',` + itoa(liveMS) + `,` + itoa(liveMS) + `,1)`,
	} {
		if _, err := hub.Writer().Exec(q); err != nil {
			hub.Close()
			t.Fatal(err)
		}
	}
	hub.Close()

	code, out, errOut := runRecallCLI(t, "sessions", "--db", db, "--json")
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	env := decodeEnvelope(t, out)
	if env.Command != "ohya sessions" || env.Error != nil {
		t.Fatalf("envelope = %+v", env)
	}
	var got sessionsJSONData
	if err := json.Unmarshal(env.Data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Sessions) != 3 {
		t.Fatalf("sessions len = %d; want 3 (synthetic excluded): %+v", len(got.Sessions), got.Sessions)
	}
	if got.Sessions[0].ID != "pi:dev1:live-1" {
		t.Fatalf("newest session = %s; want pi:dev1:live-1", got.Sessions[0].ID)
	}
	if got.Sessions[0].Settled == nil || *got.Sessions[0].Settled {
		t.Fatalf("live session settled = %v; want false", got.Sessions[0].Settled)
	}
	if strings.Contains(got.Sessions[0].FirstPrompt, token) || !strings.Contains(got.Sessions[0].FirstPrompt, "[redacted:github-token]") {
		t.Fatalf("first_prompt was not redacted: %q", got.Sessions[0].FirstPrompt)
	}
	if got.Redactions < 1 {
		t.Fatalf("redactions = %d; want >= 1", got.Redactions)
	}
	if got.Sessions[1].Settled == nil || !*got.Sessions[1].Settled {
		t.Fatalf("older session settled = %v; want true", got.Sessions[1].Settled)
	}

	code, textOut, errOut := runRecallCLI(t, "sessions", "--db", db, "--limit", "2", "--repo", "github.com/example/app")
	if code != 0 || errOut != "" {
		t.Fatalf("text sessions exit %d, stderr %q", code, errOut)
	}
	if !strings.Contains(textOut, "pi:dev1:live-1") || !strings.Contains(textOut, "active") || !strings.Contains(textOut, "settled") {
		t.Fatalf("text sessions output:\n%s", textOut)
	}
	if strings.Contains(textOut, "codex:dev2:thread-9") || strings.Contains(textOut, "ohya-synthetic") {
		t.Fatalf("filtered text output leaked unrelated/synthetic sessions:\n%s", textOut)
	}

	code, _, errOut = runRecallCLI(t, "sessions", "--db", db, "--limit", "0")
	if code != 2 || !strings.Contains(errOut, "--limit") {
		t.Fatalf("limit 0 exit=%d stderr=%q", code, errOut)
	}
}

func TestRecallFloat64WireNumbersDecodeCleanly(t *testing.T) {
	ts := float64(1763000002000)
	if got := recallTime(ts); got != time.UnixMilli(1763000002000).UTC().Format(time.RFC3339) {
		t.Fatalf("recallTime(float64) = %q", got)
	}
	res := buildSearchJSON("q", mcpserver.Result{
		Rows:     [][]any{{float64(42), "s1", "codex", "github.com/o/r", ts, "hello"}},
		RowCount: 1,
	})
	if len(res.Matches) != 1 || res.Matches[0].EventID != 42 || res.Matches[0].TimeMS == nil || *res.Matches[0].TimeMS != 1763000002000 {
		t.Fatalf("float64 search match = %+v", res.Matches)
	}
}

type recallBackendFunc func(context.Context, string) (mcpserver.Result, error)

func (f recallBackendFunc) Query(ctx context.Context, query string) (mcpserver.Result, error) {
	return f(ctx, query)
}

func TestRecallDeadlineCarriesTheEngineHint(t *testing.T) {
	const hint = "this query used LIKE and read events; LIKE on a large table reads every row: " +
		"filter session_id with = and search text with events_fts MATCH"
	backend := recallBackendFunc(func(context.Context, string) (mcpserver.Result, error) {
		return mcpserver.Result{}, fmt.Errorf("%w (5s per call): interrupted; %s", mcpserver.ErrDeadline, hint)
	})

	_, err := recallQuery(context.Background(), backend, `SELECT id FROM events WHERE text LIKE '%x%'`)
	var classified *classifiedError
	if !errors.As(err, &classified) || classified.code != "query_deadline" {
		t.Fatalf("error = %v, want query_deadline", err)
	}
	if classified.hint != "" {
		t.Fatalf("remedy = %q, want none: the engine error names the cause", classified.hint)
	}
	if !strings.Contains(err.Error(), hint) {
		t.Fatalf("error = %v, want the hint of the engine", err)
	}
}

func TestSQLQueryTextJSONRedactionAndSafeguards(t *testing.T) {
	db := seedRecallDatabase(t)
	hub, err := store.Open(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	token := "ghp_" + strings.Repeat("Z", 36)
	_, err = hub.Writer().Exec(`INSERT INTO events(id,session_id,source_id,source_offset,time_ms,kind,role,raw_json,text)
		 VALUES(9,'claude-code:dev1:sess-1','src-1',7,`+itoa(1763000008000)+`,'message','user','{}',?)`,
		"export GITHUB_TOKEN="+token)
	hub.Close()
	if err != nil {
		t.Fatal(err)
	}

	code, out, errOut := runRecallCLI(t, "sql", "--db", db, "SELECT agent, repo, count(*) AS n FROM sessions GROUP BY agent, repo ORDER BY agent")
	if code != 0 || errOut != "" {
		t.Fatalf("text sql exit %d, stderr %q", code, errOut)
	}
	for _, want := range []string{"agent", "repo", "claude-code", "github.com/example/app", "codex", "NULL"} {
		if !strings.Contains(out, want) {
			t.Fatalf("text sql output lacks %q:\n%s", want, out)
		}
	}

	code, out, errOut = runRecallCLI(t, "sql", "SELECT id, text FROM events WHERE id = 9", "--db", db, "--json")
	if code != 0 || errOut != "" {
		t.Fatalf("json sql exit %d, stderr %q", code, errOut)
	}
	env := decodeEnvelope(t, out)
	if env.Command != "ohya sql" || env.Error != nil {
		t.Fatalf("envelope = %+v", env)
	}
	var res mcpserver.Result
	if err := json.Unmarshal(env.Data, &res); err != nil {
		t.Fatal(err)
	}
	if res.RowCount != 1 || res.Redactions != 1 || res.Truncated {
		t.Fatalf("result = %+v", res)
	}
	gotText, _ := res.Rows[0][1].(string)
	if strings.Contains(gotText, token) || !strings.Contains(gotText, "[redacted:github-token]") {
		t.Fatalf("redacted cell = %q", gotText)
	}

	code, out, errOut = runRecallCLI(t, "sql", "--db", db, "SELECT id FROM events WHERE id = 999")
	if code != 0 || errOut != "" || !strings.Contains(out, "no rows") {
		t.Fatalf("empty sql = %d, %q, %q", code, out, errOut)
	}

	for _, tc := range []struct {
		name     string
		query    string
		wantCode string
	}{
		{"write denied", "DELETE FROM events", "query_denied"},
		{"purged_sources denied", "SELECT * FROM purged_sources", "query_denied"},
		{"multiple statements", "SELECT 1; SELECT 2", "multiple_statements"},
		{"empty query", "   ", "empty_query"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, _ := runRecallCLI(t, "sql", "--db", db, "--json", tc.query)
			if code != 1 {
				t.Fatalf("exit = %d; want 1 (stdout %q)", code, out)
			}
			env := decodeEnvelope(t, out)
			if env.Error == nil || env.Error.Code != tc.wantCode {
				t.Fatalf("error = %+v; want %q", env.Error, tc.wantCode)
			}
		})
	}
}

func TestSQLReadsStdinAndBoundsSize(t *testing.T) {
	db := seedRecallDatabase(t)

	var stdout, stderr bytes.Buffer
	err := runSQLCommand([]string{"--db", db, "--json", "-"}, strings.NewReader("SELECT count(*) AS n\nFROM sessions\n"), &stdout, &stderr)
	if err != nil {
		t.Fatalf("stdin sql failed: %v", err)
	}
	env := decodeEnvelope(t, stdout.String())
	if env.Command != "ohya sql" || env.Error != nil {
		t.Fatalf("envelope = %+v", env)
	}
	var res mcpserver.Result
	if err := json.Unmarshal(env.Data, &res); err != nil {
		t.Fatal(err)
	}
	if res.RowCount != 1 || recallValue(res.Rows[0][0]) != "2" {
		t.Fatalf("stdin sql result = %+v, want 2 sessions", res)
	}

	stdout.Reset()
	oversized := "SELECT '" + strings.Repeat("a", mcpserver.DefaultMaxQueryBytes+10) + "'"
	err = runSQLCommand([]string{"--db", db, "--json", "-"}, strings.NewReader(oversized), &stdout, &stderr)
	if err == nil {
		t.Fatal("oversized stdin query succeeded, want error")
	}
	env = decodeEnvelope(t, stdout.String())
	if env.Error == nil || env.Error.Code != "query_too_large" {
		t.Fatalf("oversized stdin error = %+v, want query_too_large", env.Error)
	}
}

func TestSQLRemoteWithoutPairingAndPrintNotes(t *testing.T) {
	code, _, errOut := runRecallCLI(t, "sql", "--remote", "--state", t.TempDir(), "SELECT 1")
	if code != 1 || !strings.Contains(errOut, "no pairing credential") {
		t.Fatalf("remote sql exit=%d stderr=%q", code, errOut)
	}

	var out bytes.Buffer
	printSQLResult(&out, mcpserver.Result{
		Columns:          []string{"text"},
		Rows:             [][]any{{"[redacted:github-token]"}},
		RowCount:         1,
		Truncated:        true,
		TruncationReason: mcpserver.TruncationReasonBytes,
		Redactions:       1,
	})
	got := out.String()
	if !strings.Contains(got, "; result truncated (byte_limit)") || !strings.Contains(got, "; 1 secret-shaped value(s) redacted") {
		t.Fatalf("printSQLResult output = %q", got)
	}
}

func TestCommandsBeforeSetupNameTheSetupStep(t *testing.T) {
	missing := filepath.Join(t.TempDir(), ".ohya", "hub.db")
	for _, args := range [][]string{
		{"search", "--db", missing, "anything"},
		{"sessions", "--db", missing},
		{"usage", "--db", missing, "--since", "24h"},
	} {
		code, _, errOut := runRecallCLI(t, args...)
		if code == 0 {
			t.Fatalf("%v succeeded without a hub database", args)
		}
		if !strings.Contains(errOut, "run ohya setup") {
			t.Fatalf("%v stderr = %q; want it to name ohya setup", args, errOut)
		}
		for _, stale := range []string{"ingest --once", "unable to open database file"} {
			if strings.Contains(errOut, stale) {
				t.Fatalf("%v stderr = %q; must not say %q", args, errOut, stale)
			}
		}
	}
}
