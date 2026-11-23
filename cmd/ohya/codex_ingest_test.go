package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nibzard/ohya/internal/claudecode"
	"github.com/nibzard/ohya/internal/codex"
	"github.com/nibzard/ohya/internal/store"
)

const (
	codexMainThread = "00000000-0000-7000-8000-0000000000c1"
	codexSubThread  = "00000000-0000-7000-8000-0000000000d1"
	codexTailThread = "00000000-0000-7000-8000-0000000000c4"
)

func newCodexEnv(t *testing.T) *sliceEnv {
	t.Helper()
	e := newSliceEnv(t)
	e.agent = codex.AgentName
	return e
}

func (e *sliceEnv) rollout(name string) string {
	return filepath.Join(e.root, "2026", "11", "13", name)
}

func copyCodexFixture(t *testing.T, dst, name string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "codex", name))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, dst, string(data))
}

func codexLine(ordinal int, role, text string) string {
	return fmt.Sprintf(`{"timestamp":"2026-11-13T10:31:00.000Z","ordinal":%d,"type":"response_item","payload":{"type":"message","role":%q,"content":[{"type":"input_text","text":%q}]}}`+"\n",
		ordinal, role, text)
}

func codexMeta(thread string) string {
	return fmt.Sprintf(`{"timestamp":"2026-11-13T10:30:00.000Z","ordinal":0,"type":"session_meta","payload":{"id":%q,"cwd":"/home/dev/example"}}`+"\n", thread)
}

func TestIngestCodexThreadAcrossRolloutFiles(t *testing.T) {
	e := newCodexEnv(t)
	mainPath := e.rollout("rollout-2026-11-13T09-00-00-" + codexMainThread + ".jsonl")
	pagePath := e.rollout("rollout-2026-11-13T10-30-00-" + codexMainThread + "_00000000-0000-7000-8000-0000000000e1.jsonl")
	copyCodexFixture(t, mainPath, "main-session.jsonl")
	copyCodexFixture(t, pagePath, "resumed-page.jsonl")
	copyCodexFixture(t, e.rollout("rollout-2026-11-13T09-01-00-"+codexSubThread+".jsonl"), "subagent-session.jsonl")

	stats, stderr := e.runPass(t)
	if stats.sources != 3 || stats.inserted != 24 || stderr != "" {
		t.Fatalf("first pass = %+v, stderr %q; want 24 rows from 3 sources", stats, stderr)
	}
	hub := openHub(t, e.dbPath)
	db := hub.Writer()
	dev := e.deviceID(t)
	mainSID := store.SessionID(dev, codex.AgentName, codexMainThread)
	if n := countRows(t, db, `SELECT count(*) FROM sessions`); n != 2 {
		t.Fatalf("sessions = %d, want the main thread and the subagent", n)
	}
	if n := countRows(t, db, `SELECT event_count FROM sessions WHERE id = ?`, mainSID); n != 17 {
		t.Fatalf("main thread event_count = %d, want 17 (14 + page meta + 2 new)", n)
	}
	if n := countRows(t, db, `SELECT count(*) FROM events WHERE duplicate_of IS NOT NULL AND ordinal IN (12, 13)`); n != 2 {
		t.Fatalf("duplicates = %d, want the page's 2 replayed ordinals", n)
	}
	var prompt string
	if err := db.QueryRow(`SELECT first_prompt FROM sessions WHERE id = ?`, mainSID).Scan(&prompt); err != nil ||
		prompt != "Add a Codex section to the README." {
		t.Fatalf("first_prompt = %q, %v", prompt, err)
	}

	appendFile(t, pagePath, codexLine(16, "user", "Appended after the first pass."))
	stats, _ = e.runPass(t)
	if stats.inserted != 1 {
		t.Fatalf("pass after append = %+v, want 1 event", stats)
	}
	if n := countRows(t, db, `SELECT count(*) FROM events WHERE session_id = ? AND ordinal = 16`, mainSID); n != 1 {
		t.Fatal("the appended record did not join the main thread")
	}

	if stats, _ := e.runPass(t); stats.inserted != 0 {
		t.Fatalf("idle pass = %+v", stats)
	}
}

