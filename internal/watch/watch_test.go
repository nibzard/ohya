package watch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const agent = "claude-code"

func mustNew(t *testing.T, dir string) *Watcher {
	t.Helper()
	w, err := New(dir, agent, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return w
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
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
		t.Fatalf("close %s: %v", path, err)
	}
}

// replaceFile puts new content at path through a distinct inode, so the path
// ends up holding a genuinely new file incarnation.
func replaceFile(t *testing.T, path, content string) {
	t.Helper()
	tmp := path + ".replacement"
	writeFile(t, tmp, content)
	if err := os.Rename(tmp, path); err != nil {
		t.Fatalf("rename %s: %v", tmp, err)
	}
}

func poll(t *testing.T, w *Watcher, candidates ...string) []Outcome {
	t.Helper()
	outcomes, err := w.Poll(candidates)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	return outcomes
}

func mustReadPending(t *testing.T, w *Watcher, o Outcome) string {
	t.Helper()
	data, err := w.ReadPending(o)
	if err != nil {
		t.Fatalf("ReadPending: %v", err)
	}
	return string(data)
}

// ingestAll is the caller-side loop T007 will run: read the pending bytes,
// hand them to ingestion, then acknowledge the durable offset.
func ingestAll(t *testing.T, w *Watcher, outcomes []Outcome) {
	t.Helper()
	for _, o := range outcomes {
		if o.Missing || o.End <= o.Cursor {
			continue
		}
		mustReadPending(t, w, o)
		if err := w.Acknowledge(o.Path, o.SourceID, o.End); err != nil {
			t.Fatalf("Acknowledge(%s): %v", o.Path, err)
		}
	}
}

func sole(t *testing.T, outcomes []Outcome) Outcome {
	t.Helper()
	if len(outcomes) != 1 {
		t.Fatalf("outcomes = %v, want exactly one", outcomes)
	}
	return outcomes[0]
}

func stateFilePath(dir string) string {
	return filepath.Join(dir, "sources", agent+".json")
}

func TestNewRejectsInvalidAgentName(t *testing.T) {
	for _, name := range []string{"", "Claude-Code", "../escape", "a b", strings.Repeat("x", 65)} {
		if _, err := New(t.TempDir(), name, Options{}); err == nil {
			t.Fatalf("New(%q) succeeded, want an error", name)
		}
	}
}

func TestNewStartsEmptyWithoutStateFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := New(dir, agent, Options{}); err != nil {
		t.Fatalf("New without state: %v", err)
	}
	if _, err := os.Stat(stateFilePath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state file exists before the first poll: %v", err)
	}
}

func TestNewReportsLostState(t *testing.T) {
	cases := map[string]string{
		"corrupt JSON":       "{not json",
		"wrong version":      `{"version":99,"sources":{}}`,
		"inconsistent entry": `{"version":1,"sources":{"/a":{"source_id":"","agent":"claude-code","path":"/a"}}}`,
		"stale fingerprint":  `{"version":1,"sources":{"/a":{"source_id":"aa","agent":"claude-code","path":"/a","cursor_bytes":10,"fingerprint_window":0,"fingerprint_sha256":""}}}`,
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Dir(stateFilePath(dir)), 0o700); err != nil {
				t.Fatal(err)
			}
			writeFile(t, stateFilePath(dir), content)
			w, err := New(dir, agent, Options{})
			if !errors.Is(err, ErrStateLost) {
				t.Fatalf("error = %v, want ErrStateLost", err)
			}
			if w != nil {
				t.Fatal("New returned a watcher with lost state")
			}
			if !strings.Contains(err.Error(), "recovery required") {
				t.Fatalf("error %q does not explain the recovery need", err)
			}
		})
	}
}

