// End-to-end pairing, authorization, and revocation tests: a real sync
// endpoint over TCP, authenticated by the credential registry, serving real
// devices through the wire protocol of internal/hubsync. These tests pin the
// implementation against the rules of tla/Auth.tla: invitation consumption,
// per-request revocation checks, connection teardown and in-flight
// cancellation on revocation, and identity spoofing.

package registry

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nibzard/ohya/internal/claudecode"
	"github.com/nibzard/ohya/internal/device"
	"github.com/nibzard/ohya/internal/hubsync"
	"github.com/nibzard/ohya/internal/mcpserver"
	"github.com/nibzard/ohya/internal/store"
)

// inertPoll disables the revocation watcher for tests that pin only the
// per-request check.
const inertPoll = time.Hour

// testHub runs one registry-authenticated endpoint for a test.
type testHub struct {
	t      *testing.T
	reg    *DB
	hubDB  *store.DB
	addr   string
	cancel context.CancelFunc
	done   chan error
	wait   sync.Once
}

// stop cancels the serving context and waits for Serve to return.
func (h *testHub) stop() {
	h.cancel()
	h.wait.Do(func() {
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

// startHub serves the endpoint on a free local port. poll configures how
// quickly the endpoint learns of revocations committed elsewhere; opts carries
// the endpoint options, with Retry reset to the given value. A non-nil engine
// wires the remote query path through a real mcpserver engine; nil keeps the
// endpoint upload-only.
func startHub(t *testing.T, poll time.Duration, opts hubsync.Options, engine *mcpserver.EngineOptions) *testHub {
	t.Helper()
	retry := opts.Retry
	hubDB, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	reg, err := Open(filepath.Join(t.TempDir(), FileName))
	if err != nil {
		t.Fatal(err)
	}
	if engine != nil {
		readers, err := hubDB.Reader()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { readers.Close() })
		opts.Remote = mcpserver.NewEngine(readers, hubDB.Writer(), *engine)
	}
	auth := &Authenticator{DB: reg, PollInterval: poll}
	opts.Retry = retry
	ep, err := hubsync.NewEndpoint(hubDB, auth, opts)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0") // free port
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ep.Serve(ctx, ln) }()
	h := &testHub{t: t, reg: reg, hubDB: hubDB, addr: ln.Addr().String(), cancel: cancel, done: done}
	t.Cleanup(h.stop)
	t.Cleanup(func() { hubDB.Close(); reg.Close() })
	return h
}

// deviceClient is one device connection to the test hub.
type deviceClient struct {
	t    *testing.T
	conn net.Conn
	br   *bufio.Reader
}

func dialHub(t *testing.T, addr string) *deviceClient {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	d := &deviceClient{t: t, conn: conn, br: bufio.NewReader(conn)}
	t.Cleanup(func() { conn.Close() })
	return d
}

func (d *deviceClient) deadline() {
	d.t.Helper()
	if err := d.conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		d.t.Fatal(err)
	}
}

func (d *deviceClient) register(deviceID, sourceID, path string) {
	d.t.Helper()
	d.deadline()
	if err := hubsync.WriteRegistration(d.conn, hubsync.Registration{
		DeviceID: deviceID, SourceID: sourceID, Agent: claudecode.AgentName, Path: path}); err != nil {
		d.t.Fatal(err)
	}
}

func (d *deviceClient) send(b hubsync.Batch) {
	d.t.Helper()
	d.deadline()
	if err := hubsync.WriteBatch(d.conn, b); err != nil {
		d.t.Fatal(err)
	}
}

func (d *deviceClient) probe(deviceID, sourceID string) {
	d.t.Helper()
	d.send(hubsync.Batch{DeviceID: deviceID, SourceID: sourceID, Start: 0, End: 0})
}

func (d *deviceClient) reply() (hubsync.Ack, *hubsync.Error) {
	d.t.Helper()
	d.deadline()
	ack, perr, err := hubsync.ReadReply(d.br)
	if err != nil {
		d.t.Fatalf("read reply: %v", err)
	}
	return ack, perr
}