func TestIngestCodexReenumeratedPage(t *testing.T) {
	e := newCodexEnv(t)
	copyCodexFixture(t, e.rollout("rollout-2026-11-13T09-00-00-"+codexMainThread+".jsonl"), "main-session.jsonl")
	copyCodexFixture(t, e.rollout("rollout-2026-11-13T10-40-00-"+codexMainThread+"_00000000-0000-7000-8000-0000000000e2.jsonl"), "reenumerated-page.jsonl")

	stats, stderr := e.runPass(t)
	if stats.sources != 2 || stats.inserted != 19 || stderr != "" {
		t.Fatalf("first pass = %+v, stderr %q; want 19 rows from 2 sources", stats, stderr)
	}
	hub := openHub(t, e.dbPath)
	db := hub.Writer()
	mainSID := store.SessionID(e.deviceID(t), codex.AgentName, codexMainThread)
	if n := countRows(t, db, `SELECT event_count FROM sessions WHERE id = ?`, mainSID); n != 19 {
		t.Fatalf("main thread event_count = %d, want 19 (no re-enumerated record suppressed)", n)
	}
	if n := countRows(t, db, `SELECT count(*) FROM events WHERE duplicate_of IS NOT NULL`); n != 0 {
		t.Fatalf("duplicates = %d, want 0", n)
	}
	if n := countRows(t, db, `SELECT count(*) FROM session_token_usage WHERE message_id = 'resp_fixture_0003'`); n != 1 {
		t.Fatalf("re-enumerated usage rows = %d, want 1", n)
	}
	var input int64
	if err := db.QueryRow(`SELECT sum(input_tokens + cache_read_input_tokens) FROM session_token_usage`).Scan(&input); err != nil || input != 3900 {
		t.Fatalf("input totals = %d, %v; want 3900 across all three responses", input, err)
	}
	for _, phrase := range []string{"no Codex section yet", "rolled back"} {
		if n := countRows(t, db, fmt.Sprintf(`SELECT count(*) FROM events_fts WHERE events_fts MATCH '"%s"'`, phrase)); n != 1 {
			t.Fatalf("search %q = %d hits, want 1", phrase, n)
		}
	}

	if stats, _ := e.runPass(t); stats.inserted != 0 {
		t.Fatalf("idle pass = %+v", stats)
	}
}

func TestIngestCodexIncompleteTail(t *testing.T) {
	e := newCodexEnv(t)
	path := e.rollout("rollout-2026-11-13T11-00-00-" + codexTailThread + ".jsonl")
	copyCodexFixture(t, path, "incomplete-tail.jsonl")

	stats, stderr := e.runPass(t)
	if stats.inserted != 3 || stderr != "" {
		t.Fatalf("pass = %+v, stderr %q; want 3 complete records", stats, stderr)
	}
	if _, cursor := soleAgentCheckpoint(t, e.stateDir, codex.AgentName); cursor != 797 {
		t.Fatalf("checkpoint cursor = %d, want 797", cursor)
	}

	appendFile(t, path, `ory/.","phase":"final"}}`+"\n")
	stats, _ = e.runPass(t)
	if stats.inserted != 1 {
		t.Fatalf("pass after completion = %+v, want 1 event", stats)
	}
	hub := openHub(t, e.dbPath)
	sid := store.SessionID(e.deviceID(t), codex.AgentName, codexTailThread)
	if n := countRows(t, hub.Writer(), `SELECT count(*) FROM events WHERE session_id = ?`, sid); n != 4 {
		t.Fatalf("thread events = %d, want 4", n)
	}
}