func TestPollRegistersNewSourceBeforeIngestion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	writeFile(t, path, "line1\n")

	w := mustNew(t, dir)
	outcome := sole(t, poll(t, w, path))
	if !outcome.Fresh || outcome.Reason != "new file" {
		t.Fatalf("outcome = %+v, want a fresh new-file outcome", outcome)
	}
	if outcome.Cursor != 0 || outcome.End != int64(len("line1\n")) {
		t.Fatalf("outcome spans [%d, %d), want [0, %d)", outcome.Cursor, outcome.End, len("line1\n"))
	}
	if len(outcome.SourceID) != 32 {
		t.Fatalf("source ID = %q, want 32 hex characters", outcome.SourceID)
	}

	// The state file must already hold the new source ID, before any
	// ingestion: a crash here cannot lose the identity.
	state, err := os.ReadFile(stateFilePath(dir))
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if !strings.Contains(string(state), outcome.SourceID) {
		t.Fatalf("state does not persist the new source ID:\n%s", state)
	}
	if !strings.Contains(string(state), `"file_identity": "dev:`) {
		t.Fatalf("state does not persist the platform file identity:\n%s", state)
	}

	// Reopening without any ingestion keeps the same source.
	reopened := mustNew(t, dir)
	next := sole(t, poll(t, reopened, path))
	if next.SourceID != outcome.SourceID || next.Fresh {
		t.Fatalf("outcome = %+v, want the persisted source %s", next, outcome.SourceID)
	}
}

func TestPollThenAppendThenAcknowledge(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	writeFile(t, path, "first\n")

	w := mustNew(t, dir)
	first := sole(t, poll(t, w, path))
	ingestAll(t, w, []Outcome{first})

	// Acknowledgment advanced the durable checkpoint.
	state, err := os.ReadFile(stateFilePath(dir))
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if !strings.Contains(string(state), `"cursor_bytes": 6`) {
		t.Fatalf("state does not record the acknowledged cursor:\n%s", state)
	}

	appendFile(t, path, "second\n")
	outcome := sole(t, poll(t, w, path))
	if outcome.Fresh || outcome.SourceID != first.SourceID {
		t.Fatalf("outcome = %+v, want the same source %s", outcome, first.SourceID)
	}
	if outcome.Cursor != 6 || outcome.End != 13 {
		t.Fatalf("outcome spans [%d, %d), want [6, 13)", outcome.Cursor, outcome.End)
	}
	if got := mustReadPending(t, w, outcome); got != "second\n" {
		t.Fatalf("pending = %q, want %q", got, "second\n")
	}
	if err := w.Acknowledge(path, first.SourceID, 13); err != nil {
		t.Fatalf("Acknowledge: %v", err)
	}

	idle := sole(t, poll(t, w, path))
	if idle.Cursor != 13 || idle.End != 13 {
		t.Fatalf("idle outcome = %+v, want cursor and end at 13", idle)
	}

	// The checkpoint survives a restart.
	reopened := mustNew(t, dir)
	resumed := sole(t, poll(t, reopened, path))
	if resumed.Cursor != 13 || resumed.SourceID != first.SourceID {
		t.Fatalf("resumed outcome = %+v, want cursor 13 for %s", resumed, first.SourceID)
	}
}

func TestPollIgnoresDuplicateAndUnusableCandidates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	writeFile(t, path, "x\n")

	w := mustNew(t, dir)
	outcomes := poll(t, w, path, path, "", filepath.Join(dir, "absent.jsonl"), dir)
	outcome := sole(t, outcomes)
	if !outcome.Fresh {
		t.Fatalf("outcome = %+v, want one fresh registration", outcome)
	}
}

func TestPollReportsMissingAndResumesWhenObservable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	writeFile(t, path, "content\n")

	w := mustNew(t, dir)
	first := sole(t, poll(t, w, path))
	ingestAll(t, w, []Outcome{first})

	// An empty candidate list: the discovery pass saw nothing, so the
	// source is missing but its checkpoint is retained.
	missing := sole(t, poll(t, w))
	if !missing.Missing || missing.SourceID != first.SourceID || missing.Cursor != 8 {
		t.Fatalf("missing outcome = %+v, want the retained source at cursor 8", missing)
	}
	if missing.Fresh {
		t.Fatal("a missing source must not be re-registered")
	}

	back := sole(t, poll(t, w, path))
	if back.Missing || back.Fresh || back.SourceID != first.SourceID || back.Cursor != 8 {
		t.Fatalf("resumed outcome = %+v, want the same source at cursor 8", back)
	}
}

func TestReplacementCreatesNewSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	writeFile(t, path, "old content\n")

	w := mustNew(t, dir)
	first := sole(t, poll(t, w, path))
	ingestAll(t, w, []Outcome{first})

	replaceFile(t, path, "brand new file\n")
	outcome := sole(t, poll(t, w, path))
	if !outcome.Fresh {
		t.Fatalf("outcome = %+v, want a fresh source", outcome)
	}
	if outcome.SourceID == first.SourceID {
		t.Fatalf("replacement kept source ID %s", first.SourceID)
	}
	if !strings.Contains(outcome.Reason, "identity changed") {
		t.Fatalf("reason = %q, want the replacement explanation", outcome.Reason)
	}
	if outcome.Cursor != 0 || outcome.End != int64(len("brand new file\n")) {
		t.Fatalf("outcome spans [%d, %d), want a full re-import", outcome.Cursor, outcome.End)
	}
	if got := mustReadPending(t, w, outcome); got != "brand new file\n" {
		t.Fatalf("pending = %q, want the whole new file", got)
	}

	// The old incarnation is gone from the local state.
	state, err := os.ReadFile(stateFilePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(state), first.SourceID) {
		t.Fatalf("state still holds the replaced source %s:\n%s", first.SourceID, state)
	}
}

func TestTruncationCreatesNewSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	writeFile(t, path, "0123456789\n")

	w := mustNew(t, dir)
	first := sole(t, poll(t, w, path))
	ingestAll(t, w, []Outcome{first})

	if err := os.Truncate(path, 4); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	outcome := sole(t, poll(t, w, path))
	if !outcome.Fresh || outcome.SourceID == first.SourceID {
		t.Fatalf("outcome = %+v, want a fresh source after truncation", outcome)
	}
	if !strings.Contains(outcome.Reason, "truncated") {
		t.Fatalf("reason = %q, want the truncation explanation", outcome.Reason)
	}
	if got := mustReadPending(t, w, outcome); got != "0123" {
		t.Fatalf("pending = %q, want the truncated prefix", got)
	}
}

func TestTruncateAndRegrowDetectedByFingerprint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	writeFile(t, path, "0123456789\n")

	w := mustNew(t, dir)
	first := sole(t, poll(t, w, path))
	ingestAll(t, w, []Outcome{first})

	// The file shrinks below the cursor and grows past it again between
	// polls, keeping its inode: the identity and size checks cannot see it,
	// the fingerprint can.
	if err := os.Truncate(path, 4); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	appendFile(t, path, "XXXXXXX\n") // size 12, past the old cursor 11
	outcome := sole(t, poll(t, w, path))
	if !outcome.Fresh || outcome.SourceID == first.SourceID {
		t.Fatalf("outcome = %+v, want a fresh source after the rewrite", outcome)
	}
	if !strings.Contains(outcome.Reason, "rewritten in place") {
		t.Fatalf("reason = %q, want the rewrite explanation", outcome.Reason)
	}
}