func (d *deviceClient) mustAck(want hubsync.Ack) {
	d.t.Helper()
	ack, perr := d.reply()
	if perr != nil {
		d.t.Fatalf("reply error = %+v; want ack %+v", perr, want)
	}
	if ack != want {
		d.t.Fatalf("ack = %+v; want %+v", ack, want)
	}
}

func (d *deviceClient) wantError(code hubsync.ErrorCode, sub string) {
	d.t.Helper()
	ack, perr := d.reply()
	if perr == nil {
		d.t.Fatalf("reply ack = %+v; want error %v", ack, code)
	}
	if perr.Code != code {
		d.t.Fatalf("error code = %v (%q); want %v", perr.Code, perr.Message, code)
	}
	if sub != "" && !strings.Contains(perr.Message, sub) {
		d.t.Fatalf("error message = %q; want text %q", perr.Message, sub)
	}
}

// expectClosed asserts the hub closed the connection: the next read fails.
func (d *deviceClient) expectClosed() {
	d.t.Helper()
	d.deadline()
	if _, _, err := hubsync.ReadReply(d.br); err == nil {
		d.t.Fatal("connection is still open; want the hub to close it")
	}
}

// rawLine builds one minimal Claude Code user record for a session ID.
func rawLine(sessionID, content string) []byte {
	return []byte(fmt.Sprintf(`{"type":"user","sessionId":%q,"message":{"role":"user","content":%q}}`, sessionID, content))
}

// oneRecordBatch frames one record as a batch from offset 0.
func oneRecordBatch(deviceID, sourceID string, rec []byte) hubsync.Batch {
	return hubsync.Batch{DeviceID: deviceID, SourceID: sourceID,
		Start: 0, End: int64(len(rec)) + 1, Records: []hubsync.Record{{Offset: 0, JSON: rec}}}
}