func TestIngestCodexMalformedLineStops(t *testing.T) {
	e := newCodexEnv(t)
	copyCodexFixture(t, e.rollout("rollout-bad.jsonl"), "malformed-line.jsonl")
	var stderr bytes.Buffer
	r, done := e.newRunner(t, &stderr)
	defer done()
	for i := 0; i < 2; i++ {
		stats, err := r.poll(context.Background(), e.dirs(), false)
		if err != nil {
			t.Fatal(err)
		}
		if want := []int{2, 0}[i]; stats.inserted != want {
			t.Fatalf("pass %d inserted %d, want %d", i, stats.inserted, want)
		}
	}
	if n := strings.Count(stderr.String(), "offset 502"); n != 1 {
		t.Fatalf("diagnostic reported %d times, want 1:\n%s", n, stderr.String())
	}
	if strings.Contains(stderr.String(), "unescaped") {
		t.Fatalf("diagnostic leaks transcript content:\n%s", stderr.String())
	}
	if _, cursor := soleAgentCheckpoint(t, e.stateDir, codex.AgentName); cursor != 502 {
		t.Fatalf("checkpoint cursor = %d, want 502", cursor)
	}
}

func TestIngestCodexReplacementAndTruncation(t *testing.T) {
	e := newCodexEnv(t)
	path := e.rollout("rollout-x.jsonl")
	writeFile(t, path, codexMeta("thread-a")+codexLine(1, "user", "first"))
	if stats, _ := e.runPass(t); stats.inserted != 2 {
		t.Fatalf("first pass = %+v", stats)
	}

	replaceFile(t, path, codexMeta("thread-b")+codexLine(1, "user", "replaced")+codexLine(2, "assistant", "more"))
	stats, stderr := e.runPass(t)
	if stats.inserted != 3 || !strings.Contains(stderr, "identity changed") {
		t.Fatalf("pass after replacement = %+v, stderr %q", stats, stderr)
	}

	if err := os.Truncate(path, int64(len(codexMeta("thread-b")))); err != nil {
		t.Fatal(err)
	}
	stats, stderr = e.runPass(t)
	if stats.inserted != 1 || !strings.Contains(stderr, "truncated") {
		t.Fatalf("pass after truncation = %+v, stderr %q", stats, stderr)
	}

	hub := openHub(t, e.dbPath)
	db := hub.Writer()
	if n := countRows(t, db, `SELECT count(*) FROM sources`); n != 3 {
		t.Fatalf("sources = %d, want one per incarnation", n)
	}
	if n := countRows(t, db, `SELECT count(*) FROM events e JOIN sessions s ON s.id = e.session_id
	    WHERE s.native_id = 'thread-b'`); n != 4 {
		t.Fatalf("thread-b events = %d, want 4", n)
	}
	appendFile(t, path, codexLine(1, "user", "replaced"))
	if stats, _ := e.runPass(t); stats.inserted != 1 {
		t.Fatalf("pass after append = %+v", stats)
	}
	if n := countRows(t, db, `SELECT count(*) FROM events e JOIN sessions s ON s.id = e.session_id
	    WHERE s.native_id = 'thread-b' AND e.duplicate_of IS NOT NULL`); n != 1 {
		t.Fatalf("thread-b duplicates = %d, want 1", n)
	}
}

func TestIngestCodexLargeFileSpansBatches(t *testing.T) {
	e := newCodexEnv(t)
	var b strings.Builder
	b.WriteString(codexMeta("thread-big"))
	pad := strings.Repeat("x", 64<<10)
	for i := 1; int64(b.Len()) <= maxBatchBytes+(1<<20); i++ {
		b.WriteString(codexLine(i, "user", pad))
	}
	writeFile(t, e.rollout("rollout-big.jsonl"), b.String())
	stats, stderr := e.runPass(t)
	if stderr != "" || stats.inserted < 2 {
		t.Fatalf("pass = %+v, stderr %q", stats, stderr)
	}
	hub := openHub(t, e.dbPath)
	if n := countRows(t, hub.Writer(), `SELECT count(DISTINCT session_id) FROM events`); n != 1 {
		t.Fatalf("sessions across batches = %d, want 1", n)
	}
	if n := countRows(t, hub.Writer(), `SELECT count(*) FROM events`); n != stats.inserted {
		t.Fatalf("events = %d, want %d", n, stats.inserted)
	}
}