func TestInPlaceRewriteDetectedByFingerprint(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	content := "0123456789\n"
	writeFile(t, path, content)

	w := mustNew(t, dir)
	first := sole(t, poll(t, w, path))
	ingestAll(t, w, []Outcome{first})

	// Same inode, same size, one changed byte inside the acknowledged
	// prefix: only the checkpoint fingerprint can detect it.
	f, err := os.OpenFile(path, os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := f.WriteAt([]byte("X"), 0); err != nil {
		t.Fatalf("write in place: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	outcome := sole(t, poll(t, w, path))
	if !outcome.Fresh || outcome.SourceID == first.SourceID {
		t.Fatalf("outcome = %+v, want a fresh source after the in-place rewrite", outcome)
	}
	if !strings.Contains(outcome.Reason, "rewritten in place") {
		t.Fatalf("reason = %q, want the rewrite explanation", outcome.Reason)
	}
}

func TestRenameKeepsSourceIdentity(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "session.jsonl")
	writeFile(t, oldPath, "record one\n")

	w := mustNew(t, dir)
	first := sole(t, poll(t, w, oldPath))
	ingestAll(t, w, []Outcome{first})

	newPath := filepath.Join(dir, "session-moved.jsonl")
	if err := os.Rename(oldPath, newPath); err != nil {
		t.Fatalf("rename: %v", err)
	}
	outcome := sole(t, poll(t, w, newPath))
	if outcome.Fresh || outcome.Missing {
		t.Fatalf("outcome = %+v, want a continued source after the rename", outcome)
	}
	if outcome.SourceID != first.SourceID {
		t.Fatalf("rename created source %s, want to keep %s", outcome.SourceID, first.SourceID)
	}
	if outcome.Path != normalizePath(newPath) || outcome.RenamedFrom != normalizePath(oldPath) {
		t.Fatalf("outcome tracks %q renamed from %q", outcome.Path, outcome.RenamedFrom)
	}
	if outcome.Cursor != 11 {
		t.Fatalf("cursor = %d, want the retained checkpoint 11", outcome.Cursor)
	}

	// Acknowledgment works under the new path and the state forgets the old one.
	appendFile(t, newPath, "record two\n")
	grown := sole(t, poll(t, w, newPath))
	if err := w.Acknowledge(newPath, first.SourceID, grown.End); err != nil {
		t.Fatalf("Acknowledge under the new path: %v", err)
	}
	state, err := os.ReadFile(stateFilePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	canonOld, canonNew := normalizePath(oldPath), normalizePath(newPath)
	if strings.Contains(string(state), canonOld) {
		t.Fatalf("state still watches the old path:\n%s", state)
	}
	if !strings.Contains(string(state), canonNew) {
		t.Fatalf("state does not watch the new path:\n%s", state)
	}
}

func TestRenameWithReplacementAtOldPath(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "session.jsonl")
	writeFile(t, oldPath, "original\n")

	w := mustNew(t, dir)
	first := sole(t, poll(t, w, oldPath))
	ingestAll(t, w, []Outcome{first})

	// The original file moves away and a new incarnation takes its place.
	newPath := filepath.Join(dir, "session-moved.jsonl")
	if err := os.Rename(oldPath, newPath); err != nil {
		t.Fatalf("rename: %v", err)
	}
	replaceFile(t, oldPath, "imposter\n")

	outcomes := poll(t, w, oldPath, newPath)
	if len(outcomes) != 2 {
		t.Fatalf("outcomes = %+v, want two", outcomes)
	}
	moved, atOld := outcomes[0], outcomes[1]
	if moved.Path == normalizePath(oldPath) { // outcomes are sorted by path
		moved, atOld = atOld, moved
	}
	if moved.SourceID != first.SourceID || moved.RenamedFrom != normalizePath(oldPath) {
		t.Fatalf("moved outcome = %+v, want source %s renamed from the old path", moved, first.SourceID)
	}
	if moved.Cursor != 9 || moved.End != 9 {
		t.Fatalf("moved outcome spans [%d, %d), want [9, 9)", moved.Cursor, moved.End)
	}
	if !atOld.Fresh || atOld.SourceID == first.SourceID {
		t.Fatalf("old-path outcome = %+v, want a fresh source", atOld)
	}
}

func TestReadPendingGuardsAgainstChangesSincePoll(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	writeFile(t, path, "first\nsecond\n")

	w := mustNew(t, dir)
	outcome := sole(t, poll(t, w, path))
	if got := mustReadPending(t, w, outcome); got != "first\nsecond\n" {
		t.Fatalf("pending = %q", got)
	}

	// A replacement after the poll must not feed wrong bytes.
	replaceFile(t, path, "other\n")
	if _, err := w.ReadPending(outcome); err == nil {
		t.Fatal("ReadPending after replacement succeeded, want an error")
	}
	if err := w.Acknowledge(path, outcome.SourceID, 6); err == nil {
		t.Fatal("Acknowledge after replacement succeeded, want an error")
	}
}

func TestAcknowledgeRejectsBadArguments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	writeFile(t, path, "0123456789\n")

	w := mustNew(t, dir)
	first := sole(t, poll(t, w, path))
	if err := w.Acknowledge(filepath.Join(dir, "other.jsonl"), first.SourceID, 1); err == nil {
		t.Fatal("Acknowledge for an unwatched path succeeded, want an error")
	}
	if err := w.Acknowledge(path, "notthesource", 1); err == nil {
		t.Fatal("Acknowledge with a wrong source ID succeeded, want an error")
	}
	if err := w.Acknowledge(path, first.SourceID, 3); err != nil {
		t.Fatalf("Acknowledge partial: %v", err)
	}
	if err := w.Acknowledge(path, first.SourceID, 1); err == nil {
		t.Fatal("Acknowledge before the cursor succeeded, want an error")
	}
	if err := w.Acknowledge(path, first.SourceID, 99); err == nil {
		t.Fatal("Acknowledge past the file size succeeded, want an error")
	}
}

func TestAcknowledgeRecordsFingerprintWindow(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	content := "0123456789\n"
	writeFile(t, path, content)

	w := mustNew(t, dir)
	first := sole(t, poll(t, w, path))
	if err := w.Acknowledge(path, first.SourceID, int64(len(content))); err != nil {
		t.Fatalf("Acknowledge: %v", err)
	}
	entry := w.state.Sources[normalizePath(path)]
	if entry.CursorBytes != int64(len(content)) {
		t.Fatalf("cursor = %d, want %d", entry.CursorBytes, len(content))
	}
	// The content is shorter than the default window, so the fingerprint
	// covers the whole ingested prefix.
	if entry.FingerprintWindow != int64(len(content)) {
		t.Fatalf("window = %d, want %d", entry.FingerprintWindow, len(content))
	}
	want := fingerprintOf(t, path, 0, int64(len(content)))
	if entry.FingerprintSHA256 != want {
		t.Fatalf("fingerprint = %s, want %s", entry.FingerprintSHA256, want)
	}
}

func fingerprintOf(t *testing.T, path string, from, to int64) string {
	t.Helper()
	sum, err := fingerprintFile(path, to, to-from)
	if err != nil {
		t.Fatalf("fingerprintFile: %v", err)
	}
	return sum
}

// hardlink creates a second path for the file at path, so two live paths
// share one platform file identity — what a hardlinking backup tool inside
// the watched tree produces.
func hardlink(t *testing.T, path, link string) {
	t.Helper()
	if err := os.Link(path, link); err != nil {
		t.Skipf("hardlinks unavailable: %v", err)
	}
}

// fileIdentityOf returns the platform identity of the file at path.
func fileIdentityOf(t *testing.T, path string) string {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return platformFileIdentity(info)
}

// writeSharedIdentityState writes a checkpoint file that holds one source
// per path, all on the same file identity and acknowledged through cursor —
// the state a build without the duplicate-identity guards leaves behind.
// It returns the source IDs in path order.
func writeSharedIdentityState(t *testing.T, dir string, paths []string, identity string, cursor int64) []string {
	t.Helper()
	sources := map[string]sourceCheckpoint{}
	var ids []string
	for i, p := range paths {
		norm := normalizePath(p)
		sum, err := fingerprintFile(norm, cursor, cursor)
		if err != nil {
			t.Fatalf("fingerprint %s: %v", norm, err)
		}
		id := fmt.Sprintf("%032x", i+1)
		ids = append(ids, id)
		sources[norm] = sourceCheckpoint{
			SourceID:          id,
			Agent:             agent,
			Path:              norm,
			FileIdentity:      identity,
			CursorBytes:       cursor,
			SizeBytes:         cursor,
			FingerprintWindow: cursor,
			FingerprintSHA256: sum,
		}
	}
	data, err := json.Marshal(checkpointFile{Version: stateVersion, Sources: sources})
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(stateFilePath(dir)), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, stateFilePath(dir), string(data))
	return ids
}

