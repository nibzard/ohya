package uploader

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nibzard/ohya/internal/claudecode"
	"github.com/nibzard/ohya/internal/device"
	"github.com/nibzard/ohya/internal/hubsync"
	"github.com/nibzard/ohya/internal/observe/observertest"
	"github.com/nibzard/ohya/internal/registry"
	"github.com/nibzard/ohya/internal/store"
	"github.com/nibzard/ohya/internal/watch"
)

type testHub struct {
	t      *testing.T
	db     *store.DB
	addr   string
	cancel context.CancelFunc
	done   chan error
	once   sync.Once
}

func (h *testHub) stop() {
	h.cancel()
	h.once.Do(func() {
		select {
		case err := <-h.done:
			if err != nil {
				h.t.Errorf("serve: %v", err)
			}
		case <-time.After(10 * time.Second):
			h.t.Error("serve did not stop within 10 s")
		}
	})
}

func openHubDB(t *testing.T, path string) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func serve(t *testing.T, db *store.DB, reg *registry.DB, opts hubsync.Options) *testHub {
	t.Helper()
	opts.Retry = hubsync.RetryPolicy{Base: time.Millisecond}
	ep, err := hubsync.NewEndpoint(db, &registry.Authenticator{DB: reg, PollInterval: 20 * time.Millisecond}, opts)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &testHub{t: t, db: db, addr: ln.Addr().String(), cancel: cancel, done: make(chan error, 1)}
	go func() { h.done <- ep.Serve(ctx, ln) }()
	t.Cleanup(h.stop)
	return h
}

type fault int32

const (
	noFault fault = iota
	loseAck
	stallAck
	closedHub
)

type testDialer struct {
	addr    atomic.Pointer[string]
	fault   atomic.Int32
	batches atomic.Int32
}

func (d *testDialer) point(addr string) { d.addr.Store(&addr) }

func (d *testDialer) dial(ctx context.Context) (net.Conn, error) {
	if fault(d.fault.Load()) == closedHub {
		return nil, errors.New("injected: hub unreachable")
	}
	var nd net.Dialer
	c, err := nd.DialContext(ctx, "tcp", *d.addr.Load())
	if err != nil {
		return nil, err
	}
	return &faultConn{Conn: c, d: d, frameStart: true}, nil
}

type faultConn struct {
	net.Conn
	d          *testDialer
	frameStart bool
	dataBatch  bool
}

func (c *faultConn) Write(p []byte) (int, error) {
	if c.frameStart && len(p) > 0 {
		c.frameStart = false
		switch p[0] {
		case 'Z':
			c.dataBatch = true
			c.d.batches.Add(1)
		case 'B':
			c.dataBatch = bytes.IndexByte(p, '{') >= 0
			if c.dataBatch {
				c.d.batches.Add(1)
			}
		}
	}
	return c.Conn.Write(p)
}

func (c *faultConn) Read(p []byte) (int, error) {
	c.frameStart = true
	if !c.dataBatch {
		return c.Conn.Read(p)
	}
	c.dataBatch = false
	switch fault(c.d.fault.Load()) {
	case loseAck:
		c.d.fault.Store(int32(noFault))
		if _, err := c.Conn.Read(p); err != nil {
			return 0, err
		}
		c.Conn.Close()
		return 0, errors.New("injected: acknowledgment lost")
	case stallAck:
		c.d.fault.Store(int32(noFault))
		if _, err := c.Conn.Read(p); err != nil {
			return 0, err
		}
		return c.Conn.Read(p)
	}
	return c.Conn.Read(p)
}