func TestIngestCodexCrashBetweenCommitAndAcknowledge(t *testing.T) {
	e := newCodexEnv(t)
	path := e.rollout("rollout-main.jsonl")
	copyCodexFixture(t, path, "main-session.jsonl")

	cmd := exec.Command(os.Args[0], "-test.run=TestIngestCrashHelper")
	cmd.Env = append(os.Environ(),
		"OHYA_SLICE_CRASH_DB="+e.dbPath,
		"OHYA_SLICE_CRASH_STATE="+e.stateDir,
		"OHYA_SLICE_CRASH_ROOT="+e.root,
		"OHYA_SLICE_CRASH_AGENT="+codex.AgentName,
	)
	if err := cmd.Run(); !errors.As(err, new(*exec.ExitError)) {
		t.Fatalf("helper did not crash as intended: %v", err)
	}
	hub := openHub(t, e.dbPath)
	if n := countRows(t, hub.Writer(), `SELECT count(*) FROM events`); n != 14 {
		t.Fatalf("events after crash = %d, want 14", n)
	}
	if _, cursor := soleAgentCheckpoint(t, e.stateDir, codex.AgentName); cursor != 0 {
		t.Fatalf("checkpoint cursor after crash = %d, want 0", cursor)
	}

	stats, stderr := e.runPass(t)
	if stats.inserted != 0 || stderr != "" {
		t.Fatalf("restart = %+v, stderr %q; want nothing new", stats, stderr)
	}
	line := codexLine(14, "user", "after the crash")
	appendFile(t, path, line)
	if stats, _ := e.runPass(t); stats.inserted != 1 {
		t.Fatalf("pass after append = %+v", stats)
	}
	sid := store.SessionID(e.deviceID(t), codex.AgentName, codexMainThread)
	if n := countRows(t, hub.Writer(), `SELECT event_count FROM sessions WHERE id = ?`, sid); n != 15 {
		t.Fatalf("event_count = %d, want 15", n)
	}
	if _, cursor := soleAgentCheckpoint(t, e.stateDir, codex.AgentName); cursor != 4780+int64(len(line)) {
		t.Fatalf("checkpoint cursor after append = %d", cursor)
	}
}

func TestIngestWatchesBothAgents(t *testing.T) {
	e := newSliceEnv(t)
	copyFixture(t, e.session("main-session.jsonl"), "main-session.jsonl")
	codexRoot := filepath.Join(filepath.Dir(e.root), "codex-sessions")
	copyCodexFixture(t, filepath.Join(codexRoot, "2026", "11", "13", "rollout-a.jsonl"), "main-session.jsonl")
	var stdout bytes.Buffer
	err := runIngest(context.Background(), ingestOptions{
		dbPath: e.dbPath, stateDir: e.stateDir, once: true,
		roots: append(e.claudeRoots(), agentRoot{agent: codex.AgentName, flag: "codex-root",
			dirs: []agentDir{{dir: codexRoot}}}),
		stdout: &stdout, stderr: io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "imported 30 events from 2 sources") {
		t.Fatalf("summary = %q", stdout.String())
	}
	soleAgentCheckpoint(t, e.stateDir, claudecode.AgentName)
	soleAgentCheckpoint(t, e.stateDir, codex.AgentName)
}

func TestIngestRootRules(t *testing.T) {
	run := func(e *sliceEnv, roots ...agentRoot) error {
		return runIngest(context.Background(), ingestOptions{
			dbPath: e.dbPath, stateDir: e.stateDir, once: true, roots: roots,
			stdout: io.Discard, stderr: io.Discard,
		})
	}
	e := newSliceEnv(t)
	absent := filepath.Join(e.root, "absent")
	if err := run(e, agentRoot{agent: claudecode.AgentName, flag: "claude-root", dirs: []agentDir{{dir: e.root}}},
		agentRoot{agent: codex.AgentName, flag: "codex-root", dirs: []agentDir{{dir: absent, optional: true}}}); err != nil {
		t.Fatalf("missing optional root: %v", err)
	}
	e = newSliceEnv(t)
	err := run(e, agentRoot{agent: codex.AgentName, flag: "codex-root", dirs: []agentDir{{dir: absent}}})
	if err == nil || !strings.Contains(err.Error(), "pass --codex-root") {
		t.Fatalf("missing explicit root error = %v", err)
	}
	if _, err := os.Stat(e.dbPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("database exists after the failed run: %v", err)
	}
	err = run(e, agentRoot{agent: claudecode.AgentName, flag: "claude-root", dirs: []agentDir{{dir: absent, optional: true}}},
		agentRoot{agent: codex.AgentName, flag: "codex-root", dirs: []agentDir{{dir: absent, optional: true}}})
	if err == nil || !strings.Contains(err.Error(), "--claude-root or --codex-root") {
		t.Fatalf("all roots missing error = %v", err)
	}
	if err := run(e); err == nil {
		t.Fatal("a run without roots succeeded")
	}
	if err := run(e, agentRoot{agent: "opencode", flag: "x", dirs: []agentDir{{dir: e.root}}}); err == nil {
		t.Fatal("an agent without a parser was accepted")
	}
}

func TestDefaultCodexRoots(t *testing.T) {
	for _, tc := range []struct {
		name         string
		codexHome    string
		wantSessions string
		wantArchived string
	}{
		{"home default", "", filepath.Join("/home/u", ".codex", "sessions"), filepath.Join("/home/u", ".codex", "archived_sessions")},
		{"CODEX_HOME", "/opt/codex", filepath.Join("/opt/codex", "sessions"), filepath.Join("/opt/codex", "archived_sessions")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CODEX_HOME", tc.codexHome)
			if got := defaultCodexRoot("/home/u"); got != tc.wantSessions {
				t.Fatalf("sessions root = %q, want %q", got, tc.wantSessions)
			}
			if got := defaultCodexArchivedRoot("/home/u"); got != tc.wantArchived {
				t.Fatalf("archived root = %q, want %q", got, tc.wantArchived)
			}
		})
	}
}