// readState returns the checkpoint file's raw text.
func readState(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(stateFilePath(dir))
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	return string(data)
}

func TestHardlinkedPathsWatchedOnce(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.jsonl")
	b := filepath.Join(dir, "b.jsonl")
	writeFile(t, a, "line1\n")
	hardlink(t, a, b)

	w := mustNew(t, dir)
	outcomes := poll(t, w, a, b)
	// Hardlinks put two live paths on one file incarnation. Only the first
	// path in path order becomes a source; the duplicate is the same
	// incarnation watched through another name.
	if len(outcomes) != 1 {
		t.Fatalf("outcomes = %+v, want exactly one for the shared identity", outcomes)
	}
	first := outcomes[0]
	if first.Path != normalizePath(a) || !first.Fresh {
		t.Fatalf("outcome = %+v, want a fresh source at the first path %s", first, normalizePath(a))
	}
	if strings.Contains(readState(t, dir), normalizePath(b)) {
		t.Fatalf("state watches the hardlinked duplicate path:\n%s", readState(t, dir))
	}

	ingestAll(t, w, outcomes)

	// A later poll is stable: one source, no second registration.
	again := sole(t, poll(t, w, a, b))
	if again.Fresh || again.SourceID != first.SourceID {
		t.Fatalf("outcome = %+v, want the continued source %s", again, first.SourceID)
	}

	// When the watched path disappears, the source rebinds to the surviving
	// link and keeps its acknowledged cursor.
	if err := os.Remove(a); err != nil {
		t.Fatalf("remove %s: %v", a, err)
	}
	rebound := sole(t, poll(t, w, b))
	if rebound.Fresh || rebound.Missing || rebound.SourceID != first.SourceID {
		t.Fatalf("outcome = %+v, want the source %s continued at the surviving link", rebound, first.SourceID)
	}
	if rebound.Cursor != 6 {
		t.Fatalf("cursor = %d, want the acknowledged cursor 6", rebound.Cursor)
	}
	if rebound.RenamedFrom != normalizePath(a) {
		t.Fatalf("outcome tracks a rename from %q, want %q", rebound.RenamedFrom, normalizePath(a))
	}
}

