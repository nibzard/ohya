package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nibzard/ohya/internal/pi"
	"github.com/nibzard/ohya/internal/store"
)

// Session ids of the Pi fixtures (testdata/pi/README.md).
const (
	piMainSession = "00000000-0000-7000-8000-0000000000b1"
	piTailSession = "00000000-0000-7000-8000-0000000000b3"
)

// newPiEnv is one local hub facing a synthetic Pi sessions root.
func newPiEnv(t *testing.T) *sliceEnv {
	t.Helper()
	e := newSliceEnv(t)
	e.agent = pi.AgentName
	return e
}

// piSession places a session file at the Pi layout: an encoded-cwd
// directory below the sessions root.
func (e *sliceEnv) piSession(name string) string {
	return filepath.Join(e.root, "--home-dev-pi-project--", name)
}

func copyPiFixture(t *testing.T, dst, name string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "pi", name))
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, dst, string(data))
}

// piLine builds one complete Pi message entry.
func piLine(id, parent, role, text string) string {
	return fmt.Sprintf(`{"type":"message","id":%q,"parentId":%q,"timestamp":"2026-11-13T10:31:00.000Z","message":{"role":%q,"content":[{"type":"text","text":%q}]}}`+"\n",
		id, parent, role, text)
}

func piHeader(session string) string {
	return fmt.Sprintf(`{"type":"session","version":3,"id":%q,"timestamp":"2026-11-13T10:30:00.000Z","cwd":"/home/dev/pi-project"}`+"\n", session)
}

// TestIngestPiSessions drives the whole slice over a successful and a failed
// Pi session: two sessions with their aggregates, per-response token totals
// that sum directly, and incremental appends landing in the same session.
func TestIngestPiSessions(t *testing.T) {
	e := newPiEnv(t)
	mainPath := e.piSession("2026-11-13T09-10-00-000Z_" + piMainSession + ".jsonl")
	copyPiFixture(t, mainPath, "main-session.jsonl")
	copyPiFixture(t, e.piSession("2026-11-13T09-20-00-000Z_00000000-0000-7000-8000-0000000000b2.jsonl"), "error-session.jsonl")

	stats, stderr := e.runPass(t)
	if stats.sources != 2 || stats.inserted != 13 || stderr != "" {
		t.Fatalf("first pass = %+v, stderr %q; want 13 events from 2 sources", stats, stderr)
	}
	hub := openHub(t, e.dbPath)
	db := hub.Writer()
	dev := e.deviceID(t)
	mainSID := store.SessionID(dev, pi.AgentName, piMainSession)
	if n := countRows(t, db, `SELECT count(*) FROM sessions`); n != 2 {
		t.Fatalf("sessions = %d, want the successful and the failed session", n)
	}
	if n := countRows(t, db, `SELECT event_count FROM sessions WHERE id = ?`, mainSID); n != 8 {
		t.Fatalf("main event_count = %d, want 8", n)
	}
	var cwd, model, prompt string
	if err := db.QueryRow(`SELECT coalesce(cwd,''), coalesce(model,''), coalesce(first_prompt,'') FROM sessions WHERE id = ?`, mainSID).
		Scan(&cwd, &model, &prompt); err != nil {
		t.Fatal(err)
	}
	if cwd != "/home/dev/pi-project" || model != "gpt-5.4-nano" ||
		prompt != "Use your read or list tool once to see the file names in the current working directory, then reply with one short sentence that names them." {
		t.Fatalf("main aggregates = %q/%q/%q", cwd, model, prompt)
	}
	// Two responses with usage; the failed session contributes none.
	var input, cached, output int64
	if err := db.QueryRow(`SELECT sum(input_tokens), sum(cache_read_input_tokens), sum(output_tokens)
	    FROM session_token_usage`).Scan(&input, &cached, &output); err != nil {
		t.Fatal(err)
	}
	if input != 1840+345 || cached != 1536 || output != 24+41 {
		t.Fatalf("tokens = %d/%d/%d", input, cached, output)
	}

	// The session continues: an appended entry has no session field of its
	// own and still joins the header's session.
	appendFile(t, mainPath, piLine("ee000008", "aa000007", "user", "Thanks."))
	stats, _ = e.runPass(t)
	if stats.inserted != 1 {
		t.Fatalf("pass after append = %+v, want 1 event", stats)
	}
	if n := countRows(t, db, `SELECT count(*) FROM events WHERE session_id = ? AND kind = 'message' AND role = 'user'`, mainSID); n != 2 {
		t.Fatal("the appended entry did not join the session")
	}

	// An idle pass inserts nothing.
	if stats, _ := e.runPass(t); stats.inserted != 0 {
		t.Fatalf("idle pass = %+v", stats)
	}
}