func codexDefaultRoots(t *testing.T, codexHome string) []agentRoot {
	t.Helper()
	t.Setenv("CODEX_HOME", codexHome)
	fs := flag.NewFlagSet("ingest", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	f := newAgentRootFlags(fs, t.TempDir())
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	var roots []agentRoot
	for _, r := range f.roots(fs) {
		if r.agent == codex.AgentName {
			roots = append(roots, r)
		}
	}
	if len(roots) != 1 {
		t.Fatalf("codex roots = %+v, want one root", roots)
	}
	return roots
}

func TestIngestCodexDiscoversBothDefaultRoots(t *testing.T) {
	for _, tc := range []struct {
		name        string
		sessions    bool
		archived    bool
		wantEvents  int
		wantSources int
	}{
		{"both roots", true, true, 19, 2},
		{"only sessions exists", true, false, 14, 1},
		{"only archived exists", false, true, 5, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			codexHome := t.TempDir()
			if tc.sessions {
				copyCodexFixture(t, filepath.Join(codexHome, "sessions", "2026", "11", "13",
					"rollout-2026-11-13T09-00-00-"+codexMainThread+".jsonl"), "main-session.jsonl")
			}
			if tc.archived {
				copyCodexFixture(t, filepath.Join(codexHome, "archived_sessions",
					"rollout-2026-11-13T08-00-00-"+codexSubThread+".jsonl"), "subagent-session.jsonl")
			}
			dir := t.TempDir()
			var stdout, stderr bytes.Buffer
			err := runIngest(context.Background(), ingestOptions{
				dbPath: filepath.Join(dir, "hub.db"), stateDir: filepath.Join(dir, "state"),
				once: true, roots: codexDefaultRoots(t, codexHome),
				stdout: &stdout, stderr: &stderr,
			})
			if err != nil {
				t.Fatal(err)
			}
			want := fmt.Sprintf("ohya: imported %d events from %d sources\n", tc.wantEvents, tc.wantSources)
			if stdout.String() != want {
				t.Fatalf("summary = %q, want %q", stdout.String(), want)
			}
			if stderr.String() != "" {
				t.Fatalf("diagnostics: %s", stderr.String())
			}
		})
	}
}

