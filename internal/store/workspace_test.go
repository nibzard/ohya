package store

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/nibzard/ohya/internal/workspace"
)

const otherDevice = "fedcba9876543210fedcba9876543210"

func sessionAt(offset int64, native, cwd string) Event {
	e := mkEvent(offset, rawUser(native+cwd))
	e.NativeID = native
	e.Cwd = cwd
	return e
}

func mustObserve(t *testing.T, hub *DB, device string, obs ...workspace.Observation) ObserveResult {
	t.Helper()
	res, err := hub.ObserveWorkspaces(context.Background(), device, obs)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func mustPending(t *testing.T, hub *DB, device, agent string) []string {
	t.Helper()
	paths, err := hub.PendingWorkspaces(context.Background(), device, agent, 256)
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func repoOf(t *testing.T, hub *DB, device, native string) string {
	t.Helper()
	return sessionRowFor(t, hub, SessionID(device, "claude-code", native)).Repo.String
}

func TestObserveWorkspacesFillsSessionRepositories(t *testing.T) {
	hub := ingestHub(t)
	mustRegister(t, hub, "s1", "/a.jsonl")
	a := sessionAt(0, "a", "/work/dioclet")
	b := sessionAt(a.End, "b", "/work/dioclet")
	c := sessionAt(b.End, "c", "/work/ohya")
	d := sessionAt(c.End, "d", "/work/codex")
	d.Repo = "github.com/nibzard/from-records"
	e := sessionAt(d.End, "e", "")
	mustIngest(t, hub, Batch{SourceID: "s1", Events: []Event{a, b, c, d, e}})

	if got := mustPending(t, hub, deviceHex, "claude-code"); !slices.Equal(got, []string{"/work/dioclet", "/work/ohya"}) {
		t.Fatalf("pending = %q, want the two cwds of sessions without a repository", got)
	}
	if got := mustPending(t, hub, otherDevice, "claude-code"); len(got) != 0 {
		t.Fatalf("pending for another device = %q", got)
	}
	if got := mustPending(t, hub, deviceHex, "codex"); len(got) != 0 {
		t.Fatalf("pending for another agent = %q", got)
	}

	res := mustObserve(t, hub, deviceHex,
		workspace.Observation{Path: "/work/dioclet", Repo: "github.com/nibzard/dioclet"},
		workspace.Observation{Path: "/work/ohya"},
		workspace.Observation{Path: "/work/codex", Repo: "github.com/nibzard/observed"},
		workspace.Observation{Path: "/not/a/session/cwd", Repo: "github.com/x/y"})
	if res != (ObserveResult{Applied: 3, Ignored: 1}) {
		t.Fatalf("observe = %+v", res)
	}
	for native, want := range map[string]string{
		"a": "github.com/nibzard/dioclet", "b": "github.com/nibzard/dioclet",
		"c": "", "d": "github.com/nibzard/from-records", "e": "",
	} {
		if got := repoOf(t, hub, deviceHex, native); got != want {
			t.Errorf("session %s repo = %q, want %q", native, got, want)
		}
	}
	if got := mustPending(t, hub, deviceHex, "claude-code"); len(got) != 0 {
		t.Fatalf("pending after observation = %q", got)
	}
	if n := countEvents(t, hub, `SELECT count(*) FROM workspaces WHERE path = '/not/a/session/cwd'`); n != 0 {
		t.Fatalf("an observation for an unknown path was stored")
	}
}

func TestObserveWorkspacesIsWriteOnce(t *testing.T) {
	hub := ingestHub(t)
	mustRegister(t, hub, "s1", "/a.jsonl")
	mustIngest(t, hub, Batch{SourceID: "s1", Events: []Event{sessionAt(0, "a", "/work/app")}})
	mustObserve(t, hub, deviceHex, workspace.Observation{Path: "/work/app", Repo: "github.com/x/first"})
	res := mustObserve(t, hub, deviceHex, workspace.Observation{Path: "/work/app", Repo: "github.com/x/second"})
	if res != (ObserveResult{Unchanged: 1}) {
		t.Fatalf("second observation = %+v, want unchanged", res)
	}
	if got := repoOf(t, hub, deviceHex, "a"); got != "github.com/x/first" {
		t.Fatalf("repo = %q, want the first observation", got)
	}
}

func TestObserveWorkspacesIsScopedToTheDevice(t *testing.T) {
	hub := ingestHub(t)
	mustRegister(t, hub, "s1", "/a.jsonl")
	if err := hub.RegisterSource(context.Background(), "s2", otherDevice, "claude-code", "/b.jsonl"); err != nil {
		t.Fatal(err)
	}
	mustIngest(t, hub, Batch{SourceID: "s1", Events: []Event{sessionAt(0, "a", "/work/app")}})
	mustIngestAs(t, hub, otherDevice, Batch{SourceID: "s2", Events: []Event{sessionAt(0, "b", "/work/app")}})

	res := mustObserve(t, hub, otherDevice, workspace.Observation{Path: "/work/app", Repo: "github.com/x/other"})
	if res.Applied != 1 {
		t.Fatalf("observe = %+v", res)
	}
	if got := repoOf(t, hub, deviceHex, "a"); got != "" {
		t.Fatalf("another device's observation set repo %q", got)
	}
	if got := repoOf(t, hub, otherDevice, "b"); got != "github.com/x/other" {
		t.Fatalf("repo = %q", got)
	}
}

func TestIngestUsesAnExistingObservation(t *testing.T) {
	hub := ingestHub(t)
	mustRegister(t, hub, "s1", "/a.jsonl")
	a := sessionAt(0, "a", "/work/app")
	mustIngest(t, hub, Batch{SourceID: "s1", Events: []Event{a}})
	mustObserve(t, hub, deviceHex, workspace.Observation{Path: "/work/app", Repo: "github.com/x/app"})

	b := sessionAt(a.End, "b", "")
	b2 := sessionAt(b.End, "b", "/work/app")
	mustIngest(t, hub, Batch{SourceID: "s1", Events: []Event{b}})
	if got := repoOf(t, hub, deviceHex, "b"); got != "" {
		t.Fatalf("repo before the session has a cwd = %q", got)
	}
	mustIngest(t, hub, Batch{SourceID: "s1", Events: []Event{b2}})
	if got := repoOf(t, hub, deviceHex, "b"); got != "github.com/x/app" {
		t.Fatalf("repo of a later session in an observed workspace = %q", got)
	}
}

func TestObserveWorkspacesRejectsInvalidInputAtomically(t *testing.T) {
	hub := ingestHub(t)
	mustRegister(t, hub, "s1", "/a.jsonl")
	mustIngest(t, hub, Batch{SourceID: "s1", Events: []Event{sessionAt(0, "a", "/work/app")}})
	good := workspace.Observation{Path: "/work/app", Repo: "github.com/x/app"}
	for _, tc := range []struct {
		name   string
		device string
		bad    workspace.Observation
	}{
		{"raw remote URL", deviceHex, workspace.Observation{Path: "/work/app", Repo: "https://token@github.com/x/app.git"}},
		{"empty path", deviceHex, workspace.Observation{Repo: "github.com/x/app"}},
		{"invalid text", deviceHex, workspace.Observation{Path: "/work/\xff"}},
		{"empty device", "", good},
	} {
		_, err := hub.ObserveWorkspaces(context.Background(), tc.device, []workspace.Observation{good, tc.bad})
		if !errors.Is(err, ErrObservationRejected) {
			t.Errorf("%s: err = %v, want ErrObservationRejected", tc.name, err)
		}
	}
	if got := repoOf(t, hub, deviceHex, "a"); got != "" {
		t.Fatalf("a rejected request changed repo to %q", got)
	}
}

func TestReDeriveKeepsObservedRepositories(t *testing.T) {
	hub, events := ingestFixtureHub(t)
	cwd := events[0].Cwd
	for _, e := range events {
		if cwd == "" {
			cwd = e.Cwd
		}
	}
	if cwd == "" {
		t.Fatal("fixture has no cwd")
	}
	res := mustObserve(t, hub, deviceHex, workspace.Observation{Path: cwd, Repo: "github.com/x/fixture"})
	if res.Applied != 1 {
		t.Fatalf("observe = %+v", res)
	}
	before := dumpHub(t, hub)
	if _, err := hub.ReDerive(context.Background(), []ParserDeriver{claudeDeriver(2, nil)}); err != nil {
		t.Fatal(err)
	}
	after := dumpHub(t, hub)
	if stripVersions(after) != stripVersions(before) {
		t.Fatalf("re-derivation changed observed repositories:\n%s", diffSnip(before, after))
	}
	if got := repoOf(t, hub, deviceHex, fixtureNative(t, hub)); got != "github.com/x/fixture" {
		t.Fatalf("repo after re-derivation = %q", got)
	}
}

func fixtureNative(t *testing.T, hub *DB) string {
	t.Helper()
	var native string
	if err := hub.Writer().QueryRow(`SELECT native_id FROM sessions WHERE cwd IS NOT NULL ORDER BY id LIMIT 1`).Scan(&native); err != nil {
		t.Fatal(err)
	}
	return native
}
