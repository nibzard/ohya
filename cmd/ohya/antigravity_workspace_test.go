package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nibzard/ohya/internal/antigravity"
	"github.com/nibzard/ohya/internal/store"
)

const agyConversation = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"

const agyFirstRecord = `{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-11-22T20:00:01Z","content":"<USER_REQUEST>\nfix the auth bug\n</USER_REQUEST>\n"}` + "\n"

const agySecondRecord = `{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-11-22T20:00:04Z","model":"gemini-4-neon-high","input_tokens":600,"cache_read_tokens":4800,"output_tokens":40,"content":"Fixed the nil pointer in auth.go."}` + "\n"

func transcriptPath(brain, conversation string) string {
	return filepath.Join(brain, conversation, ".system_generated", "logs", "transcript_full.jsonl")
}

func writeConversationIndex(t *testing.T, path string, rows ...[2]any) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(path)+"?_journal_mode=wal")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE conversation_summaries (conversation_id TEXT PRIMARY KEY, workspace_uris TEXT)`); err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		var uris any
		if row[1] != nil {
			uris = row[1]
		}
		if _, err := db.Exec(`INSERT INTO conversation_summaries VALUES (?, ?)`, row[0], uris); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func gitCheckout(t *testing.T, remote string) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, ".git", "config"), "[remote \"origin\"]\n\turl = "+remote+"\n")
	return dir
}

func fileURI(path string) string {
	return (&url.URL{Scheme: "file", Path: path}).String()
}

func TestAntigravitySourceWorkspaceReadsTheIndex(t *testing.T) {
	root := t.TempDir()
	brain := filepath.Join(root, "brain")
	index := filepath.Join(root, conversationIndex)
	writeConversationIndex(t, index,
		[2]any{agyConversation, `["file:///work/with%20space/app/"]`},
		[2]any{"two", `["vscode-remote://ssh-remote+host/home/me/proj","file://localhost/work/second"]`},
		[2]any{"null", nil},
		[2]any{"empty", `[]`},
		[2]any{"malformed", `{"workspace_uris": "/work/x"}`},
		[2]any{"remote", `["file://other-host/work/app"]`},
		[2]any{"relative", `["file:relative/path"]`},
	)
	before, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}

	var log bytes.Buffer
	lookup := antigravitySourceWorkspace(brain, &log)
	for name, c := range map[string]struct {
		source string
		path   string
		found  bool
	}{
		"percent-encoded": {transcriptPath(brain, agyConversation), "/work/with space/app", true},
		"second entry":    {transcriptPath(brain, "two"), "/work/second", true},
		"null":            {transcriptPath(brain, "null"), "", false},
		"empty list":      {transcriptPath(brain, "empty"), "", false},
		"malformed":       {transcriptPath(brain, "malformed"), "", false},
		"remote host":     {transcriptPath(brain, "remote"), "", false},
		"relative":        {transcriptPath(brain, "relative"), "", false},
		"missing row":     {transcriptPath(brain, "unknown"), "", false},
		"other file":      {filepath.Join(brain, "other.jsonl"), "", false},
	} {
		if path, found := lookup(c.source); path != c.path || found != c.found {
			t.Errorf("%s: lookup = (%q, %v); want (%q, %v)", name, path, found, c.path, c.found)
		}
	}

	after, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("the lookup changed the index")
	}
	for _, sidecar := range []string{index + "-wal", index + "-shm"} {
		if _, err := os.Stat(sidecar); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s exists after the lookup", filepath.Base(sidecar))
		}
	}
	if got := strings.Count(log.String(), "\n"); got != 1 || !strings.Contains(log.String(), "is not a JSON list") {
		t.Fatalf("the lookup reported %d lines, want one about the malformed value:\n%s", got, log.String())
	}
}

func indexFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		files[e.Name()] = string(data)
	}
	return files
}

func TestAntigravitySourceWorkspaceLeavesALiveIndexAlone(t *testing.T) {
	root := t.TempDir()
	brain := filepath.Join(root, "brain")
	writer, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(filepath.Join(root, conversationIndex))+"?_journal_mode=wal")
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.Exec(`CREATE TABLE conversation_summaries (conversation_id TEXT PRIMARY KEY, workspace_uris TEXT);
		INSERT INTO conversation_summaries VALUES ('` + agyConversation + `', '["file:///work/app"]')`); err != nil {
		t.Fatal(err)
	}
	before := indexFiles(t, root)

	antigravitySourceWorkspace(brain, io.Discard)(transcriptPath(brain, agyConversation))

	after := indexFiles(t, root)
	if len(before) != len(after) {
		t.Fatalf("the index directory holds %d files before and %d after the lookup", len(before), len(after))
	}
	for name, data := range before {
		if after[name] != data {
			t.Fatalf("%s changed during the lookup", name)
		}
	}
}

func TestAntigravitySourceWorkspaceReportsAnUnreadableIndexOnce(t *testing.T) {
	root := t.TempDir()
	brain := filepath.Join(root, "brain")
	var log bytes.Buffer
	lookup := antigravitySourceWorkspace(brain, &log)
	if path, found := lookup("/somewhere/other.jsonl"); path != "" || found || log.Len() > 0 {
		t.Fatalf("a source that is no conversation transcript: (%q, %v), log %q", path, found, log.String())
	}
	for range 2 {
		if path, found := lookup(transcriptPath(brain, agyConversation)); path != "" || found {
			t.Fatalf("lookup without an index = (%q, %v)", path, found)
		}
	}
	if n := strings.Count(log.String(), "\n"); n != 1 || !strings.Contains(log.String(), "no such file or directory") {
		t.Fatalf("the missing index is reported %d times, want once with its cause:\n%s", n, log.String())
	}

	writeFile(t, filepath.Join(root, conversationIndex), "not a database")
	log.Reset()
	lookup = antigravitySourceWorkspace(brain, &log)
	for range 2 {
		lookup(transcriptPath(brain, agyConversation))
	}
	if n := strings.Count(log.String(), "\n"); n != 1 || !strings.Contains(log.String(), conversationIndex) {
		t.Fatalf("an index that is no database is reported %d times, want once with its path:\n%s", n, log.String())
	}
}

func antigravitySession(t *testing.T, dbPath string) (cwd, repo sql.NullString) {
	t.Helper()
	hub, err := store.OpenReadOnly(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()
	reader, err := hub.Reader()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if err := reader.QueryRowContext(context.Background(),
		`SELECT cwd, repo FROM sessions WHERE agent = ?`, antigravity.AgentName).Scan(&cwd, &repo); err != nil {
		t.Fatal(err)
	}
	return cwd, repo
}

func wantSessionWorkspace(t *testing.T, dbPath, workspace string) {
	t.Helper()
	cwd, repo := antigravitySession(t, dbPath)
	if !cwd.Valid || cwd.String != workspace || !repo.Valid || repo.String != "github.com/nibzard/agy-app" {
		t.Fatalf("session = (%+v, %+v); want the workspace %q and its repository", cwd, repo, workspace)
	}
}

func TestAntigravityIngestSetsTheWorkspace(t *testing.T) {
	root := t.TempDir()
	brain := filepath.Join(root, "brain")
	app := gitCheckout(t, "https://github.com/nibzard/agy-app.git")
	writeFile(t, transcriptPath(brain, agyConversation), agyFirstRecord+agySecondRecord)
	writeConversationIndex(t, filepath.Join(root, conversationIndex), [2]any{agyConversation, `["` + fileURI(app) + `"]`})

	var stdout, stderr bytes.Buffer
	code := cliMain([]string{
		"ingest", "--once",
		"--db", filepath.Join(root, "hub.db"),
		"--state", filepath.Join(root, "state"),
		"--claude-root", "", "--codex-root", "", "--pi-root", "",
		"--antigravity-root", brain,
	}, &stdout, &stderr)
	if code != 0 || stderr.Len() > 0 {
		t.Fatalf("ingest exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "imported 2 events from 1 sources") {
		t.Fatalf("unexpected ingest summary: %q", stdout.String())
	}
	wantSessionWorkspace(t, filepath.Join(root, "hub.db"), app)
}

func TestAntigravityIngestWithoutIndexReportsOnce(t *testing.T) {
	root := t.TempDir()
	brain := filepath.Join(root, "brain")
	writeFile(t, transcriptPath(brain, agyConversation), agyFirstRecord)

	var stdout, stderr bytes.Buffer
	code := cliMain([]string{
		"ingest", "--once",
		"--db", filepath.Join(root, "hub.db"),
		"--state", filepath.Join(root, "state"),
		"--claude-root", "", "--codex-root", "", "--pi-root", "",
		"--antigravity-root", brain,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("ingest exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if n := strings.Count(stderr.String(), "\n"); n != 1 || !strings.Contains(stderr.String(), conversationIndex) {
		t.Fatalf("ingest without an index reported %d lines, want one that names the index:\n%s", n, stderr.String())
	}
	if cwd, _ := antigravitySession(t, filepath.Join(root, "hub.db")); cwd.Valid {
		t.Fatalf("session cwd = %q without an index", cwd.String)
	}
}

func newAntigravityEnv(t *testing.T) (*sliceEnv, string) {
	t.Helper()
	dir := t.TempDir()
	e := &sliceEnv{
		root:     filepath.Join(dir, "brain"),
		dbPath:   filepath.Join(dir, "hub", "hub.db"),
		stateDir: filepath.Join(dir, "state"),
		agent:    antigravity.AgentName,
	}
	transcript := transcriptPath(e.root, agyConversation)
	writeFile(t, transcript, agyFirstRecord)
	return e, transcript
}

func TestIngestorRetriesASourceWithoutWorkspaceOnlyAfterGrowth(t *testing.T) {
	e, transcript := newAntigravityEnv(t)
	app := gitCheckout(t, "https://github.com/nibzard/agy-app.git")
	known := false
	lookups := 0
	r, done := e.newRunner(t, io.Discard)
	defer done()
	r.sourceWorkspace = func(string) (string, bool) {
		lookups++
		return app, known
	}
	ctx := context.Background()

	for range 2 {
		if _, err := r.poll(ctx, e.dirs(), false); err != nil {
			t.Fatal(err)
		}
	}
	known = true
	if _, err := r.poll(ctx, e.dirs(), false); err != nil {
		t.Fatal(err)
	}
	if lookups != 1 {
		t.Fatalf("lookups = %d after three passes without new events of the source; want 1", lookups)
	}
	if cwd, _ := antigravitySession(t, e.dbPath); cwd.Valid {
		t.Fatalf("session cwd = %q before the source grows", cwd.String)
	}

	appendFile(t, transcript, agySecondRecord)
	if stats, err := r.poll(ctx, e.dirs(), false); err != nil || stats.inserted != 1 {
		t.Fatalf("poll = %+v, %v; want 1 inserted event", stats, err)
	}
	if lookups != 2 {
		t.Fatalf("lookups = %d after the source grew; want 2", lookups)
	}
	wantSessionWorkspace(t, e.dbPath, app)
}

func TestIngestorFillsAnOldSourceInAPassWithoutNewEvents(t *testing.T) {
	e, _ := newAntigravityEnv(t)
	app := gitCheckout(t, "https://github.com/nibzard/agy-app.git")
	known := false
	workspace := func(string) (string, bool) { return app, known }
	ctx := context.Background()

	r, done := e.newRunner(t, io.Discard)
	r.sourceWorkspace = workspace
	if _, err := r.poll(ctx, e.dirs(), false); err != nil {
		t.Fatal(err)
	}
	done()
	if cwd, _ := antigravitySession(t, e.dbPath); cwd.Valid {
		t.Fatalf("session cwd = %q before the index knows the conversation", cwd.String)
	}

	known = true
	r2, done2 := e.newRunner(t, io.Discard)
	defer done2()
	r2.sourceWorkspace = workspace
	if stats, err := r2.poll(ctx, e.dirs(), false); err != nil || stats.inserted != 0 {
		t.Fatalf("poll = %+v, %v; want no inserted event", stats, err)
	}
	wantSessionWorkspace(t, e.dbPath, app)
}

func TestIngestorReportsAFailedWorkspaceOnce(t *testing.T) {
	e, transcript := newAntigravityEnv(t)
	var stderr bytes.Buffer
	r, done := e.newRunner(t, &stderr)
	defer done()
	r.sourceWorkspace = func(string) (string, bool) { return "/work/\x00agy", true }
	ctx := context.Background()

	if _, err := r.poll(ctx, e.dirs(), false); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(stderr.String(), "set the working directory"); n != 1 {
		t.Fatalf("the failed write is reported %d times, want once:\n%s", n, stderr.String())
	}
	if _, err := r.poll(ctx, e.dirs(), false); err != nil {
		t.Fatal(err)
	}
	appendFile(t, transcript, agySecondRecord)
	if _, err := r.poll(ctx, e.dirs(), false); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(stderr.String(), "set the working directory"); n != 1 {
		t.Fatalf("the failed write is reported %d times after a retry, want once:\n%s", n, stderr.String())
	}
}
