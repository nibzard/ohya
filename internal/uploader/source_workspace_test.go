package uploader

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/nibzard/ohya/internal/hubsync"
)

func workspaceAt(path string) func(string) (string, bool) {
	return func(string) (string, bool) { return path, true }
}

func TestSourceWorkspacePrecedesTheFirstBatch(t *testing.T) {
	lookups := 0
	f := newFixture(t, Options{SourceWorkspace: func(string) (string, bool) {
		lookups++
		return "/work/agy", true
	}})
	old := serveOldHub(t, hubsync.CapSourceWorkspace)
	f.dialer.point(old.ln.Addr().String())
	a := filepath.Join(f.dir, "a.jsonl")
	writeFile(t, a, line(sessA, "one"))

	if stats := f.mustSync(); stats.Inserted != 1 {
		t.Fatalf("stats = %+v; want 1 event", stats)
	}
	if got, want := old.markers(), "VRFBBW"; got != want {
		t.Fatalf("frames = %s; want %s, the workspace between the registration and the probe", got, want)
	}

	old.kick()
	appendFile(t, a, line(sessA, "two"))
	f.mustSync()
	if got, want := old.markers(), "VRFBBWVBB"; got != want {
		t.Fatalf("frames after a reconnect = %s; want %s: the hub knows the source, so it gets no registration and no second workspace frame", got, want)
	}
	if lookups != 1 {
		t.Fatalf("lookups = %d, want 1: a source the hub stored is not looked up again in this process", lookups)
	}
}

func TestSourceWorkspaceFollowsTheProbeOfAKnownSource(t *testing.T) {
	found := false
	f := newFixture(t, Options{SourceWorkspace: func(string) (string, bool) {
		return "/work/agy", found
	}})
	old := serveOldHub(t, hubsync.CapSourceWorkspace)
	f.dialer.point(old.ln.Addr().String())
	a := filepath.Join(f.dir, "a.jsonl")
	writeFile(t, a, line(sessA, "one"))

	f.mustSync()
	if got := old.markers(); strings.Contains(got, "F") {
		t.Fatalf("frames = %s; the index has no workspace for the source yet", got)
	}

	found = true
	old.kick()
	appendFile(t, a, line(sessA, "two"))
	f.mustSync()
	if got, want := old.markers(), "VRBBWVBFB"; got != want {
		t.Fatalf("frames = %s; want %s: the workspace follows the probe of the source that the hub knows, and comes before the batch", got, want)
	}

	appendFile(t, a, line(sessA, "three"))
	f.mustSync()
	if got, want := old.markers(), "VRBBWVBFBB"; got != want {
		t.Fatalf("frames = %s; want %s: a source that the hub stored gets no second frame", got, want)
	}
}

func TestSourceWorkspaceNotFoundIsLookedUpAgainOnANewConnection(t *testing.T) {
	lookups := 0
	f := newFixture(t, Options{SourceWorkspace: func(string) (string, bool) {
		lookups++
		return "", false
	}})
	old := serveOldHub(t, hubsync.CapSourceWorkspace)
	f.dialer.point(old.ln.Addr().String())
	a := filepath.Join(f.dir, "a.jsonl")
	writeFile(t, a, line(sessA, "one"))

	f.mustSync()
	appendFile(t, a, line(sessA, "two"))
	f.mustSync()
	f.mustSync()
	if lookups != 1 {
		t.Fatalf("lookups on one connection = %d, want 1", lookups)
	}
	if strings.Contains(old.markers(), "F") {
		t.Fatalf("frames = %s; a source without a workspace sends no frame", old.markers())
	}
	old.kick()
	f.mustSync()
	if lookups != 2 {
		t.Fatalf("lookups after a reconnect = %d, want 2", lookups)
	}
	if strings.Contains(f.log.String(), "source workspace") {
		t.Fatalf("a source without a workspace is not reported:\n%s", f.log)
	}
}

func TestHubWithoutTheSourceWorkspaceCapabilityGetsNoFrame(t *testing.T) {
	f := newFixture(t, Options{SourceWorkspace: workspaceAt("/work/agy")})
	old := serveOldHub(t, hubsync.CapZstdBatches)
	f.dialer.point(old.ln.Addr().String())
	a := filepath.Join(f.dir, "a.jsonl")
	writeFile(t, a, line(sessA, "one"))

	f.mustSync()
	if strings.Contains(old.markers(), "F") {
		t.Fatalf("frames = %s; a hub that offers no source workspaces gets no frame", old.markers())
	}

	old.kick()
	appendFile(t, a, line(sessA, "two"))
	f.mustSync()
	if got := old.plain.Load(); got != 2 {
		t.Fatalf("plain data batches = %d, want 2: the batches continue without the workspace", got)
	}
	if n := strings.Count(f.log.String(), "does not accept source workspaces"); n != 1 {
		t.Fatalf("the hub without the capability is reported %d times, want once:\n%s", n, f.log)
	}
}