func TestIngestCodexMoveBetweenDefaultRootsKeepsCheckpoint(t *testing.T) {
	for _, tc := range []struct {
		name string
		flip bool
	}{
		{"sessions to archived", false},
		{"archived to sessions", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			codexHome := t.TempDir()
			from := filepath.Join(codexHome, "sessions", "2026", "11", "13", "rollout-move.jsonl")
			to := filepath.Join(codexHome, "archived_sessions", "rollout-move.jsonl")
			if tc.flip {
				from, to = to, from
			}
			copyCodexFixture(t, from, "main-session.jsonl")
			roots := codexDefaultRoots(t, codexHome)
			dir := t.TempDir()
			run := func() (string, string) {
				t.Helper()
				var stdout, stderr bytes.Buffer
				err := runIngest(context.Background(), ingestOptions{
					dbPath: filepath.Join(dir, "hub.db"), stateDir: filepath.Join(dir, "state"),
					once: true, roots: roots, stdout: &stdout, stderr: &stderr,
				})
				if err != nil {
					t.Fatal(err)
				}
				return stdout.String(), stderr.String()
			}

			firstOut, firstErr := run()
			if want := "ohya: imported 14 events from 1 sources\n"; firstOut != want {
				t.Fatalf("first pass = %q, want %q", firstOut, want)
			}
			if firstErr != "" {
				t.Fatalf("first pass diagnostics: %s", firstErr)
			}

			if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(from, to); err != nil {
				t.Fatal(err)
			}
			appendLine := codexLine(14, "user", "appended after the move")
			appendFile(t, to, appendLine)

			stdout, stderr := run()
			if got, want := stdout, "ohya: imported 1 events from 1 sources\n"; got != want {
				t.Fatalf("pass after the move = %q, want %q: the checkpoint must survive the move", got, want)
			}
			if stderr != "" {
				t.Fatalf("pass after the move diagnostics: %s", stderr)
			}

			hub := openHub(t, filepath.Join(dir, "hub.db"))
			db := hub.Writer()
			if n := countRows(t, db, `SELECT count(*) FROM sources`); n != 1 {
				t.Fatalf("sources = %d, want 1 across the move", n)
			}
			var sourceID, path string
			if err := db.QueryRow(`SELECT id, path FROM sources`).Scan(&sourceID, &path); err != nil {
				t.Fatal(err)
			}
			wantDir := "archived_sessions"
			if tc.flip {
				wantDir = filepath.Join("sessions", "2026")
			}
			if !strings.Contains(filepath.ToSlash(path), wantDir) {
				t.Fatalf("registered path = %q, want it under %s", path, wantDir)
			}
			checkpointID, cursor := soleAgentCheckpoint(t, filepath.Join(dir, "state"), codex.AgentName)
			if checkpointID != sourceID {
				t.Fatalf("checkpoint source = %s, want the moved source %s", checkpointID, sourceID)
			}
			if cursor != 4780+int64(len(appendLine)) {
				t.Fatalf("checkpoint cursor = %d, want %d", cursor, 4780+len(appendLine))
			}
		})
	}
}