// TestIngestPiIncompleteTail covers a crash during a session write: the
// partial entry stays pending, and once the agent completes the line, the
// next pass ingests it whole into the same session.
func TestIngestPiIncompleteTail(t *testing.T) {
	e := newPiEnv(t)
	path := e.piSession("2026-11-13T09-30-00-000Z_" + piTailSession + ".jsonl")
	copyPiFixture(t, path, "incomplete-tail.jsonl")

	stats, stderr := e.runPass(t)
	if stats.inserted != 3 || stderr != "" {
		t.Fatalf("pass = %+v, stderr %q; want 3 complete records", stats, stderr)
	}
	if _, cursor := soleAgentCheckpoint(t, e.stateDir, pi.AgentName); cursor != 502 {
		t.Fatalf("checkpoint cursor = %d, want 502", cursor)
	}

	appendFile(t, path, "ains `main.py` and `notes.txt`.\"}]}}\n")
	stats, _ = e.runPass(t)
	if stats.inserted != 1 {
		t.Fatalf("pass after completion = %+v, want 1 event", stats)
	}
	hub := openHub(t, e.dbPath)
	sid := store.SessionID(e.deviceID(t), pi.AgentName, piTailSession)
	if n := countRows(t, hub.Writer(), `SELECT count(*) FROM events WHERE session_id = ?`, sid); n != 4 {
		t.Fatalf("session events = %d, want 4", n)
	}
}

// TestIngestPiMalformedLineStops covers a complete but invalid entry: the
// valid prefix commits, and the source stops at the offending offset with a
// content-free diagnostic reported once per run.
func TestIngestPiMalformedLineStops(t *testing.T) {
	e := newPiEnv(t)
	copyPiFixture(t, e.piSession("bad.jsonl"), "malformed-line.jsonl")
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
	if n := strings.Count(stderr.String(), "offset 283"); n != 1 {
		t.Fatalf("diagnostic reported %d times, want 1:\n%s", n, stderr.String())
	}
	if strings.Contains(stderr.String(), "break") {
		t.Fatalf("diagnostic leaks transcript content:\n%s", stderr.String())
	}
	if _, cursor := soleAgentCheckpoint(t, e.stateDir, pi.AgentName); cursor != 283 {
		t.Fatalf("checkpoint cursor = %d, want 283", cursor)
	}
}