func TestHubWithoutNegotiationGetsNoSourceWorkspace(t *testing.T) {
	f := newFixture(t, Options{SourceWorkspace: workspaceAt("/work/agy")})
	old := serveOldHub(t, 0)
	f.dialer.point(old.ln.Addr().String())
	writeFile(t, filepath.Join(f.dir, "a.jsonl"), line(sessA, "one"))

	if stats := f.mustSync(); stats.Inserted != 1 {
		t.Fatalf("stats = %+v; want 1 event", stats)
	}
	if strings.Contains(old.markers(), "F") {
		t.Fatalf("frames = %s; a hub that refuses the negotiation gets no workspace frame", old.markers())
	}
	if n := strings.Count(f.log.String(), "does not accept source workspaces"); n != 1 {
		t.Fatalf("the older hub is reported %d times, want once:\n%s", n, f.log)
	}
}

func TestSourceWorkspaceRefusalDoesNotBlockTheSource(t *testing.T) {
	f := newFixture(t, Options{SourceWorkspace: workspaceAt("/work/agy")})
	old := serveOldHub(t, hubsync.CapSourceWorkspace)
	old.refuseWorkspace.Store(true)
	f.dialer.point(old.ln.Addr().String())
	a := filepath.Join(f.dir, "a.jsonl")
	writeFile(t, a, line(sessA, "one"))

	if stats := f.mustSync(); stats.Inserted != 1 {
		t.Fatalf("stats = %+v; a refused workspace must not block the batch", stats)
	}
	f.mustSync()
	if got, want := old.markers(), "VRFBBW"; got != want {
		t.Fatalf("frames = %s; want %s: the refused frame is not repeated on one connection", got, want)
	}

	old.kick()
	appendFile(t, a, line(sessA, "two"))
	f.mustSync()
	if got, want := old.markers(), "VRFBBWVBFB"; got != want {
		t.Fatalf("frames after a reconnect = %s; want %s: the frame is sent again, after the probe of the source that the hub knows", got, want)
	}
	if n := strings.Count(f.log.String(), "did not store the working directory"); n != 2 {
		t.Fatalf("refusals reported = %d, want 2, one for each connection:\n%s", n, f.log)
	}
}

func TestSourceWorkspaceTheWireRefusesStaysLocal(t *testing.T) {
	f := newFixture(t, Options{SourceWorkspace: workspaceAt("/" + strings.Repeat("p", 5000))})
	h := serve(t, openHubDB(t, filepath.Join(t.TempDir(), "hub.db")), f.reg, hubsync.Options{})
	f.dialer.point(h.addr)
	a := filepath.Join(f.dir, "a.jsonl")
	writeFile(t, a, line(sessA, "one"))

	if stats := f.mustSync(); stats.Inserted != 1 {
		t.Fatalf("stats = %+v; want 1 event on the same connection", stats)
	}
	if n := count(t, h.db, `SELECT count(*) FROM sources WHERE workspace_path IS NOT NULL`); n != 0 {
		t.Fatalf("sources with a workspace = %d, want 0", n)
	}
	if n := strings.Count(f.log.String(), "the wire does not carry"); n != 1 {
		t.Fatalf("the refused path is reported %d times, want once:\n%s", n, f.log)
	}
}

func TestCompressionOffKeepsTheSourceWorkspaceOffer(t *testing.T) {
	t.Setenv("OHYA_UPLOAD_COMPRESSION", "off")
	f := newFixture(t, Options{SourceWorkspace: workspaceAt("/work/agy")})
	old := serveOldHub(t, hubsync.CapZstdBatches|hubsync.CapSourceWorkspace)
	f.dialer.point(old.ln.Addr().String())
	a := filepath.Join(f.dir, "a.jsonl")
	writeFile(t, a, strings.Repeat(line(sessA, "one"), 200))

	if stats := f.mustSync(); stats.Inserted != 200 {
		t.Fatalf("stats = %+v; want 200 events", stats)
	}
	if got, want := hubsync.Capabilities(old.offered.Load()), hubsync.CapSourceWorkspace; got != want {
		t.Fatalf("offer = %#x; want %#x: only the zstd bit leaves the offer", uint16(got), uint16(want))
	}
	if got, want := old.markers(), "VRFBBW"; got != want {
		t.Fatalf("frames = %s; want %s", got, want)
	}
	if got := old.compressed.Load(); got != 0 {
		t.Fatalf("compressed batches = %d, want none", got)
	}
	wire := f.obs.Counter("upload_wire_bytes")
	payload := f.obs.Counter("upload_payload_bytes")
	if wire < payload {
		t.Fatalf("wire bytes = %d, payload bytes = %d; want plain frames", wire, payload)
	}
}

func TestHubResolvesTheRepositoryOfASourceWorkspace(t *testing.T) {
	dir := gitRepo(t, "https://github.com/nibzard/antigravity-app.git")
	f := newFixture(t, Options{SourceWorkspace: workspaceAt(dir)})
	h := serve(t, openHubDB(t, filepath.Join(t.TempDir(), "hub.db")), f.reg, hubsync.Options{})
	f.dialer.point(h.addr)
	writeFile(t, filepath.Join(f.dir, "a.jsonl"), line(sessA, "one"))

	f.mustSync()
	var path string
	if err := h.db.Writer().QueryRow(`SELECT workspace_path FROM sources`).Scan(&path); err != nil || path != dir {
		t.Fatalf("source workspace = %q, %v; want %q", path, err, dir)
	}
	if n := count(t, h.db, `SELECT count(*) FROM sessions WHERE cwd = ? AND repo = 'github.com/nibzard/antigravity-app'`, dir); n != 1 {
		t.Fatalf("sessions with the source workspace and its repository = %d, want 1\nlog:\n%s", n, f.log)
	}
}