func TestIngestWatchedSourceDisappearanceDiagnostic(t *testing.T) {
	for _, tc := range []struct {
		name    string
		leave   func(t *testing.T, path, unwatched string)
		restore func(t *testing.T, path, unwatched string)
	}{
		{"deleted", func(t *testing.T, path, unwatched string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}, nil},
		{"moved to an unwatched directory", func(t *testing.T, path, unwatched string) {
			if err := os.Rename(path, filepath.Join(unwatched, "rollout-moved.jsonl")); err != nil {
				t.Fatal(err)
			}
		}, func(t *testing.T, path, unwatched string) {
			if err := os.Rename(filepath.Join(unwatched, "rollout-moved.jsonl"), path); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newCodexEnv(t)
			archived := filepath.Join(filepath.Dir(e.root), "codex-archived")
			unwatched := filepath.Join(filepath.Dir(e.root), "codex-unwatched")
			if err := os.MkdirAll(unwatched, 0o700); err != nil {
				t.Fatal(err)
			}
			dirs := []agentDir{{dir: e.root}, {dir: archived, optional: true}}
			path := e.rollout("rollout-gone.jsonl")
			copyCodexFixture(t, path, "main-session.jsonl")

			var stderr bytes.Buffer
			r, done := e.newRunner(t, &stderr)
			defer done()
			if _, err := r.poll(context.Background(), dirs, false); err != nil {
				t.Fatal(err)
			}
			if stderr.String() != "" {
				t.Fatalf("first pass diagnostics: %s", stderr.String())
			}
			sourceID, cursor := soleAgentCheckpoint(t, e.stateDir, codex.AgentName)

			tc.leave(t, path, unwatched)
			stats, err := r.poll(context.Background(), dirs, false)
			if err != nil {
				t.Fatal(err)
			}
			if stats.sources != 0 || stats.failed != 0 {
				t.Fatalf("pass after disappearance = %+v, want 0 sources and no failure", stats)
			}
			if n := strings.Count(stderr.String(), "disappeared"); n != 1 {
				t.Fatalf("diagnostic reported %d times, want 1:\n%s", n, stderr.String())
			}
			if !strings.Contains(stderr.String(), path) {
				t.Fatalf("diagnostic misses the watched path:\n%s", stderr.String())
			}
			if id, cur := soleAgentCheckpoint(t, e.stateDir, codex.AgentName); id != sourceID || cur != cursor {
				t.Fatalf("checkpoint = %s at %d, want %s at %d", id, cur, sourceID, cursor)
			}

			if _, err := r.poll(context.Background(), dirs, false); err != nil {
				t.Fatal(err)
			}
			if n := strings.Count(stderr.String(), "disappeared"); n != 1 {
				t.Fatalf("still-missing pass repeated the diagnostic:\n%s", stderr.String())
			}

			if tc.restore == nil {
				return
			}
			tc.restore(t, path, unwatched)
			stats, err = r.poll(context.Background(), dirs, false)
			if err != nil {
				t.Fatal(err)
			}
			if stats.sources != 1 || stats.inserted != 0 {
				t.Fatalf("pass after return = %+v, want the source to resume without re-importing", stats)
			}
			if n := strings.Count(stderr.String(), "disappeared"); n != 1 {
				t.Fatalf("return pass added a diagnostic:\n%s", stderr.String())
			}

			tc.leave(t, path, unwatched)
			if _, err := r.poll(context.Background(), dirs, false); err != nil {
				t.Fatal(err)
			}
			if n := strings.Count(stderr.String(), "disappeared"); n != 2 {
				t.Fatalf("second disappearance reported %d times, want a fresh diagnostic:\n%s", n, stderr.String())
			}
		})
	}
}

func TestIngestSourcesMissingAtStartupReportedAsOneCount(t *testing.T) {
	e := newCodexEnv(t)
	dirs := []agentDir{{dir: e.root}}
	first, second := e.rollout("rollout-first.jsonl"), e.rollout("rollout-second.jsonl")
	copyCodexFixture(t, first, "main-session.jsonl")
	copyCodexFixture(t, second, "main-session.jsonl")

	var stderr bytes.Buffer
	r, done := e.newRunner(t, &stderr)
	if _, err := r.poll(context.Background(), dirs, false); err != nil {
		t.Fatal(err)
	}
	done()
	for _, path := range []string{first, second} {
		if err := os.Rename(path, path+".gone"); err != nil {
			t.Fatal(err)
		}
	}

	stderr.Reset()
	r, done = e.newRunner(t, &stderr)
	defer done()
	for i := 0; i < 2; i++ {
		if _, err := r.poll(context.Background(), dirs, false); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(stderr.String(), "2 sources with saved checkpoints were already missing at startup"); n != 1 {
		t.Fatalf("startup count reported %d times, want once:\n%s", n, stderr.String())
	}
	if strings.Contains(stderr.String(), "disappeared") {
		t.Fatalf("sources missing at startup were reported one by one:\n%s", stderr.String())
	}

	third := e.rollout("rollout-third.jsonl")
	copyCodexFixture(t, third, "main-session.jsonl")
	if _, err := r.poll(context.Background(), dirs, false); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(third); err != nil {
		t.Fatal(err)
	}
	if _, err := r.poll(context.Background(), dirs, false); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(stderr.String(), third+": source"); n != 1 {
		t.Fatalf("a source that disappeared while running was reported %d times, want once:\n%s", n, stderr.String())
	}
}
