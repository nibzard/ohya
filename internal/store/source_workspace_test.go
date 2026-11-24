package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/nibzard/ohya/internal/claudecode"
	"github.com/nibzard/ohya/internal/workspace"
)

func mustSetSourceWorkspace(t *testing.T, hub *DB, device, sourceID, path string) {
	t.Helper()
	if err := hub.SetSourceWorkspace(context.Background(), device, sourceID, path); err != nil {
		t.Fatal(err)
	}
}

func mustSourcesWithoutWorkspace(t *testing.T, hub *DB, device, agent string) []string {
	t.Helper()
	sources, err := hub.SourcesWithoutWorkspace(context.Background(), device, agent)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range sources {
		ids = append(ids, s.ID)
	}
	return ids
}

func storedSourceWorkspace(t *testing.T, hub *DB, sourceID string) string {
	t.Helper()
	var path sql.NullString
	if err := hub.Writer().QueryRow(`SELECT workspace_path FROM sources WHERE id = ?`, sourceID).Scan(&path); err != nil {
		t.Fatal(err)
	}
	return path.String
}

func TestSetSourceWorkspaceFillsSessionDirectories(t *testing.T) {
	hub := ingestHub(t)
	mustRegister(t, hub, "s1", "/brain/conversations/c1/.system_generated/logs/transcript_full.jsonl")
	a := sessionAt(0, "a", "")
	b := sessionAt(a.End, "b", "")
	c := sessionAt(b.End, "c", "/work/agy")
	d := sessionAt(c.End, "d", "/work/other")
	mustIngest(t, hub, Batch{SourceID: "s1", Events: []Event{a, b, c, d}})
	if res := mustObserve(t, hub, deviceHex, workspace.Observation{Path: "/work/agy", Repo: "github.com/x/agy"}); res.Applied != 1 {
		t.Fatalf("observe = %+v, want one applied observation", res)
	}
	if got := mustSourcesWithoutWorkspace(t, hub, deviceHex, "claude-code"); !slices.Equal(got, []string{"s1"}) {
		t.Fatalf("sources without a workspace = %v, want [s1]", got)
	}

	mustSetSourceWorkspace(t, hub, deviceHex, "s1", "/work/agy")

	for native, wantCwd := range map[string]string{"a": "/work/agy", "b": "/work/agy"} {
		row := sessionRowFor(t, hub, SessionID(deviceHex, "claude-code", native))
		if !row.Cwd.Valid || row.Cwd.String != wantCwd || !row.Repo.Valid || row.Repo.String != "github.com/x/agy" {
			t.Errorf("session %s = cwd %v repo %v, want %s and the observed repository", native, row.Cwd, row.Repo, wantCwd)
		}
	}
	for native, wantCwd := range map[string]string{"c": "/work/agy", "d": "/work/other"} {
		row := sessionRowFor(t, hub, SessionID(deviceHex, "claude-code", native))
		if !row.Cwd.Valid || row.Cwd.String != wantCwd {
			t.Errorf("session %s = cwd %v, want %s unchanged", native, row.Cwd, wantCwd)
		}
	}
	if got := mustSourcesWithoutWorkspace(t, hub, deviceHex, "claude-code"); len(got) != 0 {
		t.Fatalf("sources without a workspace = %v, want none", got)
	}
	if got := mustSourcesWithoutWorkspace(t, hub, otherDevice, "claude-code"); len(got) != 0 {
		t.Fatalf("sources of another device without a workspace = %v, want none", got)
	}
}

func TestIngestUsesSourceWorkspaceWhenRecordsCarryNone(t *testing.T) {
	hub := ingestHub(t)
	mustRegister(t, hub, "s1", "/s1.jsonl")
	mustRegister(t, hub, "s2", "/s2.jsonl")
	first := sessionAt(0, "first", "")
	mustIngest(t, hub, Batch{SourceID: "s1", Events: []Event{first}})
	mustSetSourceWorkspace(t, hub, deviceHex, "s1", "/work/agy")

	later := mkEvent(first.End, rawUser("later"))
	mustIngest(t, hub, Batch{SourceID: "s1", Events: []Event{later}})
	row := sessionRowFor(t, hub, SessionID(deviceHex, "claude-code", "sess-1"))
	if !row.Cwd.Valid || row.Cwd.String != "/work/agy" {
		t.Fatalf("a new session of the source = cwd %v, want the source workspace", row.Cwd)
	}

	shared := mkEvent(0, rawUser("shared"))
	mustIngest(t, hub, Batch{SourceID: "s2", Events: []Event{shared}})
	row = sessionRowFor(t, hub, SessionID(deviceHex, "claude-code", "sess-1"))
	if !row.Cwd.Valid || row.Cwd.String != "/work/agy" {
		t.Fatalf("a batch from a source without a workspace changed the cwd to %v", row.Cwd)
	}

	mustSetSourceWorkspace(t, hub, deviceHex, "s2", "/work/other")
	own := sessionAt(shared.End, "own", "/work/recorded")
	none := sessionAt(own.End, "none", "")
	mustIngest(t, hub, Batch{SourceID: "s2", Events: []Event{own, none}})
	for native, wantCwd := range map[string]string{"own": "/work/recorded", "none": "/work/other"} {
		row = sessionRowFor(t, hub, SessionID(deviceHex, "claude-code", native))
		if !row.Cwd.Valid || row.Cwd.String != wantCwd {
			t.Errorf("session %s = cwd %v, want %s", native, row.Cwd, wantCwd)
		}
	}
}