type fixture struct {
	t        *testing.T
	dir      string
	stateDir string
	reg      *registry.DB
	cred     device.Credential
	dialer   *testDialer
	log      *syncBuffer
	up       *Uploader
	obs      *observertest.Recorder
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newFixture(t *testing.T, opts Options) *fixture {
	t.Helper()
	reg, err := registry.Open(filepath.Join(t.TempDir(), registry.FileName))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	f := &fixture{t: t, dir: t.TempDir(), stateDir: t.TempDir(), reg: reg, dialer: &testDialer{}, log: &syncBuffer{}, obs: observertest.New()}
	deviceID, err := device.LoadOrCreate(f.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	inv, err := reg.Issue(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	c, err := reg.Consume(ctx, inv.Secret, deviceID)
	if err != nil {
		t.Fatal(err)
	}
	f.cred = device.Credential{DeviceID: c.DeviceID, Secret: c.Secret}
	f.dialer.point("127.0.0.1:1")
	f.up = f.newUploader(opts)
	return f
}

func (f *fixture) newUploader(opts Options) *Uploader {
	f.t.Helper()
	w, err := watch.New(f.stateDir, claudecode.AgentName, watch.Options{Observer: f.obs})
	if err != nil {
		f.t.Fatal(err)
	}
	opts.Agent = claudecode.AgentName
	opts.Log = f.log
	opts.Observer = f.obs
	if opts.RequestTimeout == 0 {
		opts.RequestTimeout = 10 * time.Second
	}
	up, err := New(w, f.cred, f.dialer.dial, opts)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { up.Close() })
	return up
}

func (f *fixture) candidates() []string {
	f.t.Helper()
	paths, err := filepath.Glob(filepath.Join(f.dir, "*.jsonl"))
	if err != nil {
		f.t.Fatal(err)
	}
	return paths
}

func (f *fixture) sync() (Stats, error) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return f.up.Sync(ctx, f.candidates())
}

func (f *fixture) mustSync() Stats {
	f.t.Helper()
	stats, err := f.sync()
	if err != nil {
		f.t.Fatalf("Sync: %v\nlog:\n%s", err, f.log)
	}
	return stats
}

func (f *fixture) wantDisconnected() {
	f.t.Helper()
	if _, err := f.sync(); !errors.Is(err, ErrDisconnected) {
		f.t.Fatalf("Sync error = %v; want ErrDisconnected", err)
	}
}

func (f *fixture) checkpoint(path string) int64 {
	f.t.Helper()
	w, err := watch.New(f.stateDir, claudecode.AgentName, watch.Options{})
	if err != nil {
		f.t.Fatal(err)
	}
	outcomes, err := w.Poll([]string{path})
	if err != nil {
		f.t.Fatal(err)
	}
	for _, o := range outcomes {
		if o.Path == path || filepath.Base(o.Path) == filepath.Base(path) {
			return o.Cursor
		}
	}
	f.t.Fatalf("no checkpoint for %s", path)
	return 0
}

func line(session, content string) string {
	return fmt.Sprintf(`{"type":"user","sessionId":%q,"message":{"role":"user","content":%q}}`+"\n", session, content)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	fh, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	if _, err := fh.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, db *store.DB, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := db.Writer().QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func size(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

const (
	sessA = "00000000-0000-4000-8000-0000000000a1"
	sessB = "00000000-0000-4000-8000-0000000000b1"
)

func TestOfflineCatchUp(t *testing.T) {
	f := newFixture(t, Options{})
	a := filepath.Join(f.dir, "a.jsonl")
	b := filepath.Join(f.dir, "b.jsonl")
	writeFile(t, a, line(sessA, "one")+line(sessA, "two"))
	writeFile(t, b, line(sessB, "three"))

	f.dialer.fault.Store(int32(closedHub))
	f.wantDisconnected()
	if got := f.obs.Counter("upload_failures", "connect"); got < 1 {
		t.Errorf("connect failures = %d, want at least 1 while the hub is unreachable", got)
	}
	if got := f.checkpoint(a); got != 0 {
		t.Fatalf("checkpoint while offline = %d; want 0", got)
	}

	f.dialer.fault.Store(int32(noFault))
	h := serve(t, openHubDB(t, filepath.Join(t.TempDir(), "hub.db")), f.reg, hubsync.Options{})
	f.dialer.point(h.addr)
	stats := f.mustSync()
	if stats.Inserted != 3 || stats.Sources != 2 {
		t.Fatalf("stats = %+v; want 3 events from 2 sources", stats)
	}
	if got := count(t, h.db, `SELECT count(*) FROM events`); got != 3 {
		t.Fatalf("hub events = %d; want 3", got)
	}
	for _, p := range []string{a, b} {
		if got, want := f.checkpoint(p), size(t, p); got != want {
			t.Fatalf("checkpoint %s = %d; want %d", p, got, want)
		}
	}
	if !strings.Contains(f.log.String(), "reconnected") {
		t.Fatalf("log lacks the reconnect line:\n%s", f.log)
	}

	appendFile(t, a, line(sessA, "four"))
	if stats := f.mustSync(); stats.Inserted != 1 {
		t.Fatalf("incremental stats = %+v; want 1 event", stats)
	}
	if got, want := f.checkpoint(a), size(t, a); got != want {
		t.Fatalf("checkpoint after append = %d; want %d", got, want)
	}
}

func TestLostAcknowledgment(t *testing.T) {
	f := newFixture(t, Options{})
	h := serve(t, openHubDB(t, filepath.Join(t.TempDir(), "hub.db")), f.reg, hubsync.Options{})
	f.dialer.point(h.addr)
	a := filepath.Join(f.dir, "a.jsonl")
	writeFile(t, a, line(sessA, "one")+line(sessA, "two"))

	f.dialer.fault.Store(int32(loseAck))
	f.wantDisconnected()
	if got := count(t, h.db, `SELECT count(*) FROM events`); got != 2 {
		t.Fatalf("hub events = %d; want the committed 2", got)
	}
	if got := f.checkpoint(a); got != 0 {
		t.Fatalf("checkpoint after a lost ack = %d; want 0: success was inferred from sending", got)
	}

	appendFile(t, a, line(sessA, "three"))
	stats := f.mustSync()
	if stats.Inserted != 1 {
		t.Fatalf("stats = %+v; want only the new event inserted", stats)
	}
	if got := count(t, h.db, `SELECT count(*) FROM events`); got != 3 {
		t.Fatalf("hub events = %d; want 3 with no duplicates", got)
	}
	if got := count(t, h.db, `SELECT event_count FROM sessions`); got != 3 {
		t.Fatalf("session event_count = %d; want 3", got)
	}
	if got, want := f.checkpoint(a), size(t, a); got != want {
		t.Fatalf("checkpoint = %d; want %d", got, want)
	}
	if got := f.dialer.batches.Load(); got != 2 {
		t.Fatalf("data batches sent = %d; want 2 (the lost one, then only the new bytes)", got)
	}

	one, two, three := line(sessA, "one"), line(sessA, "two"), line(sessA, "three")
	if got := f.obs.Counter("upload_payload_bytes"); got != uint64(len(one)+len(two)+len(three)) {
		t.Errorf("upload_payload_bytes = %d, want %d: attempted bytes cover both sends", got, len(one)+len(two)+len(three))
	}
	if got := f.obs.Counter("upload_events_inserted"); got != 1 {
		t.Errorf("upload_events_inserted = %d, want 1: attempted bytes grew while newly reported events did not duplicate", got)
	}
	if got := f.obs.Counter("upload_ack_bytes"); got != uint64(len(one)+len(two)+len(three)) {
		t.Errorf("upload_ack_bytes = %d, want the full file %d", got, len(one)+len(two)+len(three))
	}
	if got := f.obs.Counter("upload_checkpoint_advances"); got != 2 {
		t.Errorf("upload_checkpoint_advances = %d, want 2 (probe reconcile, final acknowledgment)", got)
	}
	if got := f.obs.Counter("upload_batches", "attempted"); got != 2 {
		t.Errorf("attempted batches = %d, want 2", got)
	}
	if got := f.obs.Counter("upload_batches", "successful"); got != 1 {
		t.Errorf("successful batches = %d, want 1: the lost batch is attempted but never acknowledged", got)
	}
	if got := f.obs.UnknownAddCalls(); got != 0 {
		t.Errorf("%d Add calls used unknown metric names or label arity", got)
	}
}

func TestTimeoutRetries(t *testing.T) {
	f := newFixture(t, Options{RequestTimeout: 2 * time.Second})
	h := serve(t, openHubDB(t, filepath.Join(t.TempDir(), "hub.db")), f.reg, hubsync.Options{})
	f.dialer.point(h.addr)
	a := filepath.Join(f.dir, "a.jsonl")
	writeFile(t, a, line(sessA, "one"))

	f.dialer.fault.Store(int32(stallAck))
	_, err := f.sync()
	if !errors.Is(err, ErrDisconnected) || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("Sync error = %v; want a timeout wrapping ErrDisconnected", err)
	}
	if got := f.dialer.batches.Load(); got != 1 {
		t.Fatalf("data batches before the timeout = %d; want 1: the stalled acknowledgment, not an earlier round trip, must consume the fault\nlog:\n%s", got, f.log)
	}
	if got := f.checkpoint(a); got != 0 {
		t.Fatalf("checkpoint after a timeout = %d; want 0", got)
	}
	f.mustSync()
	if got, want := f.checkpoint(a), size(t, a); got != want {
		t.Fatalf("checkpoint = %d; want %d", got, want)
	}
	if got := count(t, h.db, `SELECT count(*) FROM events`); got != 1 {
		t.Fatalf("hub events = %d; want 1", got)
	}
	if got := f.obs.Counter("upload_failures", "read"); got != 1 {
		t.Errorf("read failures = %d, want 1 from the stalled acknowledgment", got)
	}
	if got := f.obs.DurationCount("upload_round_trip_duration"); got < 2 {
		t.Errorf("round trips observed = %d, want at least the failed batch and the recovery probe", got)
	}
	if got := f.obs.Counter("upload_events_inserted"); got != 0 {
		t.Errorf("upload_events_inserted = %d, want 0: the lost acknowledgment reported no insertions", got)
	}
	if got := f.obs.Counter("upload_checkpoint_advances"); got != 1 {
		t.Errorf("upload_checkpoint_advances = %d, want 1 from the recovery probe", got)
	}
}

func TestReconnectAfterHubRestart(t *testing.T) {
	f := newFixture(t, Options{})
	dbPath := filepath.Join(t.TempDir(), "hub.db")
	db := openHubDB(t, dbPath)
	h := serve(t, db, f.reg, hubsync.Options{})
	f.dialer.point(h.addr)
	a := filepath.Join(f.dir, "a.jsonl")
	writeFile(t, a, line(sessA, "one"))
	f.mustSync()

	h.stop()
	appendFile(t, a, line(sessA, "two"))
	f.wantDisconnected()
	if got := f.checkpoint(a); got != int64(len(line(sessA, "one"))) {
		t.Fatalf("checkpoint while the hub is down = %d; want the first record's end", got)
	}

	h2 := serve(t, db, f.reg, hubsync.Options{})
	f.dialer.point(h2.addr)
	if stats := f.mustSync(); stats.Inserted != 1 {
		t.Fatalf("stats = %+v; want the one pending event", stats)
	}
	if got := count(t, db, `SELECT count(*) FROM events`); got != 2 {
		t.Fatalf("hub events = %d; want 2", got)
	}
	if got := f.obs.Counter("upload_events_inserted"); got != 2 {
		t.Errorf("upload_events_inserted = %d, want 2 across the restart with no duplicates", got)
	}
	if got, want := f.obs.Counter("upload_ack_bytes"), size(t, a); got != uint64(want) {
		t.Errorf("upload_ack_bytes = %d, want the full file %d", got, want)
	}
}

func TestBusyHubRetries(t *testing.T) {
	f := newFixture(t, Options{})
	h := serve(t, openHubDB(t, filepath.Join(t.TempDir(), "hub.db")), f.reg, hubsync.Options{MaxConns: 1})
	f.dialer.point(h.addr)
	a := filepath.Join(f.dir, "a.jsonl")
	writeFile(t, a, line(sessA, "one"))

	holder, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	_, err = f.sync()
	if !errors.Is(err, ErrDisconnected) || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("Sync error = %v; want busy wrapping ErrDisconnected", err)
	}
	if got := f.obs.Counter("upload_failures", "retry"); got != 1 {
		t.Errorf("busy refusals counted as retry failures = %d, want 1", got)
	}
	holder.Close()

	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := f.sync()
		if err == nil {
			break
		}
		if !errors.Is(err, ErrDisconnected) || time.Now().After(deadline) {
			t.Fatalf("Sync error = %v; want success once the slot frees", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got, want := f.checkpoint(a), size(t, a); got != want {
		t.Fatalf("checkpoint = %d; want %d", got, want)
	}
}

func TestRunRetriesUntilHubAppears(t *testing.T) {
	f := newFixture(t, Options{})
	f.up = f.newUploader(Options{
		Interval:  10 * time.Millisecond,
		Reconnect: hubsync.RetryPolicy{Base: 5 * time.Millisecond, Cap: 20 * time.Millisecond},
	})
	a := filepath.Join(f.dir, "a.jsonl")
	writeFile(t, a, line(sessA, "one")+line(sessA, "two"))
	f.dialer.fault.Store(int32(closedHub))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- f.up.Run(ctx, func() ([]string, error) {
			return filepath.Glob(filepath.Join(f.dir, "*.jsonl"))
		})
	}()
	time.Sleep(100 * time.Millisecond)
	db := openHubDB(t, filepath.Join(t.TempDir(), "hub.db"))
	h := serve(t, db, f.reg, hubsync.Options{})
	f.dialer.point(h.addr)
	f.dialer.fault.Store(int32(noFault))

	deadline := time.Now().Add(10 * time.Second)
	for count(t, db, `SELECT count(*) FROM events`) != 2 {
		if time.Now().After(deadline) {
			t.Fatalf("hub never caught up; log:\n%s", f.log)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v; want nil after cancel", err)
	}
	if n := strings.Count(f.log.String(), "retrying with backoff"); n != 1 {
		t.Fatalf("outage reported %d times; want once:\n%s", n, f.log)
	}
}

func TestHubRestoreReplaysRetainedFiles(t *testing.T) {
	f := newFixture(t, Options{})
	dbPath := filepath.Join(t.TempDir(), "hub.db")
	db := openHubDB(t, dbPath)
	h := serve(t, db, f.reg, hubsync.Options{})
	f.dialer.point(h.addr)

	a := filepath.Join(f.dir, "a.jsonl")
	b := filepath.Join(f.dir, "b.jsonl")
	writeFile(t, a, line(sessA, "one"))
	writeFile(t, b, line(sessB, "two"))
	f.mustSync()

	backup := filepath.Join(t.TempDir(), "backup.db")
	if _, err := db.Writer().Exec(`VACUUM INTO ?`, backup); err != nil {
		t.Fatal(err)
	}

	c := filepath.Join(f.dir, "c.jsonl")
	writeFile(t, c, line(sessA+"c", "three"))
	appendFile(t, a, line(sessA, "four"))
	appendFile(t, b, line(sessB, "five"))
	f.mustSync()
	if err := os.Remove(b); err != nil {
		t.Fatal(err)
	}

	h.stop()
	restored := openHubDB(t, backup)
	h2 := serve(t, restored, f.reg, hubsync.Options{})
	f.dialer.point(h2.addr)
	stats := f.mustSync()
	if stats.Inserted != 2 {
		t.Fatalf("stats = %+v; want 2 replayed events (a's second record, c's record)", stats)
	}
	if got := count(t, restored, `SELECT count(*) FROM events`); got != 4 {
		t.Fatalf("restored hub events = %d; want 4 (a: 2, b: 1 from the backup, c: 1)", got)
	}
	for _, p := range []string{a, c} {
		if got, want := f.checkpoint(p), size(t, p); got != want {
			t.Fatalf("checkpoint %s = %d; want %d", p, got, want)
		}
	}
	log := f.log.String()
	if !strings.Contains(log, "regressed") || !strings.Contains(log, "replaying the retained file") {
		t.Fatalf("log lacks the regression report:\n%s", log)
	}
	gap := fmt.Sprintf("unrecoverable gap: the hub lost bytes [%d, %d)", len(line(sessB, "two")), len(line(sessB, "two"))+len(line(sessB, "five")))
	if !strings.Contains(log, gap) {
		t.Fatalf("log lacks %q:\n%s", gap, log)
	}

	f.mustSync()
	if n := strings.Count(f.log.String(), "unrecoverable gap"); n != 1 {
		t.Fatalf("gap reported %d times; want once", n)
	}
}

func TestReplacementBeforeUploadReported(t *testing.T) {
	f := newFixture(t, Options{})
	db := openHubDB(t, filepath.Join(t.TempDir(), "hub.db"))
	h := serve(t, db, f.reg, hubsync.Options{})
	f.dialer.point(h.addr)
	a := filepath.Join(f.dir, "a.jsonl")
	writeFile(t, a, line(sessA, "uploaded"))
	f.mustSync()

	h.stop()
	lost := line(sessA, "never-uploaded")
	appendFile(t, a, lost)
	f.wantDisconnected()

	tmp := a + ".tmp"
	writeFile(t, tmp, line(sessB, "replacement"))
	if err := os.Rename(tmp, a); err != nil {
		t.Fatal(err)
	}
	h2 := serve(t, db, f.reg, hubsync.Options{})
	f.dialer.point(h2.addr)
	f.mustSync()

	want := fmt.Sprintf("%d bytes after acknowledged offset %d were never uploaded and are lost", len(lost), len(line(sessA, "uploaded")))
	if !strings.Contains(f.log.String(), want) {
		t.Fatalf("log lacks %q:\n%s", want, f.log)
	}
	if got := count(t, db, `SELECT count(*) FROM events WHERE raw_json LIKE '%never-uploaded%'`); got != 0 {
		t.Fatalf("lost record reached the hub %d times", got)
	}
	if got := count(t, db, `SELECT count(*) FROM sources`); got != 2 {
		t.Fatalf("hub sources = %d; want the old and the new incarnation", got)
	}
	if got := count(t, db, `SELECT count(*) FROM events`); got != 2 {
		t.Fatalf("hub events = %d; want 2", got)
	}
}

func TestDeletionBeforeUploadReported(t *testing.T) {
	f := newFixture(t, Options{})
	h := serve(t, openHubDB(t, filepath.Join(t.TempDir(), "hub.db")), f.reg, hubsync.Options{})
	f.dialer.point(h.addr)
	a := filepath.Join(f.dir, "a.jsonl")
	writeFile(t, a, line(sessA, "one"))
	f.dialer.fault.Store(int32(closedHub))
	f.wantDisconnected()
	if err := os.Remove(a); err != nil {
		t.Fatal(err)
	}
	f.dialer.fault.Store(int32(noFault))
	f.mustSync()
	f.mustSync()
	want := fmt.Sprintf("disappeared before upload: bytes [0, %d) were never uploaded", len(line(sessA, "one")))
	if n := strings.Count(f.log.String(), want); n != 1 {
		t.Fatalf("log reports %q %d times; want once:\n%s", want, n, f.log)
	}
}

func TestAcknowledgedSourceDisappearanceReported(t *testing.T) {
	f := newFixture(t, Options{})
	h := serve(t, openHubDB(t, filepath.Join(t.TempDir(), "hub.db")), f.reg, hubsync.Options{})
	f.dialer.point(h.addr)
	a := filepath.Join(f.dir, "a.jsonl")
	writeFile(t, a, line(sessA, "one"))
	f.mustSync()
	f.mustSync()
	if got := f.checkpoint(a); got != int64(len(line(sessA, "one"))) {
		t.Fatalf("checkpoint = %d, want the uploaded bytes acknowledged", got)
	}
	if err := os.Remove(a); err != nil {
		t.Fatal(err)
	}
	f.mustSync()
	f.mustSync()
	want := fmt.Sprintf("%s: source", a)
	for _, phrase := range []string{want, "disappeared; the checkpoint is kept and upload resumes if the file returns"} {
		if n := strings.Count(f.log.String(), phrase); n != 1 {
			t.Fatalf("log reports %q %d times, want once:\n%s", phrase, n, f.log)
		}
	}
	if strings.Contains(f.log.String(), "never uploaded") {
		t.Fatalf("an acknowledged source was reported as lost:\n%s", f.log)
	}
}

func TestObserverCountsCheckpointPersistFailure(t *testing.T) {
	f := newFixture(t, Options{})
	h := serve(t, openHubDB(t, filepath.Join(t.TempDir(), "hub.db")), f.reg, hubsync.Options{})
	f.dialer.point(h.addr)
	a := filepath.Join(f.dir, "a.jsonl")
	writeFile(t, a, line(sessA, "one"))
	f.mustSync()

	statePath := filepath.Join(f.stateDir, "sources", claudecode.AgentName+".json")
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(statePath, 0o700); err != nil {
		t.Fatal(err)
	}
	appendFile(t, a, line(sessA, "two"))
	if _, err := f.sync(); err == nil {
		t.Fatal("Sync must fail when the checkpoint cannot persist")
	}
	if got := f.obs.Counter("upload_failures", "checkpoint"); got != 1 {
		t.Errorf("checkpoint failures = %d, want 1 from the failed persist", got)
	}
	if got := f.obs.Counter("upload_checkpoint_advances"); got != 1 {
		t.Errorf("upload_checkpoint_advances = %d, want 1: only the first pass persisted", got)
	}
	if got := count(t, h.db, `SELECT count(*) FROM events`); got != 2 {
		t.Fatalf("hub events = %d, want 2: the hub committed both records", got)
	}
}

func TestParseStopIsNotResent(t *testing.T) {
	f := newFixture(t, Options{})
	db := openHubDB(t, filepath.Join(t.TempDir(), "hub.db"))
	h := serve(t, db, f.reg, hubsync.Options{})
	f.dialer.point(h.addr)
	a := filepath.Join(f.dir, "a.jsonl")
	good := line(sessA, "one")
	writeFile(t, a, good+"{not json\n"+line(sessA, "two"))

	f.mustSync()
	if got := f.checkpoint(a); got != int64(len(good)) {
		t.Fatalf("checkpoint = %d; want the valid prefix %d", got, len(good))
	}
	f.mustSync()
	if got := f.dialer.batches.Load(); got != 1 {
		t.Fatalf("data batches = %d; want 1: a stopped source must not resend unchanged bytes", got)
	}
	if n := strings.Count(f.log.String(), "claudecode: source stops"); n != 1 {
		t.Fatalf("stop reported %d times; want once:\n%s", n, f.log)
	}
	appendFile(t, a, line(sessA, "three"))
	f.mustSync()
	if got := f.dialer.batches.Load(); got != 2 {
		t.Fatalf("data batches = %d; want one resend after growth", got)
	}
	if got := count(t, db, `SELECT count(*) FROM events`); got != 1 {
		t.Fatalf("hub events = %d; want 1", got)
	}
	if got := f.obs.Gauge("watch_sources", "stopped"); got != 1 {
		t.Errorf("stopped sources gauge = %v, want 1", got)
	}
	if got := f.obs.Counter("upload_events_inserted"); got != 1 {
		t.Errorf("upload_events_inserted = %d, want 1: resends past a stop do not duplicate", got)
	}
	if got := f.obs.Counter("upload_batches", "attempted"); got != 2 {
		t.Errorf("attempted batches = %d, want 2", got)
	}
}

func TestStopCacheRetriesAfterInPlaceRepair(t *testing.T) {
	f := newFixture(t, Options{})
	db := openHubDB(t, filepath.Join(t.TempDir(), "hub.db"))
	h := serve(t, db, f.reg, hubsync.Options{})
	f.dialer.point(h.addr)
	a := filepath.Join(f.dir, "a.jsonl")
	good := line(sessA, "one")
	validTail := line(sessA, "two")
	brokenIdx := len(validTail) - 2
	torn := validTail[:brokenIdx] + "X" + validTail[brokenIdx+1:]
	writeFile(t, a, good+torn)

	f.mustSync()
	if got := f.checkpoint(a); got != int64(len(good)) {
		t.Fatalf("checkpoint = %d; want the valid prefix %d", got, len(good))
	}
	f.mustSync()
	if got := f.dialer.batches.Load(); got != 1 {
		t.Fatalf("data batches = %d; want 1: the stop cache must not resend unchanged bytes", got)
	}

	info, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	priorModTime := info.ModTime()

	fh, err := os.OpenFile(a, os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.WriteAt([]byte("}"), int64(len(good)+brokenIdx)); err != nil {
		t.Fatal(err)
	}
	if err := fh.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(a, priorModTime.Add(time.Second), priorModTime.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(a); err != nil || info.Size() != int64(len(good)+len(torn)) {
		t.Fatalf("repair changed the file size: %+v, %v", info, err)
	}

	f.mustSync()
	if got := f.dialer.batches.Load(); got != 2 {
		t.Fatalf("data batches = %d; want one resend after the same-size in-place repair", got)
	}
	if got := count(t, db, `SELECT count(*) FROM events`); got != 2 {
		t.Fatalf("hub events = %d; want 2", got)
	}
}

func TestIncompleteTailNotReReadUntilGrowth(t *testing.T) {
	f := newFixture(t, Options{})
	db := openHubDB(t, filepath.Join(t.TempDir(), "hub.db"))
	h := serve(t, db, f.reg, hubsync.Options{})
	f.dialer.point(h.addr)
	a := filepath.Join(f.dir, "a.jsonl")
	good := line(sessA, "one")
	full := line(sessA, "two")
	partial := full[:len(full)-3]
	writeFile(t, a, good+partial)

	f.mustSync()
	if got := f.checkpoint(a); got != int64(len(good)) {
		t.Fatalf("checkpoint = %d; want the complete-record prefix %d", got, len(good))
	}
	if got := f.dialer.batches.Load(); got != 1 {
		t.Fatalf("data batches = %d; want 1", got)
	}

	if err := os.Chmod(a, 0); err != nil {
		t.Fatal(err)
	}
	if fh, err := os.Open(a); err == nil {
		fh.Close()
		t.Skipf("chmod 0 left %s readable", a)
	}

	f.mustSync()
	if strings.Contains(f.log.String(), "permission denied") {
		t.Fatalf("incomplete tail was re-read though nothing changed:\n%s", f.log)
	}
	if got := f.dialer.batches.Load(); got != 1 {
		t.Fatalf("data batches = %d; want still 1: the unchanged partial tail must not resend", got)
	}

	if err := os.Chmod(a, 0o600); err != nil {
		t.Fatal(err)
	}
	appendFile(t, a, full[len(full)-3:])
	f.mustSync()
	if got := f.dialer.batches.Load(); got != 2 {
		t.Fatalf("data batches = %d; want one resend after the tail completed", got)
	}
	if got := count(t, db, `SELECT count(*) FROM events`); got != 2 {
		t.Fatalf("hub events = %d; want 2", got)
	}
}

func TestOversizedRecordStopsLocally(t *testing.T) {
	f := newFixture(t, Options{})
	db := openHubDB(t, filepath.Join(t.TempDir(), "hub.db"))
	h := serve(t, db, f.reg, hubsync.Options{})
	f.dialer.point(h.addr)
	big := line(sessA, strings.Repeat("x", int(hubsync.MaxRecordBytes)))
	writeFile(t, filepath.Join(f.dir, "a.jsonl"), line(sessA, "one")+big)
	writeFile(t, filepath.Join(f.dir, "b.jsonl"), line(sessB, "two"))

	f.mustSync()
	if got := count(t, db, `SELECT count(*) FROM events`); got != 2 {
		t.Fatalf("hub events = %d; want 2", got)
	}
	if !strings.Contains(f.log.String(), "record size limit") {
		t.Fatalf("log lacks the size-limit stop:\n%s", f.log)
	}
}

func TestLargeFileUploadsInBoundedBatches(t *testing.T) {
	f := newFixture(t, Options{})
	db := openHubDB(t, filepath.Join(t.TempDir(), "hub.db"))
	h := serve(t, db, f.reg, hubsync.Options{})
	f.dialer.point(h.addr)
	var sb strings.Builder
	n := 0
	for sb.Len() <= hubsync.MaxBatchBytes+1 {
		sb.WriteString(line(sessA, strings.Repeat("y", 200<<10)))
		n++
	}
	a := filepath.Join(f.dir, "a.jsonl")
	writeFile(t, a, sb.String())
	if stats := f.mustSync(); stats.Inserted != int64(n) {
		t.Fatalf("stats = %+v; want %d events", stats, n)
	}
	if got := f.dialer.batches.Load(); got != 2 {
		t.Fatalf("data batches = %d; want 2 for a file just above the batch bound", got)
	}
	if got, want := f.checkpoint(a), size(t, a); got != want {
		t.Fatalf("checkpoint = %d; want %d", got, want)
	}
}

func TestRevokedDeviceStops(t *testing.T) {
	f := newFixture(t, Options{})
	h := serve(t, openHubDB(t, filepath.Join(t.TempDir(), "hub.db")), f.reg, hubsync.Options{})
	f.dialer.point(h.addr)
	writeFile(t, filepath.Join(f.dir, "a.jsonl"), line(sessA, "one"))
	if err := f.reg.Revoke(context.Background(), f.cred.DeviceID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.sync(); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Sync error = %v; want ErrUnauthorized", err)
	}
	err := f.up.Run(context.Background(), func() ([]string, error) { return f.candidates(), nil })
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Run error = %v; want ErrUnauthorized", err)
	}
}

func TestForeignSourceBlocksOnlyThatSource(t *testing.T) {
	f := newFixture(t, Options{})
	db := openHubDB(t, filepath.Join(t.TempDir(), "hub.db"))
	h := serve(t, db, f.reg, hubsync.Options{})
	f.dialer.point(h.addr)
	a := filepath.Join(f.dir, "a.jsonl")
	b := filepath.Join(f.dir, "b.jsonl")
	writeFile(t, a, line(sessA, "one"))
	writeFile(t, b, line(sessB, "two"))

	w, err := watch.New(f.stateDir, claudecode.AgentName, watch.Options{})
	if err != nil {
		t.Fatal(err)
	}
	outcomes, err := w.Poll([]string{a})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RegisterSource(context.Background(), outcomes[0].SourceID,
		"ffffffffffffffffffffffffffffffff", claudecode.AgentName, "/elsewhere"); err != nil {
		t.Fatal(err)
	}
	f.up = f.newUploader(Options{})
	f.mustSync()
	if got := count(t, db, `SELECT count(*) FROM events`); got != 1 {
		t.Fatalf("hub events = %d; want only b's", got)
	}
	if !strings.Contains(f.log.String(), "ownership") {
		t.Fatalf("log lacks the ownership refusal:\n%s", f.log)
	}
	if got := f.checkpoint(a); got != 0 {
		t.Fatalf("refused source checkpoint = %d; want 0", got)
	}
}

func TestNewRejectsIncompleteConfig(t *testing.T) {
	w, err := watch.New(t.TempDir(), claudecode.AgentName, watch.Options{})
	if err != nil {
		t.Fatal(err)
	}
	dial := func(context.Context) (net.Conn, error) { return nil, io.EOF }
	cred := device.Credential{DeviceID: "0123456789abcdef0123456789abcdef", Secret: "s"}
	for name, tc := range map[string]struct {
		w    *watch.Watcher
		cred device.Credential
		dial Dialer
		opts Options
	}{
		"no watcher":    {nil, cred, dial, Options{Agent: "claude-code"}},
		"no dialer":     {w, cred, nil, Options{Agent: "claude-code"}},
		"no credential": {w, device.Credential{}, dial, Options{Agent: "claude-code"}},
		"no agent":      {w, cred, dial, Options{}},
	} {
		if _, err := New(tc.w, tc.cred, tc.dial, tc.opts); err == nil {
			t.Fatalf("%s: New succeeded", name)
		}
	}
}

func TestRecordAboveEightMiBSyncs(t *testing.T) {
	f := newFixture(t, Options{})
	db := openHubDB(t, filepath.Join(t.TempDir(), "hub.db"))
	h := serve(t, db, f.reg, hubsync.Options{})
	f.dialer.point(h.addr)
	a := filepath.Join(f.dir, "a.jsonl")
	writeFile(t, a, line(sessA, "one")+line(sessA, strings.Repeat("z", 10<<20))+line(sessA, "three"))

	f.mustSync()
	if got := count(t, db, `SELECT count(*) FROM events`); got != 3 {
		t.Fatalf("hub events = %d; want 3\n%s", got, f.log)
	}
	if got, want := f.checkpoint(a), size(t, a); got != want {
		t.Fatalf("checkpoint = %d; want %d", got, want)
	}
}

func TestBatchWindowHoldsLargestRecord(t *testing.T) {
	if hubsync.MaxBatchBytes <= hubsync.MaxRecordBytes+1 {
		t.Fatalf("batch window %d cannot hold a %d-byte record and its newline", hubsync.MaxBatchBytes, hubsync.MaxRecordBytes)
	}
}

func TestBacklogProgressReportedWhileDraining(t *testing.T) {
	f := newFixture(t, Options{})
	h := serve(t, openHubDB(t, filepath.Join(t.TempDir(), "hub.db")), f.reg, hubsync.Options{})
	f.dialer.point(h.addr)

	a := filepath.Join(f.dir, "a.jsonl")
	var big strings.Builder
	for big.Len() < backlogLogFloor+4096 {
		fmt.Fprintf(&big, "%s", line(sessA, "backlog payload"))
	}
	writeFile(t, a, big.String())

	f.mustSync()
	f.mustSync()

	logged := f.log.String()
	if !strings.Contains(logged, "upload backlog for claude-code:") {
		t.Fatalf("log misses the backlog report:\n%s", logged)
	}
	if !strings.Contains(logged, fmt.Sprintf("complete, %d bytes acknowledged", size(t, a))) {
		t.Fatalf("log misses the completion line with the drained byte count:\n%s", logged)
	}
	if !strings.Contains(logged, "rate unknown so far") {
		t.Fatalf("log misses the unknown-rate report before a window exists:\n%s", logged)
	}
	agent := claudecode.AgentName
	if got := f.obs.Gauge("upload_backlog_bytes", agent); got != 0 {
		t.Fatalf("backlog gauge after completion = %v; want 0", got)
	}
	if got := f.obs.Gauge("upload_backlog_sent_bytes", agent); int64(got) != size(t, a) {
		t.Fatalf("sent gauge = %v; want %d", got, size(t, a))
	}
	if got := f.obs.Gauge("upload_backlog_rate_bytes", agent); got != 0 {
		t.Fatalf("rate gauge after completion = %v; want 0", got)
	}
	if got := f.obs.Gauge("upload_backlog_eta_ms", agent); got != 0 {
		t.Fatalf("eta gauge after completion = %v; want 0", got)
	}
}

func TestBacklogRateAndETA(t *testing.T) {
	f := newFixture(t, Options{})
	now := time.Now()
	mib := int64(1 << 20)
	f.up.drain = &backlogDrain{
		start: now.Add(-30 * time.Second),
		left:  5 * mib,
		total: 10 * mib,
		sent:  5 * mib,
		samples: []drainSample{
			{at: now.Add(-5 * time.Second), sent: 4 * mib},
			{at: now, sent: 5 * mib},
		},
	}
	f.up.reportBacklog(now, false)

	agent := claudecode.AgentName
	rate := f.obs.Gauge("upload_backlog_rate_bytes", agent)
	if rate <= 0 {
		t.Fatalf("rate gauge = %v; want a measured rate", rate)
	}
	eta := f.obs.Gauge("upload_backlog_eta_ms", agent)
	wantETA := 25000.0
	if eta < wantETA*0.99 || eta > wantETA*1.01 {
		t.Fatalf("eta gauge = %v; want about %v ms", eta, wantETA)
	}
	if got := f.obs.Gauge("upload_backlog_bytes", agent); int64(got) != 5*mib {
		t.Fatalf("backlog gauge = %v; want %d", got, 5*mib)
	}
	logged := f.log.String()
	if !strings.Contains(logged, "bytes/s, about") || !strings.Contains(logged, "left") {
		t.Fatalf("log misses the rate and estimate:\n%s", logged)
	}
}

func TestBacklogSmallAppendStaysQuiet(t *testing.T) {
	f := newFixture(t, Options{})
	h := serve(t, openHubDB(t, filepath.Join(t.TempDir(), "hub.db")), f.reg, hubsync.Options{})
	f.dialer.point(h.addr)

	a := filepath.Join(f.dir, "a.jsonl")
	writeFile(t, a, line(sessA, "one"))
	f.mustSync()
	f.log.mu.Lock()
	f.log.buf.Reset()
	f.log.mu.Unlock()

	appendFile(t, a, line(sessA, "two"))
	f.mustSync()
	if strings.Contains(f.log.String(), "upload backlog") {
		t.Fatalf("a small steady-state append must not log backlog lines:\n%s", f.log)
	}
	agent := claudecode.AgentName
	if got := f.obs.Gauge("upload_backlog_bytes", agent); got != 0 {
		t.Fatalf("backlog gauge after a small drained append = %v; want 0", got)
	}
}

func TestCompressedUploadNegotiated(t *testing.T) {
	f := newFixture(t, Options{})
	h := serve(t, openHubDB(t, filepath.Join(t.TempDir(), "hub.db")), f.reg, hubsync.Options{})
	f.dialer.point(h.addr)
	a := filepath.Join(f.dir, "a.jsonl")
	one := line(sessA, "one")
	two := line(sessA, "two")
	writeFile(t, a, strings.Repeat(one, 200)+two)

	stats := f.mustSync()
	if stats.Inserted != 201 {
		t.Fatalf("stats = %+v; want 201 events", stats)
	}
	wire := f.obs.Counter("upload_wire_bytes")
	payload := f.obs.Counter("upload_payload_bytes")
	if wire == 0 || wire >= payload {
		t.Fatalf("wire bytes = %d, payload bytes = %d; want compression on the wire", wire, payload)
	}
	if got := count(t, h.db, `SELECT count(*) FROM events`); got != 201 {
		t.Fatalf("hub events = %d, want 201", got)
	}
	if got, want := f.checkpoint(a), size(t, a); got != want {
		t.Fatalf("checkpoint = %d, want %d", got, want)
	}
}

type oldHub struct {
	t          *testing.T
	ln         net.Listener
	mu         sync.Mutex
	conns      map[net.Conn]struct{}
	refused    atomic.Int32
	plain      atomic.Int32
	compressed atomic.Int32
}

func serveOldHub(t *testing.T) *oldHub {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h := &oldHub{t: t, ln: ln, conns: map[net.Conn]struct{}{}}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			h.mu.Lock()
			h.conns[c] = struct{}{}
			h.mu.Unlock()
			go h.serve(c)
		}
	}()
	return h
}

func (h *oldHub) kick() {
	h.mu.Lock()
	conns := make([]net.Conn, 0, len(h.conns))
	for c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.Unlock()
	for _, c := range conns {
		c.Close()
	}
}

func (h *oldHub) serve(c net.Conn) {
	defer c.Close()
	h.mu.Lock()
	delete(h.conns, c)
	h.mu.Unlock()
	br := bufio.NewReader(c)
	hello, _, err := hubsync.ReadAuthFrame(br)
	if err != nil || hello == nil {
		return
	}
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	for {
		head, err := br.Peek(1)
		if err != nil {
			return
		}
		if head[0] == 'V' {
			h.refused.Add(1)
			_ = hubsync.WriteError(c, hubsync.CodeMalformed,
				fmt.Sprintf("hubsync: bad frame marker %q", head[0]))
			return
		}
		if head[0] == 'Z' {
			h.compressed.Add(1)
			return
		}
		frame, err := hubsync.ReadFrame(br)
		if err != nil {
			return
		}
		switch {
		case frame.Registration != nil:
			_ = hubsync.WriteAck(c, hubsync.Ack{})
		case frame.Batch != nil:
			b := frame.Batch
			if len(b.Records) > 0 {
				h.plain.Add(1)
			}
			_ = hubsync.WriteAck(c, hubsync.Ack{Cursor: b.End, Inserted: int64(len(b.Records))})
		case frame.Workspaces != nil:
			_ = hubsync.WriteError(c, hubsync.CodeUnsupported, "agent has no parser at this hub")
		default:
			return
		}
	}
}

func TestOldHubFallsBackToPlainFrames(t *testing.T) {
	f := newFixture(t, Options{})
	old := serveOldHub(t)
	f.dialer.point(old.ln.Addr().String())
	a := filepath.Join(f.dir, "a.jsonl")
	one := line(sessA, "one")
	two := line(sessA, "two")
	writeFile(t, a, one+two)

	stats := f.mustSync()
	if stats.Inserted != 2 {
		t.Fatalf("stats = %+v; want 2 events", stats)
	}
	if got := old.refused.Load(); got != 1 {
		t.Fatalf("capability refusals = %d, want exactly one on first contact", got)
	}
	if got := old.compressed.Load(); got != 0 {
		t.Fatalf("compressed batches = %d, want none against an old hub", got)
	}
	if got := old.plain.Load(); got != 1 {
		t.Fatalf("plain data batches = %d, want 1", got)
	}
	if !strings.Contains(f.log.String(), "capability negotiation") {
		t.Fatalf("log misses the fallback line:\n%s", f.log)
	}
	if got, want := f.checkpoint(a), size(t, a); got != want {
		t.Fatalf("checkpoint = %d, want %d", got, want)
	}

	old.kick()
	appendFile(t, a, two)
	f.mustSync()
	if got := old.refused.Load(); got != 1 {
		t.Fatalf("capability refusals after reconnect = %d, want the process to stay plain", got)
	}
	if got := old.plain.Load(); got != 2 {
		t.Fatalf("plain data batches after reconnect = %d, want 2", got)
	}
}
