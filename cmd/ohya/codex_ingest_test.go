package main

import (
	"bytes"
	"context"
	"errors"
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

// Thread ids of the Codex fixtures (testdata/codex/README.md).
const (
	codexMainThread = "00000000-0000-7000-8000-0000000000c1"
	codexSubThread  = "00000000-0000-7000-8000-0000000000d1"
	codexTailThread = "00000000-0000-7000-8000-0000000000c4"
)

// newCodexEnv is one local hub facing a synthetic Codex sessions directory.
func newCodexEnv(t *testing.T) *sliceEnv {
	t.Helper()
	e := newSliceEnv(t)
	e.agent = codex.AgentName
	return e
}

// rollout places a rollout file at the Codex layout: dated directories under
// the sessions root.
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

// codexLine builds one complete Codex message record with an ordinal.
func codexLine(ordinal int, role, text string) string {
	return fmt.Sprintf(`{"timestamp":"2026-11-13T10:31:00.000Z","ordinal":%d,"type":"response_item","payload":{"type":"message","role":%q,"content":[{"type":"input_text","text":%q}]}}`+"\n",
		ordinal, role, text)
}

func codexMeta(thread string) string {
	return fmt.Sprintf(`{"timestamp":"2026-11-13T10:30:00.000Z","ordinal":0,"type":"session_meta","payload":{"id":%q,"cwd":"/home/dev/example"}}`+"\n", thread)
}

// TestIngestCodexThreadAcrossRolloutFiles drives the whole slice on a Codex
// thread that spans two rollout files plus a subagent rollout: the page joins
// the main thread's session, its replayed ordinals are stored as duplicates,
// and later appends to the page land in the same session.
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

	// The thread continues in the page: the appended record has no session
	// field of its own and still joins the main thread.
	appendFile(t, pagePath, codexLine(16, "user", "Appended after the first pass."))
	stats, _ = e.runPass(t)
	if stats.inserted != 1 {
		t.Fatalf("pass after append = %+v, want 1 event", stats)
	}
	if n := countRows(t, db, `SELECT count(*) FROM events WHERE session_id = ? AND ordinal = 16`, mainSID); n != 1 {
		t.Fatal("the appended record did not join the main thread")
	}

	// An idle pass inserts nothing.
	if stats, _ := e.runPass(t); stats.inserted != 0 {
		t.Fatalf("idle pass = %+v", stats)
	}
}

// TestIngestCodexReenumeratedPage drives the slice over a main file and a
// page that re-enumerates the ordinals past its history_base rollback point
// with different records: nothing is suppressed, and both the rolled-back
// turn's and the re-enumerated turn's records stay searchable with their own
// per-response usage rows.
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
	// The re-enumerated turn's response counts beside the original's two
	// (raw input totals: 1200 + 1300 + 1400).
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

	// An idle pass inserts nothing.
	if stats, _ := e.runPass(t); stats.inserted != 0 {
		t.Fatalf("idle pass = %+v", stats)
	}
}

// TestIngestCodexIncompleteTail covers a crash during a rollout write: the
// partial record stays pending, and once the writer completes the line, the
// next pass ingests it whole into the same thread.
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

