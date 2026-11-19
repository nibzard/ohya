package uploader

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nibzard/ohya/internal/claudecode"
	"github.com/nibzard/ohya/internal/codex"
	"github.com/nibzard/ohya/internal/device"
	"github.com/nibzard/ohya/internal/events"
	"github.com/nibzard/ohya/internal/hubsync"
	"github.com/nibzard/ohya/internal/registry"
	"github.com/nibzard/ohya/internal/store"
	"github.com/nibzard/ohya/internal/watch"
)

type purgeDevice struct {
	name       string
	dir        string
	stateDir   string
	cred       device.Credential
	dialer     *testDialer
	dialerAddr string
	log        *syncBuffer
	up         *Uploader
}

func newPurgeDevice(t *testing.T, name string, reg *registry.DB, hubAddr string) *purgeDevice {
	t.Helper()
	d := &purgeDevice{
		name: name, dir: t.TempDir(), stateDir: t.TempDir(),
		dialer: &testDialer{}, log: &syncBuffer{}, dialerAddr: hubAddr,
	}
	d.dialer.point(hubAddr)
	deviceID, err := device.LoadOrCreate(d.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := reg.Issue(context.Background(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	c, err := reg.Consume(context.Background(), inv.Secret, deviceID)
	if err != nil {
		t.Fatal(err)
	}
	d.cred = device.Credential{DeviceID: c.DeviceID, Secret: c.Secret}
	d.up = d.newUploader(t)
	return d
}

func (d *purgeDevice) newUploader(t *testing.T) *Uploader {
	t.Helper()
	w, err := watch.New(d.stateDir, claudecode.AgentName, watch.Options{})
	if err != nil {
		t.Fatal(err)
	}
	up, err := New(w, d.cred, d.dialer.dial, Options{
		Agent: claudecode.AgentName, Log: d.log, RequestTimeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { up.Close() })
	return up
}

func (d *purgeDevice) restart(t *testing.T) {
	t.Helper()
	d.dialer = &testDialer{}
	d.dialer.point(d.dialerAddr)
	d.log = &syncBuffer{}
	d.up = d.newUploader(t)
}

func (d *purgeDevice) mustSync(t *testing.T) Stats {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(d.dir, "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stats, err := d.up.Sync(ctx, paths)
	if err != nil {
		t.Fatalf("%s Sync: %v\nlog:\n%s", d.name, err, d.log)
	}
	return stats
}

func TestPurgeTombstonesSourceForUploaderTwoDevices(t *testing.T) {
	reg, err := registry.Open(filepath.Join(t.TempDir(), registry.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	hub := openHubDB(t, filepath.Join(t.TempDir(), "hub.db"))
	h := serve(t, hub, reg, hubsync.Options{})
	ctx := context.Background()

	alice := newPurgeDevice(t, "alice", reg, h.addr)
	bob := newPurgeDevice(t, "bob", reg, h.addr)
	aliceFile := filepath.Join(alice.dir, "a.jsonl")
	bobFile := filepath.Join(bob.dir, "b.jsonl")
	writeFile(t, aliceFile, line(sessA, "alice first")+line(sessA, "alice second"))
	writeFile(t, bobFile, line(sessB, "bob first"))
	alice.mustSync(t)
	bob.mustSync(t)
	if got := count(t, hub, `SELECT count(*) FROM events`); got != 3 {
		t.Fatalf("hub events after both devices synced = %d, want 3", got)
	}

	var aliceSource string
	if err := hub.Writer().QueryRow(
		`SELECT s.id FROM sources s
		 JOIN events e ON e.source_id = s.id
		 JOIN sessions se ON se.id = e.session_id
		 WHERE se.native_id = ? LIMIT 1`, sessA).Scan(&aliceSource); err != nil {
		t.Fatal(err)
	}

	plan, err := hub.PlanPurge(ctx, store.PurgeSelector{Session: sessA})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Sessions) != 1 || len(plan.Sources) != 1 || plan.Sources[0].ID != aliceSource {
		t.Fatalf("purge plan = %+v %+v", plan.Sessions, plan.Sources)
	}
	if got := count(t, hub, `SELECT count(*) FROM events`); got != 3 {
		t.Fatalf("the dry run changed the hub: %d events", got)
	}

	if _, _, err := hub.ApplyPurge(ctx, store.PurgeSelector{Session: sessA}); err != nil {
		t.Fatal(err)
	}
	if got := count(t, hub, `SELECT count(*) FROM events`); got != 1 {
		t.Fatalf("hub events after the purge = %d, want bob's 1", got)
	}

	appendFile(t, aliceFile, line(sessA, "alice appended after the purge"))
	appendFile(t, bobFile, line(sessB, "bob appended after the purge"))
	alice.mustSync(t)
	bob.mustSync(t)

	for _, want := range []string{
		"stops uploading",
		"was purged by the owner",
		"ohya sources reallow " + aliceSource,
	} {
		if !strings.Contains(alice.log.String(), want) {
			t.Fatalf("alice's log lacks %q:\n%s", want, alice.log)
		}
	}
	aliceSessionID := store.SessionID(alice.cred.DeviceID, "claude-code", sessA)
	if got := count(t, hub, `SELECT count(*) FROM events WHERE session_id = ?`, aliceSessionID); got != 0 {
		t.Fatalf("the purged session holds %d events after the tombstoned sync", got)
	}
	if got := count(t, hub, `SELECT count(*) FROM events`); got != 2 {
		t.Fatalf("hub events = %d, want bob's 2; the purge refusal must not wedge bob's source", got)
	}
	if err := hub.FTSIndexConsistent(ctx); err != nil {
		t.Fatalf("FTS integrity after the purge: %v", err)
	}

	if _, err := hub.ReallowSource(ctx, aliceSource); err != nil {
		t.Fatal(err)
	}
	alice.restart(t)
	alice.mustSync(t)
	if got := count(t, hub, `SELECT count(*) FROM events WHERE session_id = ?`,
		store.SessionID(alice.cred.DeviceID, "claude-code", sessA)); got != 1 {
		t.Fatalf("after reallow the purged session holds %d events, want only the 1 appended after the purge\nlogs:\n%s%s",
			got, alice.log, bob.log)
	}
	if got := count(t, hub, `SELECT count(*) FROM events`); got != 3 {
		t.Fatalf("hub events after reallow = %d, want 3", got)
	}
	if err := hub.FTSIndexConsistent(ctx); err != nil {
		t.Fatalf("FTS integrity after the resumed sync: %v", err)
	}
}

func TestPurgeReallowResumesCodexHeaderFirstSourceAndReDerives(t *testing.T) {
	reg, err := registry.Open(filepath.Join(t.TempDir(), registry.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	hub := openHubDB(t, filepath.Join(t.TempDir(), "hub.db"))
	h := serve(t, hub, reg, hubsync.Options{})
	ctx := context.Background()

	dir := t.TempDir()
	stateDir := t.TempDir()
	dialer := &testDialer{}
	dialer.point(h.addr)
	log := &syncBuffer{}
	deviceID, err := device.LoadOrCreate(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := reg.Issue(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	c, err := reg.Consume(ctx, inv.Secret, deviceID)
	if err != nil {
		t.Fatal(err)
	}
	cred := device.Credential{DeviceID: c.DeviceID, Secret: c.Secret}

	makeUp := func() *Uploader {
		w, err := watch.New(stateDir, codex.AgentName, watch.Options{})
		if err != nil {
			t.Fatal(err)
		}
		up, err := New(w, cred, dialer.dial, Options{
			Agent: codex.AgentName, Log: log, RequestTimeout: 10 * time.Second,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { up.Close() })
		return up
	}

	const threadID = "00000000-0000-7000-8000-0000000000c1"
	rollout := filepath.Join(dir, "rollout-"+threadID+".jsonl")
	metaLine := `{"timestamp":"2026-11-19T10:00:00Z","type":"session_meta","payload":{"id":"` + threadID + `","cwd":"/repo"}}` + "\n"
	msg1 := `{"timestamp":"2026-11-19T10:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"before purge"}]}}` + "\n"
	writeFile(t, rollout, metaLine+msg1)

	up := makeUp()
	if _, err := up.Sync(ctx, []string{rollout}); err != nil {
		t.Fatalf("initial codex sync: %v\n%s", err, log)
	}
	if got := count(t, hub, `SELECT count(*) FROM events`); got != 2 {
		t.Fatalf("initial codex events = %d, want 2", got)
	}

	var srcID string
	if err := hub.Writer().QueryRow(`SELECT id FROM sources WHERE agent = 'codex'`).Scan(&srcID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := hub.ApplyPurge(ctx, store.PurgeSelector{Session: threadID}); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.ReallowSource(ctx, srcID); err != nil {
		t.Fatal(err)
	}

	msg2 := `{"timestamp":"2026-11-19T10:05:00Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"after reallow codex"}]}}` + "\n"
	appendFile(t, rollout, msg2)

	up2 := makeUp()
	if _, err := up2.Sync(ctx, []string{rollout}); err != nil {
		t.Fatalf("post-reallow codex sync: %v\n%s", err, log)
	}
	if got := count(t, hub, `SELECT count(*) FROM events`); got != 1 {
		t.Fatalf("post-reallow codex events = %d, want 1\nlog:\n%s", got, log)
	}
	if _, err := hub.Writer().Exec(`UPDATE events SET parser_version = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := hub.ReDerive(ctx, events.Derivers()); err != nil {
		t.Fatalf("ReDerive after codex purge+reallow: %v", err)
	}
	if err := hub.FTSIndexConsistent(ctx); err != nil {
		t.Fatalf("FTS integrity after codex purge+reallow+rederive: %v", err)
	}
}

func TestBlockedPurgedMissingSourceSkipsProbe(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, Options{})
	hub := openHubDB(t, filepath.Join(t.TempDir(), "hub.db"))
	h := serve(t, hub, f.reg, hubsync.Options{})
	f.dialer.point(h.addr)

	a := filepath.Join(f.dir, "a.jsonl")
	writeFile(t, a, line(sessA, "before purge"))
	f.mustSync()

	if _, _, err := hub.ApplyPurge(ctx, store.PurgeSelector{Session: sessA}); err != nil {
		t.Fatal(err)
	}
	appendFile(t, a, line(sessA, "after purge"))
	f.mustSync()
	if err := os.Remove(a); err != nil {
		t.Fatal(err)
	}
	beforeLog := f.log.String()
	f.mustSync()
	afterLog := strings.TrimPrefix(f.log.String(), beforeLog)
	if strings.Contains(afterLog, "stops uploading") {
		t.Fatalf("missing blocked purged source re-probed and logged refusal again:\n%s", afterLog)
	}
}