func TestSetSourceWorkspaceRefusesWhatItMust(t *testing.T) {
	ctx := context.Background()
	hub := ingestHub(t)
	mustRegister(t, hub, "s1", "/s1.jsonl")
	mustRegisterAs(t, hub, "foreign", otherDevice, "claude-code", "/foreign.jsonl")
	mustRegisterAs(t, hub, "doomed", deviceHex, "claude-code", "/doomed.jsonl")
	mustIngest(t, hub, Batch{SourceID: "doomed", Events: []Event{sessionAt(0, "doomed", "")}})
	if _, _, err := hub.ApplyPurge(ctx, PurgeSelector{Session: "doomed"}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, device, source, path string
		want                       error
	}{
		{"unknown source", deviceHex, "missing", "/work/agy", ErrSourceNotRegistered},
		{"source of another device", deviceHex, "foreign", "/work/agy", ErrSourceOwnership},
		{"purged source", deviceHex, "doomed", "/work/agy", ErrSourcePurged},
		{"purged source, named by another device", otherDevice, "doomed", "/work/agy", ErrSourceOwnership},
		{"empty path", deviceHex, "s1", "", ErrSourceWorkspaceRejected},
		{"path above its bound", deviceHex, "s1", "/" + strings.Repeat("a", MaxWorkspacePathBytes), ErrSourceWorkspaceRejected},
		{"path with a NUL byte", deviceHex, "s1", "/work/\x00agy", ErrSourceWorkspaceRejected},
		{"path that is not text", deviceHex, "s1", "/work/\xffagy", ErrSourceWorkspaceRejected},
	} {
		err := hub.SetSourceWorkspace(ctx, tc.device, tc.source, tc.path)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	if got := mustSourcesWithoutWorkspace(t, hub, deviceHex, "claude-code"); !slices.Equal(got, []string{"s1"}) {
		t.Fatalf("a refused request changed the sources without a workspace: %v", got)
	}
}

func TestSetSourceWorkspaceWritesOnce(t *testing.T) {
	hub := ingestHub(t)
	mustRegister(t, hub, "s1", "/s1.jsonl")
	early := sessionAt(0, "early", "")
	mustIngest(t, hub, Batch{SourceID: "s1", Events: []Event{early}})

	mustSetSourceWorkspace(t, hub, deviceHex, "s1", "/work/agy")
	mustSetSourceWorkspace(t, hub, deviceHex, "s1", "/work/agy")
	late := sessionAt(early.End, "late", "")
	mustIngest(t, hub, Batch{SourceID: "s1", Events: []Event{late}})
	mustSetSourceWorkspace(t, hub, deviceHex, "s1", "/work/moved")

	if got := storedSourceWorkspace(t, hub, "s1"); got != "/work/agy" {
		t.Fatalf("stored workspace = %q, want /work/agy", got)
	}
	for _, native := range []string{"early", "late"} {
		row := sessionRowFor(t, hub, SessionID(deviceHex, "claude-code", native))
		if !row.Cwd.Valid || row.Cwd.String != "/work/agy" {
			t.Errorf("session %s = cwd %v, want /work/agy", native, row.Cwd)
		}
	}
}

func TestSourcesWithoutWorkspaceListsOneDeviceAndAgent(t *testing.T) {
	hub := ingestHub(t)
	mustRegister(t, hub, "s2", "/s2.jsonl")
	mustRegister(t, hub, "s1", "/s1.jsonl")
	mustRegisterAs(t, hub, "s3", deviceHex, "codex", "/s3.jsonl")
	mustRegisterAs(t, hub, "s4", otherDevice, "claude-code", "/s4.jsonl")
	mustSetSourceWorkspace(t, hub, deviceHex, "s2", "/work/agy")

	sources, err := hub.SourcesWithoutWorkspace(context.Background(), deviceHex, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	want := []SourcePath{{ID: "s1", Path: "/s1.jsonl"}}
	if !slices.Equal(sources, want) {
		t.Fatalf("SourcesWithoutWorkspace = %+v, want %+v", sources, want)
	}
}

func TestPurgeKeepsSourceWorkspaceAcrossReallow(t *testing.T) {
	ctx := context.Background()
	hub := ingestHub(t)
	mustRegister(t, hub, "s1", "/s1.jsonl")
	first := sessionAt(0, "first", "")
	mustIngest(t, hub, Batch{SourceID: "s1", Events: []Event{first}})
	mustSetSourceWorkspace(t, hub, deviceHex, "s1", "/work/agy")

	if _, _, err := hub.ApplyPurge(ctx, PurgeSelector{Session: "first"}); err != nil {
		t.Fatal(err)
	}
	if err := hub.SetSourceWorkspace(ctx, deviceHex, "s1", "/work/agy"); !errors.Is(err, ErrSourcePurged) {
		t.Fatalf("set on a purged source: err = %v, want ErrSourcePurged", err)
	}
	info, err := hub.ReallowSource(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if info.Workspace != "/work/agy" {
		t.Fatalf("reallow reports workspace %q, want /work/agy", info.Workspace)
	}
	if got := mustSourcesWithoutWorkspace(t, hub, deviceHex, "claude-code"); len(got) != 0 {
		t.Fatalf("after reallow, sources without a workspace = %v, want none", got)
	}
	mustSetSourceWorkspace(t, hub, deviceHex, "s1", "/work/moved")
	if got := storedSourceWorkspace(t, hub, "s1"); got != "/work/agy" {
		t.Fatalf("workspace after reallow = %q, want the purged value /work/agy", got)
	}
	second := mkEvent(first.End, rawUser("second"))
	mustIngest(t, hub, Batch{SourceID: "s1", Events: []Event{second}})
	row := sessionRowFor(t, hub, SessionID(deviceHex, "claude-code", "sess-1"))
	if !row.Cwd.Valid || row.Cwd.String != "/work/agy" {
		t.Fatalf("a new session after reallow = cwd %v, want /work/agy", row.Cwd)
	}
}

func TestReDeriveKeepsSourceWorkspace(t *testing.T) {
	ctx := context.Background()
	hub := ingestHub(t)
	mustRegister(t, hub, "s1", "/s1.jsonl")
	stored, err := parseFixture(t, "main-session.jsonl", "s1")
	if err != nil {
		t.Fatalf("fixture parse stopped: %v", err)
	}
	noDirectory := func(e *Event) { e.Cwd = ""; e.Repo = "" }
	for i := range stored {
		noDirectory(&stored[i])
	}
	mustIngest(t, hub, Batch{SourceID: "s1", Events: stored})
	mustSetSourceWorkspace(t, hub, deviceHex, "s1", "/work/agy")
	if res := mustObserve(t, hub, deviceHex, workspace.Observation{Path: "/work/agy", Repo: "github.com/x/agy"}); res.Applied != 1 {
		t.Fatalf("observe = %+v, want one applied observation", res)
	}
	native := fixtureNative(t, hub)

	before := dumpHub(t, hub)
	if _, err := hub.ReDerive(ctx, []ParserDeriver{claudeDeriver(claudecode.OutputVersion+1, noDirectory)}); err != nil {
		t.Fatal(err)
	}
	if after := dumpHub(t, hub); stripVersions(after) != stripVersions(before) {
		t.Fatalf("re-derivation changed the records:\n%s", diffSnip(before, after))
	}
	row := sessionRowFor(t, hub, SessionID(deviceHex, "claude-code", native))
	if !row.Cwd.Valid || row.Cwd.String != "/work/agy" || !row.Repo.Valid || row.Repo.String != "github.com/x/agy" {
		t.Fatalf("the rebuilt session = cwd %v repo %v, want /work/agy and its repository", row.Cwd, row.Repo)
	}

	moved := func(e *Event) { noDirectory(e); e.NativeID = "moved" }
	if _, err := hub.ReDerive(ctx, []ParserDeriver{claudeDeriver(claudecode.OutputVersion+2, moved)}); err != nil {
		t.Fatal(err)
	}
	row = sessionRowFor(t, hub, SessionID(deviceHex, "claude-code", "moved"))
	if !row.Cwd.Valid || row.Cwd.String != "/work/agy" || !row.Repo.Valid || row.Repo.String != "github.com/x/agy" {
		t.Fatalf("the session under its new native ID = cwd %v repo %v, want /work/agy and its repository", row.Cwd, row.Repo)
	}
	var left int
	if err := hub.Writer().QueryRow(`SELECT count(*) FROM sessions WHERE id != ?`, SessionID(deviceHex, "claude-code", "moved")).Scan(&left); err != nil || left != 0 {
		t.Fatalf("sessions under the old native ID = %d, %v; want none", left, err)
	}
}