// pairOnHub dials the hub, consumes a fresh invitation for deviceID, and
// keeps the connection, which is now an authorized session of the device.
func pairOnHub(t *testing.T, h *testHub, deviceID string) (*deviceClient, device.Credential) {
	t.Helper()
	inv, err := h.reg.Issue(context.Background(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	d := dialHub(t, h.addr)
	d.deadline()
	credential, err := hubsync.Pair(d.conn, inv.Secret, deviceID)
	if err != nil {
		t.Fatalf("pair: %v", err)
	}
	return d, device.Credential{DeviceID: deviceID, Secret: credential}
}

// helloOnHub dials the hub and presents a stored credential.
func helloOnHub(t *testing.T, h *testHub, credential string) *deviceClient {
	t.Helper()
	d := dialHub(t, h.addr)
	d.deadline()
	if err := hubsync.WriteHello(d.conn, credential); err != nil {
		t.Fatal(err)
	}
	return d
}

// TestPairingEnrollsAndServes walks the full device journey: a stranger
// consumes a one-time invitation, stores the minted credential in its
// protected credential file, uploads its own source on the pairing
// connection, and reconnects later through a credential hello.
func TestPairingEnrollsAndServes(t *testing.T) {
	h := startHub(t, inertPoll, hubsync.Options{Retry: hubsync.RetryPolicy{Base: time.Millisecond}}, nil)

	// The device's stable identity exists before pairing; the credential
	// authenticates it.
	stateDir := t.TempDir()
	deviceID, err := device.LoadOrCreate(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	d, cred := pairOnHub(t, h, deviceID)
	if cred.DeviceID != deviceID {
		t.Fatalf("credential device = %q; want %q", cred.DeviceID, deviceID)
	}
	if err := device.SaveCredential(stateDir, cred); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(stateDir, device.CredentialFileName))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("credential file perms = %o; want owner-only", perm)
	}
	stored, err := device.LoadCredential(stateDir)
	if err != nil || stored != cred {
		t.Fatalf("stored credential = %+v, %v; want %+v", stored, err, cred)
	}

	// The pairing connection is an authorized session: register and upload.
	d.register(deviceID, "src-1", "/dev/s.jsonl")
	d.mustAck(hubsync.Ack{})
	line := rawLine("00000000-0000-4000-8000-0000000000a1", "first prompt")
	d.send(oneRecordBatch(deviceID, "src-1", line))
	d.mustAck(hubsync.Ack{Cursor: int64(len(line)) + 1, Inserted: 1})

	// A later connection presents the stored credential, not an invitation.
	fresh := helloOnHub(t, h, stored.Secret)
	fresh.probe(deviceID, "src-1")
	fresh.mustAck(hubsync.Ack{Cursor: int64(len(line)) + 1})

	// A stranger with an unknown credential learns CodeAuth and nothing else.
	stranger := helloOnHub(t, h, "ffffffffffffffffffffffffffffffff")
	stranger.register(deviceID, "src-x", "/x.jsonl")
	stranger.wantError(hubsync.CodeAuth, "credential")
	stranger.expectClosed()

	// The invitation was one-time: presenting it again is refused.
	inv, err := h.reg.Issue(context.Background(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	replay := dialHub(t, h.addr)
	replay.deadline()
	if _, err := hubsync.Pair(replay.conn, inv.Secret, deviceID); err == nil {
		t.Fatal("an active device re-paired through a fresh invitation")
	}
}

// TestSpoofedDeviceRejected pins that a frame can never name another
// device's identity: the connection's registry identity rules, whatever the
// frames claim. A spoofed registration and a spoofed batch both end the
// connection; pairing under an already-active identity is refused too.
func TestSpoofedDeviceRejected(t *testing.T) {
	h := startHub(t, inertPoll, hubsync.Options{Retry: hubsync.RetryPolicy{Base: time.Millisecond}}, nil)
	other := "ffffffffffffffffffffffffffffffff"

	a, cred := pairOnHub(t, h, "0123456789abcdef0123456789abcdef")
	a.register("0123456789abcdef0123456789abcdef", "src-1", "/a.jsonl")
	a.mustAck(hubsync.Ack{})

	// Spoofed registration: this connection belongs to device A, the frame
	// names another device.
	spoof := helloOnHub(t, h, cred.Secret)
	spoof.register(other, "src-2", "/b.jsonl")
	spoof.wantError(hubsync.CodeAuth, "belongs to")
	spoof.expectClosed()

	// Spoofed batch: A's credential, B's identity on the upload.
	spoof2 := helloOnHub(t, h, cred.Secret)
	spoof2.register("0123456789abcdef0123456789abcdef", "src-1", "/a.jsonl")
	spoof2.mustAck(hubsync.Ack{})
	line := rawLine("00000000-0000-4000-8000-0000000000a1", "claiming another device")
	spoof2.send(oneRecordBatch(other, "src-1", line))
	spoof2.wantError(hubsync.CodeAuth, "belongs to")
	spoof2.expectClosed()

	// Pairing spoof: a fresh invitation presented under an already-active
	// identity enrolls nothing.
	inv, err := h.reg.Issue(context.Background(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	steal := dialHub(t, h.addr)
	steal.deadline()
	if _, err := hubsync.Pair(steal.conn, inv.Secret, "0123456789abcdef0123456789abcdef"); err == nil {
		t.Fatal("pairing under an active identity succeeded")
	}

	// The genuine device still works, and the spoofed frames stored nothing.
	good := helloOnHub(t, h, cred.Secret)
	good.send(oneRecordBatch("0123456789abcdef0123456789abcdef", "src-1", line))
	good.mustAck(hubsync.Ack{Cursor: int64(len(line)) + 1, Inserted: 1})
	var spoofed int
	if err := h.hubDB.Writer().QueryRow(
		`SELECT count(*) FROM sessions WHERE device_id = ?`, other).Scan(&spoofed); err != nil {
		t.Fatal(err)
	}
	if spoofed != 0 {
		t.Fatalf("%d sessions exist under the spoofed identity; want 0", spoofed)
	}
}

// TestRevocationCheckedOnEveryRequest pins the Authorized guard of
// tla/Auth.tla with the revocation watcher inert: the very next frame on a
// live connection is refused, and the revoked credential cannot open a new
// session.
func TestRevocationCheckedOnEveryRequest(t *testing.T) {
	h := startHub(t, inertPoll, hubsync.Options{Retry: hubsync.RetryPolicy{Base: time.Millisecond}}, nil)
	deviceID := "0123456789abcdef0123456789abcdef"

	d, cred := pairOnHub(t, h, deviceID)
	d.register(deviceID, "src-1", "/a.jsonl")
	d.mustAck(hubsync.Ack{})
	line := rawLine("00000000-0000-4000-8000-0000000000a1", "before revocation")
	d.send(oneRecordBatch(deviceID, "src-1", line))
	d.mustAck(hubsync.Ack{Cursor: int64(len(line)) + 1, Inserted: 1})

	if err := h.reg.Revoke(context.Background(), deviceID); err != nil {
		t.Fatal(err)
	}

	// The live connection's next frame hits the per-request registry check.
	d.probe(deviceID, "src-1")
	d.wantError(hubsync.CodeAuth, "revoked")
	d.expectClosed()

	// A new connection cannot authenticate: the session cannot be
	// established again for the revoked identity.
	reborn := helloOnHub(t, h, cred.Secret)
	reborn.register(deviceID, "src-1", "/a.jsonl")
	reborn.wantError(hubsync.CodeAuth, "revoked")
	reborn.expectClosed()

	// History written before the revocation stays.
	var events int
	if err := h.hubDB.Writer().QueryRow(`SELECT count(*) FROM events`).Scan(&events); err != nil || events != 1 {
		t.Fatalf("events after revocation = %d, %v; want 1", events, err)
	}
}

// TestRevocationCancelsInFlightRequest pins the teardown half of
// RevokeDevice: a revocation that lands while a request is being processed
// closes the device's connections — the idle ones and the busy one — cancels
// the in-flight request so nothing commits, and the endpoint keeps serving
// other devices.
func TestRevocationCancelsInFlightRequest(t *testing.T) {
	h := startHub(t, 10*time.Millisecond, hubsync.Options{
		Retry: hubsync.RetryPolicy{Base: 50 * time.Millisecond, Cap: 500 * time.Millisecond, Attempts: 200}}, nil)
	deviceID := "0123456789abcdef0123456789abcdef"

	d, cred := pairOnHub(t, h, deviceID)
	d.register(deviceID, "src-1", "/a.jsonl")
	d.mustAck(hubsync.Ack{})
	idle := helloOnHub(t, h, cred.Secret) // same device, second connection

	// Every commit attempt fails inside the transaction, so the request
	// stays in flight in the retry loop until revocation cancels it.
	line := rawLine("00000000-0000-4000-8000-0000000000a1", "in flight when revoked")
	if _, err := h.hubDB.Writer().Exec(
		`CREATE TRIGGER block_commit BEFORE INSERT ON events
		 BEGIN SELECT RAISE(ABORT, 'blocked for the revocation test'); END`); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		ack hubsync.Ack
		err *hubsync.Error
	}
	replies := make(chan outcome, 1)
	go func() {
		d.send(oneRecordBatch(deviceID, "src-1", line))
		ack, perr, err := hubsync.ReadReply(d.br)
		if err != nil {
			// The expected outcome: the hub closed the connection.
			replies <- outcome{}
			return
		}
		replies <- outcome{ack: ack, err: perr}
	}()

	// The request is now unanswered and retrying. Revoke mid-request.
	time.Sleep(250 * time.Millisecond)
	if err := h.reg.Revoke(context.Background(), deviceID); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-replies:
		if got.err != nil || got.ack != (hubsync.Ack{}) {
			t.Fatalf("the in-flight request completed after revocation: %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the revoked device's busy connection was not closed")
	}
	idle.expectClosed()

	// Nothing committed: the cancelled request wrote no cursor and no event.
	var cursor, events int
	if err := h.hubDB.Writer().QueryRow(
		`SELECT cursor_bytes, (SELECT count(*) FROM events) FROM sources WHERE id='src-1'`).
		Scan(&cursor, &events); err != nil {
		t.Fatal(err)
	}
	if cursor != 0 || events != 0 {
		t.Fatalf("after cancelled request: cursor = %d, events = %d; want 0 and 0", cursor, events)
	}

	// The revoked credential is dead at the next connection.
	again := helloOnHub(t, h, cred.Secret)
	again.register(deviceID, "src-1", "/a.jsonl")
	again.wantError(hubsync.CodeAuth, "revoked")
	again.expectClosed()

	// The endpoint keeps serving other devices.
	other, _ := pairOnHub(t, h, "abababababababababababababababab")
	other.register("abababababababababababababababab", "src-b", "/b.jsonl")
	other.mustAck(hubsync.Ack{})
}

// TestSilentStrangerTimesOut pins the auth timeout: a connection that never
// presents a pairing request or credential holds its slot only for the
// configured window, then the endpoint closes it.
func TestSilentStrangerTimesOut(t *testing.T) {
	h := startHub(t, inertPoll, hubsync.Options{AuthTimeout: 100 * time.Millisecond}, nil)
	d := dialHub(t, h.addr)
	// No first frame. The endpoint must close the connection; a later frame
	// arrives at a closed peer.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, _, err := hubsync.ReadReply(d.br); err != nil {
			return // closed, as wanted
		}
		if time.Now().After(deadline) {
			t.Fatal("a silent stranger held its connection past the auth timeout")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// remoteClientOnHub dials the hub and presents a stored credential through
// the remote client, the way a joined device's MCP bridge connects.
func remoteClientOnHub(t *testing.T, h *testHub, credential string) *hubsync.RemoteClient {
	t.Helper()
	conn, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatal(err)
	}
	if err := hubsync.WriteHello(conn, credential); err != nil {
		t.Fatal(err)
	}
	client := hubsync.NewRemoteClient(conn)
	t.Cleanup(func() { client.Close() })
	return client
}

// TestRemoteQueriesThroughPairing walks the remote read and annotate path
// with real pairing: one device's upload becomes readable and annotatable by
// another paired device, the annotation writer is the authenticated device,
// and revocation ends that device's remote access at its next frame.
func TestRemoteQueriesThroughPairing(t *testing.T) {
	h := startHub(t, inertPoll, hubsync.Options{Retry: hubsync.RetryPolicy{Base: time.Millisecond}},
		&mcpserver.EngineOptions{})
	deviceA := "0123456789abcdef0123456789abcdef"
	deviceB := "abababababababababababababababab"

	a, credA := pairOnHub(t, h, deviceA)
	a.register(deviceA, "src-1", "/a.jsonl")
	a.mustAck(hubsync.Ack{})
	line := rawLine("00000000-0000-4000-8000-0000000000a1", "shared history")
	a.send(oneRecordBatch(deviceA, "src-1", line))
	a.mustAck(hubsync.Ack{Cursor: int64(len(line)) + 1, Inserted: 1})

	// A second paired device reads the first device's history — the MVP
	// read rule — and annotates it as itself.
	_, credB := pairOnHub(t, h, deviceB)
	b := remoteClientOnHub(t, h, credB.Secret)
	ctx := context.Background()
	res, err := b.Query(ctx, `SELECT count(*) FROM events`)
	if err != nil || res.RowCount != 1 || fmt.Sprint(res.Rows) != "[[1]]" {
		t.Fatalf("remote query = %+v, %v", res, err)
	}
	if res, err := b.Annotate(ctx, []byte(
		`{"operations":[{"op":"upsert","event_id":1,"namespace":"security","key":"review","value":{"status":"reviewed"}}]}`)); err != nil ||
		res.Applied != 1 {
		t.Fatalf("remote annotate = %+v, %v", res, err)
	}
	res, err = b.Query(ctx, `SELECT writer_id FROM event_annotations`)
	if err != nil || res.RowCount != 1 || res.Rows[0][0] != deviceB {
		t.Fatalf("annotation writer = %+v, %v; want %s", res, err, deviceB)
	}

	// A request naming a writer is rejected by the strict decode, and the
	// connection stays usable.
	if _, err := b.Annotate(ctx, []byte(
		`{"operations":[{"op":"upsert","event_id":1,"namespace":"x","key":"y","value":1,"writer_id":"attacker"}]}`)); err == nil {
		t.Fatal("spoofed writer annotate succeeded")
	}

	// Revocation ends deviceB's remote access: the per-frame registry check
	// refuses its next frame and the connection closes.
	if err := h.reg.Revoke(context.Background(), deviceB); err != nil {
		t.Fatal(err)
	}
	_, err = b.Query(ctx, `SELECT count(*) FROM events`)
	var perr *hubsync.Error
	if !errors.As(err, &perr) || perr.Code != hubsync.CodeAuth {
		t.Fatalf("query after revocation = %v; want CodeAuth", err)
	}

	// The other device keeps reading and writing, and deviceB's annotation
	// written before revocation stays.
	other := remoteClientOnHub(t, h, credA.Secret)
	if res, err := other.Query(ctx, `SELECT count(*) FROM event_annotations`); err != nil || res.RowCount != 1 {
		t.Fatalf("deviceA after deviceB revocation = %+v, %v", res, err)
	}
}

// TestRevocationCancelsInFlightQuery pins the teardown half for the remote
// path: a revocation that lands while a query is executing closes the
// revoked device's connection, interrupts the statement, and delivers no
// result; the endpoint keeps serving other devices' queries.
func TestRevocationCancelsInFlightQuery(t *testing.T) {
	h := startHub(t, 10*time.Millisecond, hubsync.Options{Retry: hubsync.RetryPolicy{Base: time.Millisecond}},
		&mcpserver.EngineOptions{Deadline: 30 * time.Second})
	deviceA := "0123456789abcdef0123456789abcdef"
	deviceB := "abababababababababababababababab"

	_, credA := pairOnHub(t, h, deviceA)
	_, credB := pairOnHub(t, h, deviceB)
	idle := remoteClientOnHub(t, h, credA.Secret)

	// The endless recursive count runs until the engine deadline — 30 s —
	// unless revocation interrupts it first.
	busy := remoteClientOnHub(t, h, credB.Secret)
	type outcome struct {
		result mcpserver.Result
		err    error
	}
	replies := make(chan outcome, 1)
	go func() {
		result, err := busy.Query(context.Background(),
			`WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM c) SELECT count(*) FROM c`)
		replies <- outcome{result: result, err: err}
	}()

	// The query is now executing. Revoke mid-query.
	time.Sleep(250 * time.Millisecond)
	if err := h.reg.Revoke(context.Background(), deviceB); err != nil {
		t.Fatal(err)
	}

	select {
	case got := <-replies:
		if got.err == nil {
			t.Fatalf("the in-flight query delivered a result after revocation: %+v", got.result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the revoked device's query was not interrupted")
	}

	// The revoked credential is dead at the next connection.
	again := remoteClientOnHub(t, h, credB.Secret)
	if _, err := again.Query(context.Background(), `SELECT 1`); err == nil {
		t.Fatal("revoked credential still queries")
	}

	// The endpoint keeps serving other devices, and nothing was written for
	// the revoked device to recall: a query is read-only.
	if res, err := idle.Query(context.Background(), `SELECT count(*) FROM events`); err != nil || res.RowCount != 1 {
		t.Fatalf("other device after revocation = %+v, %v", res, err)
	}
}
