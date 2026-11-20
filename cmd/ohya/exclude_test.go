package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nibzard/ohya/internal/observe/observertest"
	"github.com/nibzard/ohya/internal/watch"
)

func TestDiscoverSessionFilesExcludeGlobs(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "projects")
	guardian := filepath.Join(root, "-home-agent-guardian", "session-a.jsonl")
	normal := filepath.Join(root, "-home-agent-ohya", "session-b.jsonl")
	nested := filepath.Join(root, "-home-agent-guardian", "subagents", "agent-1.jsonl")
	for _, path := range []string{guardian, normal, nested} {
		writeFile(t, path, "{}\n")
	}
	all := []string{guardian, nested, normal}
	relativeGlob := filepath.Join(root, "-home-agent-guardian", "*.jsonl")

	cases := []struct {
		name        string
		globs       []string
		want        []string
		wantSkipped int
		wantErr     string
	}{
		{"no globs keeps everything", nil, all, 0, ""},
		{"glob excludes one file", []string{guardian}, []string{nested, normal}, 1, ""},
		{"glob excludes one directory level", []string{relativeGlob}, []string{nested, normal}, 1, ""},
		{"star does not cross the separator", []string{"*-guardian-*"}, all, 0, ""},
		{"several globs union", []string{relativeGlob, filepath.Join(root, "-home-agent-ohya", "*")}, []string{nested}, 2, ""},
		{"question mark matches one character", []string{filepath.Join(root, "-home-agent-ohya", "session-?.jsonl")}, []string{guardian, nested}, 1, ""},
		{"non-matching glob keeps everything", []string{filepath.Join(base, "elsewhere", "*")}, all, 0, ""},
		{"malformed glob fails loudly", []string{filepath.Join(root, "[")}, nil, 0, "--exclude glob"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files, excluded, err := discoverSessionFiles([]agentDir{{dir: root}}, excludeGlobs(tc.globs), false)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(files, tc.want) {
				t.Fatalf("files = %v, want %v", files, tc.want)
			}
			if len(excluded) != tc.wantSkipped {
				t.Fatalf("excluded = %v, want %d paths", excluded, tc.wantSkipped)
			}
		})
	}
}

func TestDiscoverSessionFilesExcludeMatchesRelativeRoot(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "projects")
	writeFile(t, filepath.Join(root, "session.jsonl"), "{}\n")
	t.Chdir(base)

	files, excluded, err := discoverSessionFiles([]agentDir{{dir: "projects"}}, excludeGlobs{filepath.Join(root, "*.jsonl")}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 || len(excluded) != 1 {
		t.Fatalf("files = %v, excluded = %v, want the only file excluded", files, excluded)
	}
}