// TestIngestPiReplacementAndTruncation covers changed incarnations of a
// session file: each starts a new source that re-imports from byte zero and
// names its session from its own header.
func TestIngestPiReplacementAndTruncation(t *testing.T) {
	e := newPiEnv(t)
	path := e.piSession("swap.jsonl")
	writeFile(t, path, piHeader("sess-a")+piLine("ff000002", "ff000001", "user", "first"))
	if stats, _ := e.runPass(t); stats.inserted != 2 {
		t.Fatalf("first pass = %+v", stats)
	}

	replaceFile(t, path, piHeader("sess-b")+piLine("ff000012", "ff000011", "user", "replaced")+piLine("ff000013", "ff000012", "assistant", "more"))
	stats, stderr := e.runPass(t)
	if stats.inserted != 3 || !strings.Contains(stderr, "identity changed") {
		t.Fatalf("pass after replacement = %+v, stderr %q", stats, stderr)
	}

	if err := os.Truncate(path, int64(len(piHeader("sess-b")))); err != nil {
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
	// The truncated incarnation re-imports sess-b's header: a new source,
	// the same session. Copied history is retry-safe, not semantically
	// deduplicated.
	if n := countRows(t, db, `SELECT count(*) FROM events e JOIN sessions s ON s.id = e.session_id
	    WHERE s.native_id = 'sess-b'`); n != 4 {
		t.Fatalf("sess-b events = %d, want 4", n)
	}
	// An entry appended to the truncated incarnation joins sess-b.
	appendFile(t, path, piLine("ff000014", "ff000011", "user", "after truncation"))
	if stats, _ := e.runPass(t); stats.inserted != 1 {
		t.Fatalf("pass after append = %+v", stats)
	}
}

// TestIngestPiLargeFileSpansBatches forces a session above the batch bound,
// so a later batch starts past offset 0 and takes its session from the
// committed header record.
func TestIngestPiLargeFileSpansBatches(t *testing.T) {
	e := newPiEnv(t)
	var b strings.Builder
	b.WriteString(piHeader("sess-big"))
	pad := strings.Repeat("x", 64<<10)
	for i := 0; int64(b.Len()) <= maxBatchBytes+(1<<20); i++ {
		fmt.Fprintf(&b, `{"type":"message","id":"ff%06d","parentId":null,"timestamp":"2026-11-13T11:00:00.000Z","message":{"role":"user","content":[{"type":"text","text":%q}]}}`+"\n", i, pad)
	}
	writeFile(t, e.piSession("big.jsonl"), b.String())
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

// TestIngestPiCrashBetweenCommitAndAcknowledge kills a helper process after
// the write path committed a session file but before the checkpoint
// acknowledged it. The restart replays the bytes and inserts nothing.
func TestIngestPiCrashBetweenCommitAndAcknowledge(t *testing.T) {
	e := newPiEnv(t)
	path := e.piSession("2026-11-13T09-10-00-000Z_" + piMainSession + ".jsonl")
	copyPiFixture(t, path, "main-session.jsonl")

	cmd := exec.Command(os.Args[0], "-test.run=TestIngestCrashHelper")
	cmd.Env = append(os.Environ(),
		"OHYA_SLICE_CRASH_DB="+e.dbPath,
		"OHYA_SLICE_CRASH_STATE="+e.stateDir,
		"OHYA_SLICE_CRASH_ROOT="+e.root,
		"OHYA_SLICE_CRASH_AGENT="+pi.AgentName,
	)
	if err := cmd.Run(); !errors.As(err, new(*exec.ExitError)) {
		t.Fatalf("helper did not crash as intended: %v", err)
	}
	hub := openHub(t, e.dbPath)
	if n := countRows(t, hub.Writer(), `SELECT count(*) FROM events`); n != 8 {
		t.Fatalf("events after crash = %d, want 8", n)
	}
	if _, cursor := soleAgentCheckpoint(t, e.stateDir, pi.AgentName); cursor != 0 {
		t.Fatalf("checkpoint cursor after crash = %d, want 0", cursor)
	}

	stats, stderr := e.runPass(t)
	if stats.inserted != 0 || stderr != "" {
		t.Fatalf("restart = %+v, stderr %q; want nothing new", stats, stderr)
	}
	// The session continues after the crash; the appended entry still finds
	// its session.
	line := piLine("ee000008", "aa000007", "user", "after the crash")
	appendFile(t, path, line)
	if stats, _ := e.runPass(t); stats.inserted != 1 {
		t.Fatalf("pass after append = %+v", stats)
	}
	sid := store.SessionID(e.deviceID(t), pi.AgentName, piMainSession)
	if n := countRows(t, hub.Writer(), `SELECT event_count FROM sessions WHERE id = ?`, sid); n != 9 {
		t.Fatalf("event_count = %d, want 9", n)
	}
	if _, cursor := soleAgentCheckpoint(t, e.stateDir, pi.AgentName); cursor != 2953+int64(len(line)) {
		t.Fatalf("checkpoint cursor after append = %d", cursor)
	}
}

func TestDefaultPiRoot(t *testing.T) {
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "")
	t.Setenv("PI_CODING_AGENT_DIR", "")
	if got := defaultPiRoot("/home/u"); got != filepath.Join("/home/u", ".pi", "agent", "sessions") {
		t.Fatalf("default = %q", got)
	}
	t.Setenv("PI_CODING_AGENT_DIR", "/opt/pi-agent")
	if got := defaultPiRoot("/home/u"); got != filepath.Join("/opt/pi-agent", "sessions") {
		t.Fatalf("PI_CODING_AGENT_DIR default = %q", got)
	}
	t.Setenv("PI_CODING_AGENT_SESSION_DIR", "/srv/pi-sessions")
	if got := defaultPiRoot("/home/u"); got != "/srv/pi-sessions" {
		t.Fatalf("PI_CODING_AGENT_SESSION_DIR default = %q", got)
	}
}
