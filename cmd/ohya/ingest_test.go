package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nibzard/ohya/internal/claudecode"
	"github.com/nibzard/ohya/internal/device"
	"github.com/nibzard/ohya/internal/store"
	"github.com/nibzard/ohya/internal/watch"
)

const fixtureNativeID = "00000000-0000-4000-8000-0000000000a1"

func fixturePath(name string) string {
	return filepath.Join("..", "..", "testdata", "claude-code", name)
}

func copyFixture(t *testing.T, dst, name string) {
	t.Helper()
	data, err := os.ReadFile(fixturePath(name))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("append %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func replaceFile(t *testing.T, path, content string) {
	t.Helper()
	tmp := path + ".replacement"
	writeFile(t, tmp, content)
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

type sliceEnv struct {
	root           string
	dbPath         string
	stateDir       string
	agent          string
	maxRecordBytes int64
}

func newSliceEnv(t *testing.T) *sliceEnv {
	t.Helper()
	dir := t.TempDir()
	e := &sliceEnv{
		root:     filepath.Join(dir, "projects"),
		dbPath:   filepath.Join(dir, "hub", "hub.db"),
		stateDir: filepath.Join(dir, "state"),
		agent:    claudecode.AgentName,
	}
	if err := os.MkdirAll(e.root, 0o700); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *sliceEnv) claudeRoots() []agentRoot {
	return []agentRoot{{agent: claudecode.AgentName, dir: e.root, flag: "claude-root"}}
}

func (e *sliceEnv) session(name string) string {
	return filepath.Join(e.root, "fixture-project", name)
}

func (e *sliceEnv) newRunner(t *testing.T, stderr io.Writer) (*ingestor, func()) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(e.dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(e.stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	hub, err := store.Open(context.Background(), e.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	deviceID, err := device.LoadOrCreate(e.stateDir)
	if err != nil {
		hub.Close()
		t.Fatal(err)
	}
	w, err := watch.New(e.stateDir, e.agent, watch.Options{})
	if err != nil {
		hub.Close()
		t.Fatal(err)
	}
	return newIngestor(hub, w, e.agent, deviceID, e.maxRecordBytes, stderr), func() { hub.Close() }
}

func (e *sliceEnv) runPass(t *testing.T) (passStats, string) {
	t.Helper()
	var stderr bytes.Buffer
	r, done := e.newRunner(t, &stderr)
	defer done()
	stats, err := r.poll(context.Background(), e.root, false)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	return stats, stderr.String()
}

func openHub(t *testing.T, dbPath string) *store.DB {
	t.Helper()
	hub, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hub.Close() })
	return hub
}

func (e *sliceEnv) deviceID(t *testing.T) string {
	t.Helper()
	id, err := device.LoadOrCreate(e.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func countRows(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func soleSourceID(t *testing.T, db *sql.DB) (string, int64) {
	t.Helper()
	var id string
	var cursor int64
	if err := db.QueryRow(`SELECT id, cursor_bytes FROM sources`).Scan(&id, &cursor); err != nil {
		t.Fatal(err)
	}
	return id, cursor
}

func soleCheckpoint(t *testing.T, stateDir string) (string, int64) {
	t.Helper()
	return soleAgentCheckpoint(t, stateDir, claudecode.AgentName)
}

func soleAgentCheckpoint(t *testing.T, stateDir, agent string) (string, int64) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(stateDir, "sources", agent+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Sources map[string]struct {
			SourceID    string `json:"source_id"`
			CursorBytes int64  `json:"cursor_bytes"`
		} `json:"sources"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if len(state.Sources) != 1 {
		t.Fatalf("checkpoint holds %d sources, want 1", len(state.Sources))
	}
	for _, s := range state.Sources {
		return s.SourceID, s.CursorBytes
	}
	return "", 0
}

func TestIngestImportsHistoryThenPollsIncrementally(t *testing.T) {
	e := newSliceEnv(t)
	session := e.session("main-session.jsonl")
	copyFixture(t, session, "main-session.jsonl")

	stats, stderr := e.runPass(t)
	if stats.sources != 1 || stats.inserted != 16 {
		t.Fatalf("first pass = %+v, want 16 events from 1 source", stats)
	}
	if stderr != "" {
		t.Fatalf("first pass diagnostics: %s", stderr)
	}

	hub := openHub(t, e.dbPath)
	sid := store.SessionID(e.deviceID(t), claudecode.AgentName, fixtureNativeID)
	readers, err := hub.Reader()
	if err != nil {
		t.Fatal(err)
	}
	defer readers.Close()
	if n := countRows(t, readers, `SELECT count(*) FROM events`); n != 16 {
		t.Fatalf("reader sees %d events, want 16", n)
	}
	var prompt string
	if err := readers.QueryRow(`SELECT first_prompt FROM sessions WHERE id = ?`, sid).
		Scan(&prompt); err != nil || prompt != "Find the highest prime below 20." {
		t.Fatalf("first_prompt = %q, %v; want the fixture prompt", prompt, err)
	}
	var input, output int64
	if err := readers.QueryRow(
		`SELECT coalesce(sum(input_tokens), 0), coalesce(sum(output_tokens), 0)
		 FROM session_token_usage WHERE session_id = ?`, sid).Scan(&input, &output); err != nil {
		t.Fatal(err)
	}
	if input != 180 || output != 47 {
		t.Fatalf("token totals = %d/%d, want 180/47", input, output)
	}

	if _, cursor := soleCheckpoint(t, e.stateDir); cursor != 6528 {
		t.Fatalf("checkpoint cursor = %d, want 6528", cursor)
	}

	appendLine := `{"type":"user","sessionId":"` + fixtureNativeID +
		`","message":{"role":"user","content":"one more question"}}` + "\n"
	appendFile(t, session, appendLine)
	stats, stderr = e.runPass(t)
	if stats.inserted != 1 || stats.sources != 1 {
		t.Fatalf("incremental pass = %+v, want 1 event from 1 source", stats)
	}
	if stderr != "" {
		t.Fatalf("incremental pass diagnostics: %s", stderr)
	}
	if n := countRows(t, readers, `SELECT count(*) FROM events`); n != 17 {
		t.Fatalf("events after append = %d, want 17", n)
	}
	if n := countRows(t, readers, `SELECT event_count FROM sessions WHERE id = ?`, sid); n != 17 {
		t.Fatalf("event_count after append = %d, want 17", n)
	}
	if _, cursor := soleCheckpoint(t, e.stateDir); cursor != 6528+int64(len(appendLine)) {
		t.Fatalf("checkpoint cursor = %d, want %d", cursor, 6528+int64(len(appendLine)))
	}

	stats, _ = e.runPass(t)
	if stats.inserted != 0 {
		t.Fatalf("idle pass inserted %d events, want 0", stats.inserted)
	}
}

func TestIngestMainAndSubagentFilesShareOneSession(t *testing.T) {
	e := newSliceEnv(t)
	copyFixture(t, e.session("main-session.jsonl"), "main-session.jsonl")
	copyFixture(t, filepath.Join(e.root, "fixture-project", fixtureNativeID,
		"subagents", "agent-1.jsonl"), "subagent-session.jsonl")

	stats, stderr := e.runPass(t)
	if stats.sources != 2 || stats.inserted != 22 {
		t.Fatalf("pass = %+v, want 22 events from 2 sources", stats)
	}
	if stderr != "" {
		t.Fatalf("diagnostics: %s", stderr)
	}
	hub := openHub(t, e.dbPath)
	sid := store.SessionID(e.deviceID(t), claudecode.AgentName, fixtureNativeID)
	if n := countRows(t, hub.Writer(), `SELECT count(*) FROM sessions`); n != 2 {
		t.Fatalf("sessions = %d, want 2 (parent and synthetic)", n)
	}
	if n := countRows(t, hub.Writer(), `SELECT event_count FROM sessions WHERE id = ?`, sid); n != 20 {
		t.Fatalf("parent event_count = %d, want 20", n)
	}
	if n := countRows(t, hub.Writer(),
		`SELECT event_count FROM sessions WHERE native_id LIKE ?`,
		claudecode.SyntheticNativeIDPrefix+"%"); n != 2 {
		t.Fatalf("synthetic event_count = %d, want 2", n)
	}
}

func TestIngestHardlinkedBackupKeepsCheckpoint(t *testing.T) {
	e := newSliceEnv(t)
	session := e.session("main-session.jsonl")
	copyFixture(t, session, "main-session.jsonl")
	backup := filepath.Join(e.root, "fixture-project", "backup", "main-session.jsonl")
	if err := os.MkdirAll(filepath.Dir(backup), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(session, backup); err != nil {
		t.Skipf("hardlinks unavailable: %v", err)
	}

	stats, stderr := e.runPass(t)
	if stats.sources != 1 || stats.inserted != 16 {
		t.Fatalf("first pass = %+v, want 16 events from 1 source", stats)
	}
	if stderr != "" {
		t.Fatalf("first pass diagnostics: %s", stderr)
	}
	sourceID, cursor := soleCheckpoint(t, e.stateDir)
	if cursor != 6528 {
		t.Fatalf("checkpoint cursor = %d, want 6528", cursor)
	}

	if err := os.Remove(session); err != nil {
		t.Fatal(err)
	}
	stats, stderr = e.runPass(t)
	if stats.sources != 1 || stats.inserted != 0 {
		t.Fatalf("pass after link removal = %+v, want 0 events from 1 source", stats)
	}
	if stderr != "" {
		t.Fatalf("pass after link removal diagnostics: %s", stderr)
	}
	id, cursor := soleCheckpoint(t, e.stateDir)
	if id != sourceID || cursor != 6528 {
		t.Fatalf("checkpoint = %s at %d, want %s at 6528", id, cursor, sourceID)
	}
	hub := openHub(t, e.dbPath)
	if n := countRows(t, hub.Writer(), `SELECT count(*) FROM sources`); n != 1 {
		t.Fatalf("sources = %d, want 1", n)
	}
	if n := countRows(t, hub.Writer(), `SELECT count(*) FROM events`); n != 16 {
		t.Fatalf("events = %d, want 16 — history must not duplicate", n)
	}
}

func TestIngestIncompleteTailCommitsOnlyCompleteRecords(t *testing.T) {
	e := newSliceEnv(t)
	session := e.session("incomplete-tail.jsonl")
	copyFixture(t, session, "incomplete-tail.jsonl")

	stats, stderr := e.runPass(t)
	if stats.inserted != 3 {
		t.Fatalf("pass = %+v, want 3 complete records", stats)
	}
	if stderr != "" {
		t.Fatalf("an incomplete tail is not a diagnostic: %s", stderr)
	}
	if _, cursor := soleCheckpoint(t, e.stateDir); cursor != 1545 {
		t.Fatalf("checkpoint cursor = %d, want 1545", cursor)
	}

	appendFile(t, session, `"}]}}`+"\n")
	stats, _ = e.runPass(t)
	if stats.inserted != 1 {
		t.Fatalf("pass after completion = %+v, want 1 event", stats)
	}
	hub := openHub(t, e.dbPath)
	if n := countRows(t, hub.Writer(), `SELECT count(*) FROM events`); n != 4 {
		t.Fatalf("events = %d, want 4", n)
	}
	if _, cursor := soleCheckpoint(t, e.stateDir); cursor != 2003 {
		t.Fatalf("checkpoint cursor = %d, want 2003", cursor)
	}
}

func TestIngestMalformedLineStopsSourceAndOthersContinue(t *testing.T) {
	e := newSliceEnv(t)
	copyFixture(t, filepath.Join(e.root, "a-project", "bad.jsonl"), "malformed-line.jsonl")
	copyFixture(t, e.session("main-session.jsonl"), "main-session.jsonl")

	stats, stderr := e.runPass(t)
	if stats.sources != 2 || stats.inserted != 18 || stats.stopped != 1 || stats.failed != 0 {
		t.Fatalf("pass = %+v, want 18 events from 2 sources, 1 stopped", stats)
	}
	if !strings.Contains(stderr, "offset 1094") {
		t.Fatalf("diagnostic misses the stop offset:\n%s", stderr)
	}
	if !strings.Contains(stderr, "repair the record at this offset") {
		t.Fatalf("diagnostic misses the repair remedy (ADR 0004):\n%s", stderr)
	}
	if strings.Contains(stderr, "unterminated") {
		t.Fatalf("diagnostic leaks transcript content:\n%s", stderr)
	}

	hub := openHub(t, e.dbPath)
	if n := countRows(t, hub.Writer(), `SELECT count(*) FROM sources`); n != 2 {
		t.Fatalf("sources = %d, want 2", n)
	}
	if n := countRows(t, hub.Writer(),
		`SELECT count(*) FROM events e JOIN sources s ON e.source_id = s.id
		 WHERE s.path LIKE '%bad.jsonl'`); n != 2 {
		t.Fatalf("bad source events = %d, want 2", n)
	}

	stats, _ = e.runPass(t)
	if stats.inserted != 0 || stats.stopped != 1 {
		t.Fatalf("later pass = %+v, want 0 events, 1 stopped", stats)
	}
}

func TestIngestReportsPersistentDiagnosticOncePerRun(t *testing.T) {
	e := newSliceEnv(t)
	copyFixture(t, e.session("malformed-line.jsonl"), "malformed-line.jsonl")
	var stderr bytes.Buffer
	r, done := e.newRunner(t, &stderr)
	defer done()
	for i := 0; i < 2; i++ {
		if _, err := r.poll(context.Background(), e.root, false); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(stderr.String(), "offset 1094"); n != 1 {
		t.Fatalf("diagnostic reported %d times in one run, want 1:\n%s", n, stderr.String())
	}
}

func TestIngestOversizedRecordStopRecoversWithLargerLimit(t *testing.T) {
	e := newSliceEnv(t)
	session := e.session("oversized.jsonl")
	line := func(content string) string {
		return fmt.Sprintf(`{"type":"user","sessionId":%q,"message":{"role":"user","content":%q}}`+"\n",
			fixtureNativeID, content)
	}
	before := line("before")
	oversized := line(strings.Repeat("x", claudecode.DefaultMaxRecordBytes+512))
	after := line("after")
	writeFile(t, session, before+oversized+after)

	stats, stderr := e.runPass(t)
	if stats.inserted != 1 || stats.stopped != 1 || stats.failed != 0 {
		t.Fatalf("pass = %+v, want 1 event before the stop, 1 stopped", stats)
	}
	if !strings.Contains(stderr, "record size limit") ||
		!strings.Contains(stderr, "--max-record-bytes") ||
		!strings.Contains(stderr, "docs/adr/0004") {
		t.Fatalf("diagnostic misses the remedy:\n%s", stderr)
	}
	if _, cursor := soleCheckpoint(t, e.stateDir); cursor != int64(len(before)) {
		t.Fatalf("checkpoint cursor = %d, want the stop offset %d", cursor, len(before))
	}

	e.maxRecordBytes = claudecode.DefaultMaxRecordBytes + 4096
	stats, stderr = e.runPass(t)
	if stats.inserted != 2 || stats.stopped != 0 || stats.failed != 0 {
		t.Fatalf("recovery pass = %+v, want 2 events past the stop, nothing degraded", stats)
	}
	if stderr != "" {
		t.Fatalf("recovery pass diagnostics: %s", stderr)
	}
	hub := openHub(t, e.dbPath)
	if n := countRows(t, hub.Writer(), `SELECT count(*) FROM events`); n != 3 {
		t.Fatalf("events = %d, want 3", n)
	}
	if _, cursor := soleCheckpoint(t, e.stateDir); cursor != int64(len(before+oversized+after)) {
		t.Fatalf("checkpoint cursor = %d, want the file size", cursor)
	}
}

func TestIngestUnreadableSourceCountsFailed(t *testing.T) {
	e := newSliceEnv(t)
	copyFixture(t, e.session("main-session.jsonl"), "main-session.jsonl")
	locked := e.session("locked.jsonl")
	copyFixture(t, locked, "main-session.jsonl")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	if f, err := os.Open(locked); err == nil {
		f.Close()
		t.Skipf("chmod 0 left %s readable", locked)
	}

	stats, stderr := e.runPass(t)
	if stats.sources != 2 || stats.inserted != 16 || stats.failed != 1 || stats.stopped != 0 {
		t.Fatalf("pass = %+v, want 16 events from 2 sources, 1 failed", stats)
	}
	if !strings.Contains(stderr, locked) {
		t.Fatalf("diagnostic misses the unreadable source:\n%s", stderr)
	}
}

func TestIngestOnceDegradedReturnsErrDegraded(t *testing.T) {
	e := newSliceEnv(t)
	copyFixture(t, e.session("malformed-line.jsonl"), "malformed-line.jsonl")
	var stdout bytes.Buffer
	err := runIngest(context.Background(), ingestOptions{
		dbPath: e.dbPath, stateDir: e.stateDir, roots: e.claudeRoots(),
		once: true, stdout: &stdout, stderr: io.Discard,
	})
	if !errors.Is(err, errDegraded) {
		t.Fatalf("error = %v, want errDegraded", err)
	}
	if got, want := stdout.String(), "ohya: imported 2 events from 1 sources; 0 failed, 1 stopped\n"; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
}

func TestIngestOnceExitCodes(t *testing.T) {
	line := func(content string) string {
		return fmt.Sprintf(`{"type":"user","sessionId":%q,"message":{"role":"user","content":%q}}`+"\n",
			fixtureNativeID, content)
	}
	run := func(t *testing.T, root string) (int, string, string) {
		t.Helper()
		dir := t.TempDir()
		var stdout, stderr bytes.Buffer
		cmd := exec.Command(os.Args[0], "child-run", "ingest", "--once",
			"--db", filepath.Join(dir, "hub.db"),
			"--state", filepath.Join(dir, "state"),
			"--claude-root", root, "--codex-root", "", "--pi-root", "")
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		runErr := cmd.Run()
		code := 0
		if runErr != nil {
			var exitErr *exec.ExitError
			if !errors.As(runErr, &exitErr) {
				t.Fatal(runErr)
			}
			code = exitErr.ExitCode()
		}
		return code, stdout.String(), stderr.String()
	}

	t.Run("all sources import", func(t *testing.T) {
		e := newSliceEnv(t)
		copyFixture(t, e.session("main-session.jsonl"), "main-session.jsonl")
		code, stdout, stderr := run(t, e.root)
		if code != 0 {
			t.Fatalf("exit code = %d, want 0 (stderr: %q)", code, stderr)
		}
		if got, want := stdout, "ohya: imported 16 events from 1 sources\n"; got != want {
			t.Fatalf("stdout = %q, want %q", got, want)
		}
		if stderr != "" {
			t.Fatalf("stderr = %q, want empty", stderr)
		}
	})

	t.Run("stopped source", func(t *testing.T) {
		e := newSliceEnv(t)
		session := e.session("oversized.jsonl")
		oversized := line(strings.Repeat("x", claudecode.DefaultMaxRecordBytes+512))
		writeFile(t, session, line("before")+oversized+line("after"))
		code, stdout, stderr := run(t, e.root)
		if code != 3 {
			t.Fatalf("exit code = %d, want 3 (stderr: %q)", code, stderr)
		}
		if !strings.Contains(stdout, "; 0 failed, 1 stopped") {
			t.Fatalf("summary misses the stopped count: %q", stdout)
		}
		if !strings.Contains(stderr, "--max-record-bytes") {
			t.Fatalf("stderr misses the stop diagnostic: %q", stderr)
		}
	})

	t.Run("unreadable source", func(t *testing.T) {
		e := newSliceEnv(t)
		copyFixture(t, e.session("main-session.jsonl"), "main-session.jsonl")
		locked := e.session("locked.jsonl")
		copyFixture(t, locked, "main-session.jsonl")
		if err := os.Chmod(locked, 0); err != nil {
			t.Fatal(err)
		}
		if f, err := os.Open(locked); err == nil {
			f.Close()
			t.Skipf("chmod 0 left %s readable", locked)
		}
		code, stdout, stderr := run(t, e.root)
		if code != 3 {
			t.Fatalf("exit code = %d, want 3 (stderr: %q)", code, stderr)
		}
		if !strings.Contains(stdout, "; 1 failed, 0 stopped") {
			t.Fatalf("summary misses the failed count: %q", stdout)
		}
		if !strings.Contains(stderr, "locked.jsonl") {
			t.Fatalf("stderr misses the failure diagnostic: %q", stderr)
		}
	})
}

func TestIngestRecordAboveBatchBoundStillIngests(t *testing.T) {
	e := newSliceEnv(t)
	e.maxRecordBytes = maxBatchBytes + 2<<20
	session := e.session("huge.jsonl")
	line := func(content string) string {
		return fmt.Sprintf(`{"type":"user","sessionId":%q,"message":{"role":"user","content":%q}}`+"\n",
			fixtureNativeID, content)
	}
	content := line("before") +
		line(strings.Repeat("x", int(maxBatchBytes)+1<<20)) +
		line("after")
	writeFile(t, session, content)

	stats, stderr := e.runPass(t)
	if stats.inserted != 3 {
		t.Fatalf("pass = %+v, want 3 events including the huge record", stats)
	}
	if stderr != "" {
		t.Fatalf("diagnostics: %s", stderr)
	}
	hub := openHub(t, e.dbPath)
	if n := countRows(t, hub.Writer(), `SELECT count(*) FROM events`); n != 3 {
		t.Fatalf("events = %d, want 3", n)
	}
	if _, cursor := soleCheckpoint(t, e.stateDir); cursor != int64(len(content)) {
		t.Fatalf("checkpoint cursor = %d, want the file size", cursor)
	}
}

func TestIngestReplacementAndTruncationReimportUnderNewSources(t *testing.T) {
	e := newSliceEnv(t)
	path := e.session("session.jsonl")
	line := func(content, sid string) string {
		return fmt.Sprintf(`{"type":"user","sessionId":%q,"message":{"role":"user","content":%q}}`+"\n", sid, content)
	}
	writeFile(t, path, line("first", "sess-1")+line("second", "sess-1"))

	stats, _ := e.runPass(t)
	if stats.inserted != 2 {
		t.Fatalf("first pass = %+v, want 2 events", stats)
	}

	replaceFile(t, path, line("replacement one", "sess-2")+line("replacement two", "sess-2"))
	stats, stderr := e.runPass(t)
	if stats.inserted != 2 {
		t.Fatalf("pass after replacement = %+v, want 2 reimported events", stats)
	}
	if !strings.Contains(stderr, "identity changed") {
		t.Fatalf("replacement went unreported:\n%s", stderr)
	}

	if err := os.Truncate(path, int64(len(line("replacement one", "sess-2")))); err != nil {
		t.Fatal(err)
	}
	stats, stderr = e.runPass(t)
	if stats.inserted != 1 {
		t.Fatalf("pass after truncation = %+v, want 1 reimported event", stats)
	}
	if !strings.Contains(stderr, "truncated") {
		t.Fatalf("truncation went unreported:\n%s", stderr)
	}

	hub := openHub(t, e.dbPath)
	if n := countRows(t, hub.Writer(), `SELECT count(*) FROM sources`); n != 3 {
		t.Fatalf("sources = %d, want 3 (one per incarnation)", n)
	}
	if n := countRows(t, hub.Writer(), `SELECT count(*) FROM events`); n != 5 {
		t.Fatalf("events = %d, want 5", n)
	}
	if n := countRows(t, hub.Writer(),
		`SELECT count(*) FROM events e JOIN sessions s ON e.session_id = s.id
		 WHERE s.native_id = 'sess-2'`); n != 3 {
		t.Fatalf("sess-2 events = %d, want 3", n)
	}
}

func TestIngestCrashBetweenCommitAndAcknowledge(t *testing.T) {
	e := newSliceEnv(t)
	copyFixture(t, e.session("main-session.jsonl"), "main-session.jsonl")

	cmd := exec.Command(os.Args[0], "-test.run=TestIngestCrashHelper")
	cmd.Env = append(os.Environ(),
		"OHYA_SLICE_CRASH_DB="+e.dbPath,
		"OHYA_SLICE_CRASH_STATE="+e.stateDir,
		"OHYA_SLICE_CRASH_ROOT="+e.root,
	)
	if err := cmd.Run(); !errors.As(err, new(*exec.ExitError)) {
		t.Fatalf("helper did not crash as intended: %v", err)
	}

	hub := openHub(t, e.dbPath)
	if n := countRows(t, hub.Writer(), `SELECT count(*) FROM events`); n != 16 {
		t.Fatalf("events after crash = %d, want 16", n)
	}
	_, dbCursor := soleSourceID(t, hub.Writer())
	if dbCursor != 6528 {
		t.Fatalf("database cursor after crash = %d, want 6528", dbCursor)
	}
	if _, cursor := soleCheckpoint(t, e.stateDir); cursor != 0 {
		t.Fatalf("checkpoint cursor after crash = %d, want 0", cursor)
	}

	stats, stderr := e.runPass(t)
	if stats.inserted != 0 {
		t.Fatalf("restart after crash inserted %d events, want 0", stats.inserted)
	}
	if stderr != "" {
		t.Fatalf("restart diagnostics: %s", stderr)
	}
	if n := countRows(t, hub.Writer(), `SELECT count(*) FROM events`); n != 16 {
		t.Fatalf("events after restart = %d, want 16", n)
	}
	sid := store.SessionID(e.deviceID(t), claudecode.AgentName, fixtureNativeID)
	if n := countRows(t, hub.Writer(), `SELECT event_count FROM sessions WHERE id = ?`, sid); n != 16 {
		t.Fatalf("event_count after restart = %d, want 16", n)
	}
	if _, cursor := soleCheckpoint(t, e.stateDir); cursor != 6528 {
		t.Fatalf("checkpoint cursor after restart = %d, want 6528", cursor)
	}
}

func TestIngestCrashHelper(t *testing.T) {
	db := os.Getenv("OHYA_SLICE_CRASH_DB")
	if db == "" {
		t.Skip("crash helper runs only under TestIngestCrashBetweenCommitAndAcknowledge")
	}
	state := os.Getenv("OHYA_SLICE_CRASH_STATE")
	root := os.Getenv("OHYA_SLICE_CRASH_ROOT")
	ctx := context.Background()
	if err := os.MkdirAll(filepath.Dir(db), 0o700); err != nil {
		panic(err)
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		panic(err)
	}
	hub, err := store.Open(ctx, db)
	if err != nil {
		panic(err)
	}
	deviceID, err := device.LoadOrCreate(state)
	if err != nil {
		panic(err)
	}
	agent := os.Getenv("OHYA_SLICE_CRASH_AGENT")
	if agent == "" {
		agent = claudecode.AgentName
	}
	w, err := watch.New(state, agent, watch.Options{})
	if err != nil {
		panic(err)
	}
	r := newIngestor(hub, w, agent, deviceID, 0, io.Discard)
	candidates, err := discoverSessionFiles(root)
	if err != nil {
		panic(err)
	}
	outcomes, err := w.Poll(candidates)
	if err != nil || len(outcomes) != 1 {
		panic(fmt.Sprintf("poll: %v, %d outcomes", err, len(outcomes)))
	}
	o := outcomes[0]
	if err := hub.RegisterSource(ctx, o.SourceID, deviceID, agent, o.Path); err != nil {
		panic(err)
	}
	inserted, committed, stop, err := r.ingestSource(ctx, o)
	if err != nil || stop != nil || inserted == 0 || committed != o.End {
		panic(fmt.Sprintf("ingestSource = %d inserts, cursor %d, stopped %t, %v",
			inserted, committed, stop != nil, err))
	}
	os.Exit(3)
}

func TestIngestCrashBetweenAcknowledgeAndFlush(t *testing.T) {
	e := newSliceEnv(t)
	copyFixture(t, e.session("main-session.jsonl"), "main-session.jsonl")

	cmd := exec.Command(os.Args[0], "-test.run=TestIngestCrashAckHelper")
	cmd.Env = append(os.Environ(),
		"OHYA_SLICE_CRASH_DB="+e.dbPath,
		"OHYA_SLICE_CRASH_STATE="+e.stateDir,
		"OHYA_SLICE_CRASH_ROOT="+e.root,
	)
	if err := cmd.Run(); !errors.As(err, new(*exec.ExitError)) {
		t.Fatalf("helper did not crash as intended: %v", err)
	}

	hub := openHub(t, e.dbPath)
	if n := countRows(t, hub.Writer(), `SELECT count(*) FROM events`); n != 16 {
		t.Fatalf("events after crash = %d, want 16", n)
	}
	_, dbCursor := soleSourceID(t, hub.Writer())
	if dbCursor != 6528 {
		t.Fatalf("database cursor after crash = %d, want 6528", dbCursor)
	}
	if _, cursor := soleCheckpoint(t, e.stateDir); cursor != 0 {
		t.Fatalf("checkpoint cursor after an unflushed acknowledgment and crash = %d, want 0", cursor)
	}

	stats, stderr := e.runPass(t)
	if stats.inserted != 0 {
		t.Fatalf("restart after crash inserted %d events, want 0", stats.inserted)
	}
	if stderr != "" {
		t.Fatalf("restart diagnostics: %s", stderr)
	}
	if n := countRows(t, hub.Writer(), `SELECT count(*) FROM events`); n != 16 {
		t.Fatalf("events after restart = %d, want 16", n)
	}
	sid := store.SessionID(e.deviceID(t), claudecode.AgentName, fixtureNativeID)
	if n := countRows(t, hub.Writer(), `SELECT event_count FROM sessions WHERE id = ?`, sid); n != 16 {
		t.Fatalf("event_count after restart = %d, want 16", n)
	}
	if _, cursor := soleCheckpoint(t, e.stateDir); cursor != 6528 {
		t.Fatalf("checkpoint cursor after restart = %d, want 6528", cursor)
	}
}

func TestIngestCrashAckHelper(t *testing.T) {
	db := os.Getenv("OHYA_SLICE_CRASH_DB")
	if db == "" {
		t.Skip("crash helper runs only under TestIngestCrashBetweenAcknowledgeAndFlush")
	}
	state := os.Getenv("OHYA_SLICE_CRASH_STATE")
	root := os.Getenv("OHYA_SLICE_CRASH_ROOT")
	ctx := context.Background()
	if err := os.MkdirAll(filepath.Dir(db), 0o700); err != nil {
		panic(err)
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		panic(err)
	}
	hub, err := store.Open(ctx, db)
	if err != nil {
		panic(err)
	}
	deviceID, err := device.LoadOrCreate(state)
	if err != nil {
		panic(err)
	}
	agent := claudecode.AgentName
	w, err := watch.New(state, agent, watch.Options{})
	if err != nil {
		panic(err)
	}
	r := newIngestor(hub, w, agent, deviceID, 0, io.Discard)
	candidates, err := discoverSessionFiles(root)
	if err != nil {
		panic(err)
	}
	outcomes, err := w.Poll(candidates)
	if err != nil || len(outcomes) != 1 {
		panic(fmt.Sprintf("poll: %v, %d outcomes", err, len(outcomes)))
	}
	o := outcomes[0]
	if err := hub.RegisterSource(ctx, o.SourceID, deviceID, agent, o.Path); err != nil {
		panic(err)
	}
	inserted, committed, stop, err := r.ingestSource(ctx, o)
	if err != nil || stop != nil || inserted == 0 || committed != o.End {
		panic(fmt.Sprintf("ingestSource = %d inserts, cursor %d, stopped %t, %v",
			inserted, committed, stop != nil, err))
	}
	if err := w.Acknowledge(o.Path, o.SourceID, committed); err != nil {
		panic(err)
	}
	os.Exit(3)
}

func TestIngestRunsUntilCanceled(t *testing.T) {
	e := newSliceEnv(t)
	copyFixture(t, e.session("main-session.jsonl"), "main-session.jsonl")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var stdout bytes.Buffer
	err := runIngest(ctx, ingestOptions{
		dbPath: e.dbPath, stateDir: e.stateDir, roots: e.claudeRoots(),
		interval: 10 * time.Millisecond,
		stdout:   firstWriteCanceler{w: &stdout, cancel: cancel},
		stderr:   io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "imported 16 events from 1 source") {
		t.Fatalf("stdout misses the pass summary: %q", stdout.String())
	}
}

type firstWriteCanceler struct {
	w      io.Writer
	cancel context.CancelFunc
}

func (f firstWriteCanceler) Write(p []byte) (int, error) {
	f.cancel()
	return f.w.Write(p)
}

func TestIngestMissingClaudeRootFails(t *testing.T) {
	e := newSliceEnv(t)
	err := runIngest(context.Background(), ingestOptions{
		dbPath: e.dbPath, stateDir: e.stateDir,
		roots: []agentRoot{{agent: claudecode.AgentName, dir: filepath.Join(e.root, "absent"), flag: "claude-root"}},
		once:  true, stdout: io.Discard, stderr: io.Discard,
	})
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("error = %v, want the missing projects directory", err)
	}
	if _, err := os.Stat(e.dbPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("database %s exists after the failed run: %v", e.dbPath, err)
	}
	if _, err := os.Stat(filepath.Join(e.stateDir, device.FileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("device identity exists after the failed run: %v", err)
	}
}

func TestDiscoverSessionFiles(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "p", "session.jsonl")
	sub := filepath.Join(root, "p", "session", "subagents", "agent-1.jsonl")
	for _, extra := range []string{
		filepath.Join(root, "p", "session", "tool-results", "out.txt"),
		filepath.Join(root, "p", "memory", "MEMORY.md"),
	} {
		writeFile(t, extra, "x")
	}
	writeFile(t, main, "{}\n")
	writeFile(t, sub, "{}\n")

	files, err := discoverSessionFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{sub, main}
	if len(files) != len(want) {
		t.Fatalf("files = %v, want %v", files, want)
	}
	for i := range want {
		if files[i] != want[i] {
			t.Fatalf("files = %v, want %v", files, want)
		}
	}
}

func TestIngestStoppedSourceNotReReadWhileUnchanged(t *testing.T) {
	e := newSliceEnv(t)
	session := e.session("malformed-line.jsonl")
	copyFixture(t, session, "malformed-line.jsonl")
	var stderr bytes.Buffer
	r, done := e.newRunner(t, &stderr)
	defer done()

	stats, err := r.poll(context.Background(), e.root, false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.sources != 1 || stats.stopped != 1 || stats.failed != 0 {
		t.Fatalf("first pass = %+v, want 1 stopped source", stats)
	}

	stats, err = r.poll(context.Background(), e.root, false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.stopped != 1 {
		t.Fatalf("cache-priming pass = %+v, want the stop still reported", stats)
	}

	if err := os.Chmod(session, 0); err != nil {
		t.Fatal(err)
	}
	if f, err := os.Open(session); err == nil {
		f.Close()
		t.Skipf("chmod 0 left %s readable", session)
	}
	defer os.Chmod(session, 0o600)

	stderr.Reset()
	stats, err = r.poll(context.Background(), e.root, false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.sources != 1 || stats.stopped != 1 || stats.failed != 0 {
		t.Fatalf("third pass = %+v, want the cached stop replayed with no failure", stats)
	}
	if len(stats.stops) != 1 || stats.stops[0].Offset != 1094 {
		t.Fatalf("stops = %+v, want the cached offset 1094", stats.stops)
	}
	if stderr.String() != "" {
		t.Fatalf("third pass re-read the unreadable, unchanged source: %s", stderr.String())
	}
}

func TestIngestPartialTailNotReReadUntilGrowth(t *testing.T) {
	e := newSliceEnv(t)
	session := e.session("incomplete-tail.jsonl")
	copyFixture(t, session, "incomplete-tail.jsonl")
	var stderr bytes.Buffer
	r, done := e.newRunner(t, &stderr)
	defer done()

	stats, err := r.poll(context.Background(), e.root, false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.inserted != 3 || stats.stopped != 0 || stats.failed != 0 {
		t.Fatalf("first pass = %+v, want 3 complete records", stats)
	}

	stats, err = r.poll(context.Background(), e.root, false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.inserted != 0 || stats.failed != 0 {
		t.Fatalf("cache-priming pass = %+v, want no new events and no failure", stats)
	}

	if err := os.Chmod(session, 0); err != nil {
		t.Fatal(err)
	}
	if f, err := os.Open(session); err == nil {
		f.Close()
		t.Skipf("chmod 0 left %s readable", session)
	}

	stderr.Reset()
	stats, err = r.poll(context.Background(), e.root, false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.inserted != 0 || stats.failed != 0 {
		t.Fatalf("third pass = %+v, want no attempt on the unchanged partial tail", stats)
	}
	if stderr.String() != "" {
		t.Fatalf("third pass re-read the unreadable, unchanged source: %s", stderr.String())
	}

	if err := os.Chmod(session, 0o600); err != nil {
		t.Fatal(err)
	}
	appendFile(t, session, `"}]}}`+"\n")
	stats, err = r.poll(context.Background(), e.root, false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.inserted != 1 || stats.failed != 0 {
		t.Fatalf("growth pass = %+v, want the completed record ingested", stats)
	}
}

func TestIngestReplacementRetriesDespiteCachedStop(t *testing.T) {
	e := newSliceEnv(t)
	session := e.session("malformed-line.jsonl")
	copyFixture(t, session, "malformed-line.jsonl")
	var stderr bytes.Buffer
	r, done := e.newRunner(t, &stderr)
	defer done()

	stats, err := r.poll(context.Background(), e.root, false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.stopped != 1 {
		t.Fatalf("first pass = %+v, want 1 stopped source", stats)
	}

	stats, err = r.poll(context.Background(), e.root, false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.stopped != 1 {
		t.Fatalf("cache-primed pass = %+v, want the stop replayed from cache", stats)
	}

	line := func(content, sid string) string {
		return fmt.Sprintf(`{"type":"user","sessionId":%q,"message":{"role":"user","content":%q}}`+"\n", sid, content)
	}
	replaceFile(t, session, line("replacement", "sess-new"))

	stats, err = r.poll(context.Background(), e.root, false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.inserted != 1 || stats.stopped != 0 {
		t.Fatalf("pass after replacement = %+v, want the new incarnation ingested, not skipped from the old source's cache", stats)
	}
}
