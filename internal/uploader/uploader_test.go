// Uploader tests run the real sync endpoint over TCP, authenticated by the
// credential registry, and inject transport faults on the device side: lost
// acknowledgments, stalled replies, refused connections, hub restarts, and a
// hub restored from a backup.

package uploader

import (
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
	"github.com/nibzard/ohya/internal/registry"
	"github.com/nibzard/ohya/internal/store"
	"github.com/nibzard/ohya/internal/watch"
)

// testHub is one serving endpoint over a hub database and a registry.
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

// openHubDB opens a hub database and closes it at cleanup.
func openHubDB(t *testing.T, path string) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// serve runs an endpoint over db on a free local port.
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

// fault selects the injected transport failure of the next data batch.
type fault int32

const (
	noFault   fault = iota
	loseAck         // the hub commits, then the connection dies before the ack arrives
	stallAck        // the hub commits, but the ack never arrives: the request times out
	closedHub       // nothing listens: dialing fails
)

// testDialer dials the current hub address and injects one fault.
type testDialer struct {
	addr    atomic.Pointer[string]
	fault   atomic.Int32
	batches atomic.Int32 // data batches sent, probes excluded
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

// faultConn watches the frames the uploader writes. A frame begins with the
// first write after a reply was read; a data batch is a 'B' frame carrying
// JSON, which a probe never does.
type faultConn struct {
	net.Conn
	d          *testDialer
	frameStart bool
	dataBatch  bool
}

func (c *faultConn) Write(p []byte) (int, error) {
	if c.frameStart && len(p) > 0 {
		c.frameStart = false
		c.dataBatch = p[0] == 'B' && bytes.IndexByte(p, '{') >= 0
		if c.dataBatch {
			c.d.batches.Add(1)
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
		// Wait for the reply, so the hub has committed, then lose it.
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
		return c.Conn.Read(p) // nothing else arrives: the deadline fires
	}
	return c.Conn.Read(p)
}

// fixture is one paired device with a sessions directory.
type fixture struct {
	t        *testing.T
	dir      string // watched session files
	stateDir string
	reg      *registry.DB
	cred     device.Credential
	dialer   *testDialer
	log      *syncBuffer
	up       *Uploader
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
	f := &fixture{t: t, dir: t.TempDir(), stateDir: t.TempDir(), reg: reg, dialer: &testDialer{}, log: &syncBuffer{}}
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
	w, err := watch.New(f.stateDir, claudecode.AgentName, watch.Options{})
	if err != nil {
		f.t.Fatal(err)
	}
	opts.Agent = claudecode.AgentName
	opts.Log = f.log
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

// checkpoint reads the persisted local cursor of path through a fresh
// watcher, as a restarted device would.
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

// line builds one Claude Code user record, newline-terminated.
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

// TestOfflineCatchUp writes history while the hub is unreachable: nothing is
// acknowledged, and the first connected pass uploads every retained file.
func TestOfflineCatchUp(t *testing.T) {
	f := newFixture(t, Options{})
	a := filepath.Join(f.dir, "a.jsonl")
	b := filepath.Join(f.dir, "b.jsonl")
	writeFile(t, a, line(sessA, "one")+line(sessA, "two"))
	writeFile(t, b, line(sessB, "three"))

	f.dialer.fault.Store(int32(closedHub))
	f.wantDisconnected()
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

	// Appends upload incrementally on the same connection.
	appendFile(t, a, line(sessA, "four"))
	if stats := f.mustSync(); stats.Inserted != 1 {
		t.Fatalf("incremental stats = %+v; want 1 event", stats)
	}
	if got, want := f.checkpoint(a), size(t, a); got != want {
		t.Fatalf("checkpoint after append = %d; want %d", got, want)
	}
}

// TestLostAcknowledgment loses the ack after the hub committed: the device
// keeps its old checkpoint, and the next connection learns the committed
// cursor from the probe without duplicating any event.
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
}

// TestTimeoutRetries stalls an acknowledgment past the request timeout: the
// exchange fails, the checkpoint stays, and the retry replays idempotently.
func TestTimeoutRetries(t *testing.T) {
	f := newFixture(t, Options{RequestTimeout: 300 * time.Millisecond})
	h := serve(t, openHubDB(t, filepath.Join(t.TempDir(), "hub.db")), f.reg, hubsync.Options{})
	f.dialer.point(h.addr)
	a := filepath.Join(f.dir, "a.jsonl")
	writeFile(t, a, line(sessA, "one"))

	f.dialer.fault.Store(int32(stallAck))
	_, err := f.sync()
	if !errors.Is(err, ErrDisconnected) || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("Sync error = %v; want a timeout wrapping ErrDisconnected", err)
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
}

// TestReconnectAfterHubRestart drops the connection under the uploader: the
// pass fails, and the next one reconnects and resumes from the checkpoint.
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
}

// TestBusyHubRetries meets a hub at its connection bound: the refusal is a
// retryable disconnect, not a failure.
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
	time.Sleep(50 * time.Millisecond) // let the hub take the slot
	_, err = f.sync()
	if !errors.Is(err, ErrDisconnected) || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("Sync error = %v; want busy wrapping ErrDisconnected", err)
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

// TestRunRetriesUntilHubAppears runs the loop against a hub that starts
// late: capped backoff reconnects, and the retained files catch up.
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

// TestHubRestoreReplaysRetainedFiles restores the hub from a backup that
// predates acknowledged history: retained files replay the lost bytes, and a
// deleted file is reported as an unrecoverable gap.
func TestHubRestoreReplaysRetainedFiles(t *testing.T) {
	f := newFixture(t, Options{})
	dbPath := filepath.Join(t.TempDir(), "hub.db")
	db := openHubDB(t, dbPath)
	h := serve(t, db, f.reg, hubsync.Options{})
	f.dialer.point(h.addr)

	a := filepath.Join(f.dir, "a.jsonl") // retained: replays
	b := filepath.Join(f.dir, "b.jsonl") // deleted: unrecoverable gap
	writeFile(t, a, line(sessA, "one"))
	writeFile(t, b, line(sessB, "two"))
	f.mustSync()

	backup := filepath.Join(t.TempDir(), "backup.db")
	if _, err := db.Writer().Exec(`VACUUM INTO ?`, backup); err != nil {
		t.Fatal(err)
	}

	c := filepath.Join(f.dir, "c.jsonl") // registered after the backup
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

	// The gap is reported once, not every pass.
	f.mustSync()
	if n := strings.Count(f.log.String(), "unrecoverable gap"); n != 1 {
		t.Fatalf("gap reported %d times; want once", n)
	}
}

// TestReplacementBeforeUploadReported replaces a file whose appended bytes
// were never uploaded: the loss is reported, and the new incarnation uploads
// under a new source.
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
	f.wantDisconnected() // offline: the poll observes the appended bytes

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

// TestDeletionBeforeUploadReported deletes a file with unsent bytes.
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

// TestParseStopIsNotResent uploads a file with a malformed line: the valid
// prefix commits, the stop is reported once, and the stopped bytes are sent
// again only after the file grows.
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
}

// TestOversizedRecordStopsLocally keeps a record above the size limit off
// the wire, so the connection survives for the other sources.
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

// TestLargeFileUploadsInBoundedBatches uploads a file above the batch bound
// in several batches, one in flight at a time.
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

// TestRevokedDeviceStops pins that a revoked credential ends the uploader:
// retrying cannot help.
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

// TestForeignSourceBlocksOnlyThatSource meets a source the hub binds to
// another device: it stops uploading, and the other sources continue.
func TestForeignSourceBlocksOnlyThatSource(t *testing.T) {
	f := newFixture(t, Options{})
	db := openHubDB(t, filepath.Join(t.TempDir(), "hub.db"))
	h := serve(t, db, f.reg, hubsync.Options{})
	f.dialer.point(h.addr)
	a := filepath.Join(f.dir, "a.jsonl")
	b := filepath.Join(f.dir, "b.jsonl")
	writeFile(t, a, line(sessA, "one"))
	writeFile(t, b, line(sessB, "two"))

	// Learn a's source ID by polling the device state, then bind it to a
	// foreign device at the hub.
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

// TestNewRejectsIncompleteConfig pins the constructor's guards.
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