func TestDiscoverSessionFilesExcludeMatchesSymlinkedPathForm(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	sessions := filepath.Join(real, "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(sessions, "session.jsonl"), "{}\n")
	if err := os.Symlink(real, filepath.Join(base, "alias")); err != nil {
		t.Skipf("cannot create a symlink: %v", err)
	}
	seen := filepath.Join(base, "alias", "sessions")
	resolved := filepath.Join(sessions, "*.jsonl")

	files, excluded, err := discoverSessionFiles([]agentDir{{dir: seen}}, excludeGlobs{resolved}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 || len(excluded) != 1 {
		t.Fatalf("files = %v, excluded = %v, want the file excluded through its resolved path", files, excluded)
	}
	if _, ok := excludedPathSet(excluded)[resolved]; ok {
		t.Fatalf("a glob is not a path: %v", excluded)
	}
	for _, form := range excludePathForms(excluded[0]) {
		if form != filepath.Join(seen, "session.jsonl") && form != filepath.Join(sessions, "session.jsonl") {
			t.Fatalf("form %q is neither the seen nor the resolved path", form)
		}
	}
}

func TestExcludeGlobFlagValidation(t *testing.T) {
	var globs excludeGlobs
	valid := filepath.Join(t.TempDir(), "guardian") + "/*"
	if err := globs.Set(valid); err != nil {
		t.Fatalf("valid glob rejected: %v", err)
	}
	if got := globs.String(); got != valid {
		t.Fatalf("String = %q, want %q", got, valid)
	}
	if err := globs.Set(""); err != nil || globs.String() != "" {
		t.Fatalf("an empty value must clear the list: %v %q", err, globs.String())
	}
	for _, bad := range []string{"[", "ok[", "lead\r", "/one/*\n"} {
		if err := (&excludeGlobs{}).Set(bad); err == nil {
			t.Fatalf("glob %q accepted", bad)
		}
	}
	joined := excludeGlobs{}
	if err := joined.Set("/one/*\n/two/*"); err != nil || len(joined) != 2 {
		t.Fatalf("joined restore = %v (%d globs), want two restored globs", err, len(joined))
	}
}

func TestIngestExcludeSkipsUnregisteredFiles(t *testing.T) {
	e := newSliceEnv(t)
	copyFixture(t, e.session("main-session.jsonl"), "main-session.jsonl")
	excludes := excludeGlobs{filepath.Join(e.root, "fixture-project", "main-session.jsonl")}
	r, done := e.newRunnerWithExcludes(t, excludes, io.Discard)
	defer done()

	stats, err := r.poll(context.Background(), e.dirs(), false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.sources != 0 || stats.inserted != 0 || stats.excluded != 1 {
		t.Fatalf("pass = %+v, want the only file excluded", stats)
	}
	hub := openHub(t, e.dbPath)
	defer hub.Close()
	if got := countRows(t, hub.Writer(), `SELECT count(*) FROM sources`); got != 0 {
		t.Fatalf("sources = %d, want none registered", got)
	}
	if _, err := os.Stat(filepath.Join(e.stateDir, "sources", e.agent+".json")); !os.IsNotExist(err) {
		t.Fatalf("checkpoint state written: %v", err)
	}
}

func TestIngestExcludeArrivingAfterRegistration(t *testing.T) {
	e := newSliceEnv(t)
	session := e.session("main-session.jsonl")
	copyFixture(t, session, "main-session.jsonl")

	r, done := e.newRunner(t, &strings.Builder{})
	stats, err := r.poll(context.Background(), e.dirs(), false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.sources != 1 || stats.inserted != 16 {
		t.Fatalf("first pass = %+v, want the session imported", stats)
	}
	done()

	hub := openHub(t, e.dbPath)
	storedEvents := countRows(t, hub.Writer(), `SELECT count(*) FROM events`)
	sourceID, cursor := soleSourceID(t, hub.Writer())
	hub.Close()

	var stderr strings.Builder
	excluded := newIngestor(openHub(t, e.dbPath), mustWatch(t, e.stateDir, e.agent), e.agent, e.deviceID(t), 0, excludeGlobs{session}, &stderr, nil, nil)
	stats, err = excluded.poll(context.Background(), e.dirs(), false)
	if err != nil {
		t.Fatal(err)
	}
	if stats.sources != 0 || stats.inserted != 0 || stats.excluded != 1 {
		t.Fatalf("second pass = %+v, want the source skipped by the exclusion", stats)
	}
	if !strings.Contains(stderr.String(), "now matches --exclude and stops being captured going forward") {
		t.Fatalf("stderr lacks the exclusion explanation:\n%s", stderr.String())
	}
	if strings.Contains(stderr.String(), "disappeared") {
		t.Fatalf("an excluded source must not be reported as disappeared:\n%s", stderr.String())
	}

	hub = openHub(t, e.dbPath)
	defer hub.Close()
	if got := countRows(t, hub.Writer(), `SELECT count(*) FROM events`); got != storedEvents {
		t.Fatalf("stored events = %d, want %d preserved on the hub", got, storedEvents)
	}
	id, kept := soleSourceID(t, hub.Writer())
	if id != sourceID || kept != cursor {
		t.Fatalf("source = %s@%d, want %s@%d with the checkpoint kept", id, kept, sourceID, cursor)
	}
}

func mustWatch(t *testing.T, stateDir, agent string) *watch.Watcher {
	t.Helper()
	w, err := watch.New(stateDir, agent, watch.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestIngestOnceReportsExcludedFiles(t *testing.T) {
	e := newSliceEnv(t)
	copyFixture(t, e.session("main-session.jsonl"), "main-session.jsonl")
	dir := t.TempDir()
	args := []string{"ingest", "--once",
		"--db", filepath.Join(dir, "hub.db"),
		"--state", filepath.Join(dir, "state"),
		"--claude-root", e.root, "--codex-root", "", "--pi-root", "",
		"--exclude", filepath.Join(e.root, "fixture-project", "main-session.jsonl")}

	var stdout, stderr bytes.Buffer
	code := cliMain(args, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code %d, stderr %q", code, stderr.String())
	}
	summary := stdout.String()
	if !strings.Contains(summary, "0 events from 0 sources") || !strings.Contains(summary, "1 files excluded by --exclude (claude-code: 1)") {
		t.Fatalf("summary = %q", summary)
	}
	if !strings.Contains(stderr.String(), "excluding session files matching:") {
		t.Fatalf("stderr lacks the active-exclusion line: %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = cliMain(append(args, "--json"), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("json code %d, stderr %q", code, stderr.String())
	}
	goldenCompare(t, stdout.String(),
		`{"schema_version":1,"command":"ohya ingest","data":{"events":0,"sources":0,"excluded":{"claude-code":1},"failed":0,"stopped":0,"stops":[]},"error":null}`+"\n")
}

func TestExcludeFlagUsageErrors(t *testing.T) {
	for _, command := range []string{"ingest", "join"} {
		var stdout, stderr bytes.Buffer
		code := cliMain([]string{command, "--exclude", "["}, &stdout, &stderr)
		if code != 2 || !strings.Contains(stderr.String(), "--exclude glob") {
			t.Fatalf("%s --exclude [: code %d stderr %q", command, code, stderr.String())
		}
	}
	for _, command := range []string{"ingest --help", "join --help", "setup --help"} {
		var stdout, stderr bytes.Buffer
		if code := cliMain(strings.Fields(command), &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "--exclude") {
			t.Fatalf("%s: code %d, help lacks --exclude: %s", command, code, stdout.String())
		}
	}
}

func TestJoinDiscoverAccountsAndClassifies(t *testing.T) {
	base := t.TempDir()
	codexRoot := filepath.Join(base, "sessions")
	writeFile(t, filepath.Join(codexRoot, "rollout-a.jsonl"), "{}\n")
	writeFile(t, filepath.Join(codexRoot, "rollout-guardian-b.jsonl"), "{}\n")
	excludes := excludeGlobs{filepath.Join(codexRoot, "*guardian*")}
	obs := observertest.New()
	var stderr strings.Builder

	discover, excluded := joinDiscover([]agentDir{{dir: codexRoot}}, "codex", excludes, false, obs, &stderr)
	candidates, err := discover()
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || !strings.HasSuffix(candidates[0], "rollout-a.jsonl") {
		t.Fatalf("candidates = %v", candidates)
	}
	if obs.Gauge("discovery_excluded", "codex") != 1 {
		t.Fatalf("discovery_excluded gauge = %v, want 1", obs.Gauge("discovery_excluded", "codex"))
	}
	if !strings.Contains(stderr.String(), "codex: 1 session files excluded by --exclude") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if !excluded(filepath.Join(codexRoot, "rollout-guardian-b.jsonl")) {
		t.Fatal("the classifier must recognize the excluded path")
	}
	if excluded(filepath.Join(codexRoot, "rollout-a.jsonl")) {
		t.Fatal("the classifier must not claim a kept path")
	}

	stderr.Reset()
	if _, err := discover(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stderr.String(), "excluded by --exclude") {
		t.Fatalf("an unchanged count must not log again: %q", stderr.String())
	}
}

func TestJoinDiscoverWithoutExcludesStaysSilent(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "sessions")
	writeFile(t, filepath.Join(root, "a.jsonl"), "{}\n")
	obs := observertest.New()
	var stderr strings.Builder

	discover, excluded := joinDiscover([]agentDir{{dir: root}}, "codex", nil, false, obs, &stderr)
	candidates, err := discover()
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || stderr.Len() != 0 || obs.Gauge("discovery_excluded", "codex") != 0 {
		t.Fatalf("candidates = %v, stderr = %q", candidates, stderr.String())
	}
	if excluded(filepath.Join(root, "a.jsonl")) {
		t.Fatal("no exclusion configured, nothing may classify as excluded")
	}
}