// TestIngestCodexMalformedLineStops covers a complete but invalid record: the
// valid prefix commits, and the source stops at the offending offset with a
// content-free diagnostic reported once per run.
func TestIngestCodexMalformedLineStops(t *testing.T) {
	e := newCodexEnv(t)
	copyCodexFixture(t, e.rollout("rollout-bad.jsonl"), "malformed-line.jsonl")
	var stderr bytes.Buffer
	r, done := e.newRunner(t, &stderr)
	defer done()
	for i := 0; i < 2; i++ {
		stats, err := r.poll(context.Background(), e.root, false)
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

// TestIngestCodexReplacementAndTruncation covers changed incarnations of a
// rollout file: each starts a new source that re-imports from byte zero and
// names its thread from its own first record.
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
	// The truncated incarnation re-imports thread-b's meta: a new source,
	// the same thread. Its meta carries no ordinal, so it is no duplicate;
	// copied history is retry-safe, not semantically deduplicated.
	if n := countRows(t, db, `SELECT count(*) FROM events e JOIN sessions s ON s.id = e.session_id
	    WHERE s.native_id = 'thread-b'`); n != 4 {
		t.Fatalf("thread-b events = %d, want 4", n)
	}
	// A record appended to the truncated incarnation joins thread-b, and
	// its ordinal repeats one already stored, so it is a duplicate.
	appendFile(t, path, codexLine(1, "user", "replaced"))
	if stats, _ := e.runPass(t); stats.inserted != 1 {
		t.Fatalf("pass after append = %+v", stats)
	}
	if n := countRows(t, db, `SELECT count(*) FROM events e JOIN sessions s ON s.id = e.session_id
	    WHERE s.native_id = 'thread-b' AND e.duplicate_of IS NOT NULL`); n != 1 {
		t.Fatalf("thread-b duplicates = %d, want 1", n)
	}
}

// TestIngestCodexLargeFileSpansBatches forces a rollout above the batch
// bound, so a later batch starts past offset 0 and takes its thread from the
// committed first record.
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

// TestIngestCodexCrashBetweenCommitAndAcknowledge kills a helper process
// after the write path committed a rollout but before the checkpoint
// acknowledged it. The restart replays the bytes and inserts nothing.
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
	// The thread continues after the crash; the appended record still
	// finds its session.
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

// TestIngestWatchesBothAgents runs the command loop over a Claude Code root
// and a Codex root in one pass, each with its own checkpoint file.
func TestIngestWatchesBothAgents(t *testing.T) {
	e := newSliceEnv(t)
	copyFixture(t, e.session("main-session.jsonl"), "main-session.jsonl")
	codexRoot := filepath.Join(filepath.Dir(e.root), "codex-sessions")
	copyCodexFixture(t, filepath.Join(codexRoot, "2026", "11", "13", "rollout-a.jsonl"), "main-session.jsonl")
	var stdout bytes.Buffer
	err := runIngest(context.Background(), ingestOptions{
		dbPath: e.dbPath, stateDir: e.stateDir, once: true,
		roots:  append(e.claudeRoots(), agentRoot{agent: codex.AgentName, dir: codexRoot, flag: "codex-root"}),
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
	// A missing optional root holds no sessions; the run still works.
	e := newSliceEnv(t)
	absent := filepath.Join(e.root, "absent")
	if err := run(e, agentRoot{agent: claudecode.AgentName, dir: e.root, flag: "claude-root"},
		agentRoot{agent: codex.AgentName, dir: absent, flag: "codex-root", optional: true}); err != nil {
		t.Fatalf("missing optional root: %v", err)
	}
	// A missing explicit root fails before creating anything.
	e = newSliceEnv(t)
	err := run(e, agentRoot{agent: codex.AgentName, dir: absent, flag: "codex-root"})
	if err == nil || !strings.Contains(err.Error(), "pass -codex-root") {
		t.Fatalf("missing explicit root error = %v", err)
	}
	if _, err := os.Stat(e.dbPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("database exists after the failed run: %v", err)
	}
	// Nothing to watch at all is an error, not an idle loop.
	err = run(e, agentRoot{agent: claudecode.AgentName, dir: absent, flag: "claude-root", optional: true},
		agentRoot{agent: codex.AgentName, dir: absent, flag: "codex-root", optional: true})
	if err == nil || !strings.Contains(err.Error(), "-claude-root or -codex-root") {
		t.Fatalf("all roots missing error = %v", err)
	}
	if err := run(e); err == nil {
		t.Fatal("a run without roots succeeded")
	}
	if err := run(e, agentRoot{agent: "opencode", dir: e.root, flag: "x"}); err == nil {
		t.Fatal("an agent without a parser was accepted")
	}
}

func TestDefaultCodexRoot(t *testing.T) {
	t.Setenv("CODEX_HOME", "")
	if got := defaultCodexRoot("/home/u"); got != filepath.Join("/home/u", ".codex", "sessions") {
		t.Fatalf("default = %q", got)
	}
	t.Setenv("CODEX_HOME", "/opt/codex")
	if got := defaultCodexRoot("/home/u"); got != filepath.Join("/opt/codex", "sessions") {
		t.Fatalf("CODEX_HOME default = %q", got)
	}
}