func TestSharedIdentityStateKeepsEveryCheckpoint(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.jsonl")
	b := filepath.Join(dir, "b.jsonl")
	writeFile(t, a, "line1\n")
	hardlink(t, a, b)
	ids := writeSharedIdentityState(t, dir, []string{a, b}, fileIdentityOf(t, a), 6)

	w := mustNew(t, dir)
	// Both live links keep their own sources: neither entry is dropped.
	live := poll(t, w, a, b)
	if len(live) != 2 {
		t.Fatalf("outcomes = %+v, want one per live path", live)
	}

	// One link disappears. The survivor keeps its checkpoint, and the
	// rebind that would move the missing source onto the survivor's key is
	// refused instead of overwriting it.
	if err := os.Remove(a); err != nil {
		t.Fatalf("remove %s: %v", a, err)
	}
	after := poll(t, w, b)
	if len(after) != 2 {
		t.Fatalf("outcomes = %+v, want a missing and a live outcome", after)
	}
	missing, survived := after[0], after[1] // sorted by path: a then b
	if missing.Path != normalizePath(a) || !missing.Missing {
		t.Fatalf("outcome = %+v, want the removed path missing", missing)
	}
	if missing.SourceID != ids[0] || missing.Cursor != 6 {
		t.Fatalf("missing outcome = %+v, want source %s with cursor 6 retained", missing, ids[0])
	}
	if survived.SourceID != ids[1] || survived.Fresh || survived.Cursor != 6 {
		t.Fatalf("surviving outcome = %+v, want source %s at cursor 6", survived, ids[1])
	}
	state := readState(t, dir)
	for _, id := range ids {
		if !strings.Contains(state, id) {
			t.Fatalf("state dropped the acknowledged source %s:\n%s", id, state)
		}
	}

	// The removed path returns as a new incarnation: the replacement is
	// reported as a fresh source with a reason, not a silent restart.
	replaceFile(t, a, "another file\n")
	returned := poll(t, w, a, b)
	if len(returned) != 2 {
		t.Fatalf("outcomes = %+v, want two", returned)
	}
	fresh, kept := returned[0], returned[1]
	if !fresh.Fresh || fresh.SourceID == ids[0] {
		t.Fatalf("outcome = %+v, want a fresh source replacing %s", fresh, ids[0])
	}
	if !strings.Contains(fresh.Reason, "identity changed") {
		t.Fatalf("reason = %q, want the replacement explanation", fresh.Reason)
	}
	if kept.SourceID != ids[1] || kept.Fresh {
		t.Fatalf("surviving outcome = %+v, want source %s kept", kept, ids[1])
	}
}

func TestCompetingRebindsAreRefused(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.jsonl")
	b := filepath.Join(dir, "b.jsonl")
	c := filepath.Join(dir, "c.jsonl")
	writeFile(t, a, "line1\n")
	hardlink(t, a, b)
	hardlink(t, a, c)
	ids := writeSharedIdentityState(t, dir, []string{a, b}, fileIdentityOf(t, a), 6)

	// The two watched links vanish while the incarnation stays reachable
	// under the third path: both missing sources would rebind onto c, so
	// the ambiguous rebind is refused and both checkpoints stay retained.
	if err := os.Remove(a); err != nil {
		t.Fatalf("remove %s: %v", a, err)
	}
	if err := os.Remove(b); err != nil {
		t.Fatalf("remove %s: %v", b, err)
	}
	w := mustNew(t, dir)
	outcomes := poll(t, w, c)
	if len(outcomes) != 3 {
		t.Fatalf("outcomes = %+v, want two missing and one fresh", outcomes)
	}
	missingA, missingB, fresh := outcomes[0], outcomes[1], outcomes[2] // sorted by path
	if missingA.Path != normalizePath(a) || !missingA.Missing || missingA.SourceID != ids[0] || missingA.Cursor != 6 {
		t.Fatalf("outcome = %+v, want source %s missing with cursor 6", missingA, ids[0])
	}
	if missingB.Path != normalizePath(b) || !missingB.Missing || missingB.SourceID != ids[1] || missingB.Cursor != 6 {
		t.Fatalf("outcome = %+v, want source %s missing with cursor 6", missingB, ids[1])
	}
	if !fresh.Fresh || fresh.Cursor != 0 {
		t.Fatalf("outcome = %+v, want a fresh source for the new path", fresh)
	}
	state := readState(t, dir)
	for _, id := range ids {
		if !strings.Contains(state, id) {
			t.Fatalf("state dropped the acknowledged source %s:\n%s", id, state)
		}
	}
}

func TestSwappedPathsKeepBothSources(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.jsonl")
	b := filepath.Join(dir, "b.jsonl")
	writeFile(t, a, "record a\n")
	writeFile(t, b, "record b\n")

	w := mustNew(t, dir)
	first := poll(t, w, a, b)
	if len(first) != 2 {
		t.Fatalf("outcomes = %+v, want two fresh sources", first)
	}
	ingestAll(t, w, first)
	idAt := map[string]string{first[0].Path: first[0].SourceID, first[1].Path: first[1].SourceID}

	// Swap both files through a third name: each path changes incarnation
	// at once, and both sources must survive the swap.
	tmp := filepath.Join(dir, "tmp.jsonl")
	for _, step := range [][2]string{{a, tmp}, {b, a}, {tmp, b}} {
		if err := os.Rename(step[0], step[1]); err != nil {
			t.Fatalf("rename %s to %s: %v", step[0], step[1], err)
		}
	}
	swapped := poll(t, w, a, b)
	if len(swapped) != 2 {
		t.Fatalf("outcomes = %+v, want two", swapped)
	}
	for _, o := range swapped {
		if o.Fresh || o.Missing {
			t.Fatalf("outcome = %+v, want a continued source", o)
		}
		want := idAt[normalizePath(b)] // the incarnation now at this path came from the other path
		if o.Path == normalizePath(b) {
			want = idAt[normalizePath(a)]
		}
		if o.SourceID != want {
			t.Fatalf("outcome = %+v, want source %s that moved here", o, want)
		}
		if o.Cursor != 9 {
			t.Fatalf("cursor = %d, want the acknowledged cursor 9", o.Cursor)
		}
	}
}

func TestNormalizePathResolvesSymlinkedDirectories(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if got, want := normalizePath(filepath.Join(link, "s.jsonl")), filepath.Join(real, "s.jsonl"); got != want {
		t.Fatalf("normalizePath = %q, want %q", got, want)
	}
	// A file that does not exist still normalizes to the same key.
	if got, want := normalizePath(filepath.Join(link, "absent.jsonl")), filepath.Join(real, "absent.jsonl"); got != want {
		t.Fatalf("normalizePath for an absent file = %q, want %q", got, want)
	}
}

func TestStateFilesAreOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.jsonl")
	writeFile(t, path, "x\n")

	w := mustNew(t, dir)
	poll(t, w, path)
	info, err := os.Stat(stateFilePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("state permissions = %o, want 600", perm)
	}
}
