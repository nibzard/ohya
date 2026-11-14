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

const inertPoll = time.Hour

type testHub struct {
	t      *testing.T
	reg    *DB
	hubDB  *store.DB
	addr   string
	cancel context.CancelFunc
	done   chan error
	wait   sync.Once
}

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
	ln, err := net.Listen("tcp", "127.0.0.1:0")
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

func (d *deviceClient) expectClosed() {
	d.t.Helper()
	d.deadline()
	if _, _, err := hubsync.ReadReply(d.br); err == nil {
		d.t.Fatal("connection is still open; want the hub to close it")
	}
}

func rawLine(sessionID, content string) []byte {
	return []byte(fmt.Sprintf(`{"type":"user","sessionId":%q,"message":{"role":"user","content":%q}}`, sessionID, content))
}

func oneRecordBatch(deviceID, sourceID string, rec []byte) hubsync.Batch {
	return hubsync.Batch{DeviceID: deviceID, SourceID: sourceID,
		Start: 0, End: int64(len(rec)) + 1, Records: []hubsync.Record{{Offset: 0, JSON: rec}}}
}

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

func helloOnHub(t *testing.T, h *testHub, credential string) *deviceClient {
	t.Helper()
	d := dialHub(t, h.addr)
	d.deadline()
	if err := hubsync.WriteHello(d.conn, credential); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestPairingEnrollsAndServes(t *testing.T) {
	h := startHub(t, inertPoll, hubsync.Options{Retry: hubsync.RetryPolicy{Base: time.Millisecond}}, nil)

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

	d.register(deviceID, "src-1", "/dev/s.jsonl")
	d.mustAck(hubsync.Ack{})
	line := rawLine("00000000-0000-4000-8000-0000000000a1", "first prompt")
	d.send(oneRecordBatch(deviceID, "src-1", line))
	d.mustAck(hubsync.Ack{Cursor: int64(len(line)) + 1, Inserted: 1})

	fresh := helloOnHub(t, h, stored.Secret)
	fresh.probe(deviceID, "src-1")
	fresh.mustAck(hubsync.Ack{Cursor: int64(len(line)) + 1})

	stranger := helloOnHub(t, h, "ffffffffffffffffffffffffffffffff")
	stranger.register(deviceID, "src-x", "/x.jsonl")
	stranger.wantError(hubsync.CodeAuth, "credential")
	stranger.expectClosed()

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

func TestSpoofedDeviceRejected(t *testing.T) {
	h := startHub(t, inertPoll, hubsync.Options{Retry: hubsync.RetryPolicy{Base: time.Millisecond}}, nil)
	other := "ffffffffffffffffffffffffffffffff"

	a, cred := pairOnHub(t, h, "0123456789abcdef0123456789abcdef")
	a.register("0123456789abcdef0123456789abcdef", "src-1", "/a.jsonl")
	a.mustAck(hubsync.Ack{})

	spoof := helloOnHub(t, h, cred.Secret)
	spoof.register(other, "src-2", "/b.jsonl")
	spoof.wantError(hubsync.CodeAuth, "belongs to")
	spoof.expectClosed()

	spoof2 := helloOnHub(t, h, cred.Secret)
	spoof2.register("0123456789abcdef0123456789abcdef", "src-1", "/a.jsonl")
	spoof2.mustAck(hubsync.Ack{})
	line := rawLine("00000000-0000-4000-8000-0000000000a1", "claiming another device")
	spoof2.send(oneRecordBatch(other, "src-1", line))
	spoof2.wantError(hubsync.CodeAuth, "belongs to")
	spoof2.expectClosed()

	inv, err := h.reg.Issue(context.Background(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	steal := dialHub(t, h.addr)
	steal.deadline()
	if _, err := hubsync.Pair(steal.conn, inv.Secret, "0123456789abcdef0123456789abcdef"); err == nil {
		t.Fatal("pairing under an active identity succeeded")
	}

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

	d.probe(deviceID, "src-1")
	d.wantError(hubsync.CodeAuth, "revoked")
	d.expectClosed()

	reborn := helloOnHub(t, h, cred.Secret)
	reborn.register(deviceID, "src-1", "/a.jsonl")
	reborn.wantError(hubsync.CodeAuth, "revoked")
	reborn.expectClosed()

	var events int
	if err := h.hubDB.Writer().QueryRow(`SELECT count(*) FROM events`).Scan(&events); err != nil || events != 1 {
		t.Fatalf("events after revocation = %d, %v; want 1", events, err)
	}
}

func TestRevocationCancelsInFlightRequest(t *testing.T) {
	h := startHub(t, 10*time.Millisecond, hubsync.Options{
		Retry: hubsync.RetryPolicy{Base: 50 * time.Millisecond, Cap: 500 * time.Millisecond, Attempts: 200}}, nil)
	deviceID := "0123456789abcdef0123456789abcdef"

	d, cred := pairOnHub(t, h, deviceID)
	d.register(deviceID, "src-1", "/a.jsonl")
	d.mustAck(hubsync.Ack{})
	idle := helloOnHub(t, h, cred.Secret)

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
			replies <- outcome{}
			return
		}
		replies <- outcome{ack: ack, err: perr}
	}()

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

	var cursor, events int
	if err := h.hubDB.Writer().QueryRow(
		`SELECT cursor_bytes, (SELECT count(*) FROM events) FROM sources WHERE id='src-1'`).
		Scan(&cursor, &events); err != nil {
		t.Fatal(err)
	}
	if cursor != 0 || events != 0 {
		t.Fatalf("after cancelled request: cursor = %d, events = %d; want 0 and 0", cursor, events)
	}

	again := helloOnHub(t, h, cred.Secret)
	again.register(deviceID, "src-1", "/a.jsonl")
	again.wantError(hubsync.CodeAuth, "revoked")
	again.expectClosed()

	other, _ := pairOnHub(t, h, "abababababababababababababababab")
	other.register("abababababababababababababababab", "src-b", "/b.jsonl")
	other.mustAck(hubsync.Ack{})
}

func TestSilentStrangerTimesOut(t *testing.T) {
	h := startHub(t, inertPoll, hubsync.Options{AuthTimeout: 100 * time.Millisecond}, nil)
	d := dialHub(t, h.addr)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, _, err := hubsync.ReadReply(d.br); err != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("a silent stranger held its connection past the auth timeout")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

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

	if _, err := b.Annotate(ctx, []byte(
		`{"operations":[{"op":"upsert","event_id":1,"namespace":"x","key":"y","value":1,"writer_id":"attacker"}]}`)); err == nil {
		t.Fatal("spoofed writer annotate succeeded")
	}

	if err := h.reg.Revoke(context.Background(), deviceB); err != nil {
		t.Fatal(err)
	}
	_, err = b.Query(ctx, `SELECT count(*) FROM events`)
	var perr *hubsync.Error
	if !errors.As(err, &perr) || perr.Code != hubsync.CodeAuth {
		t.Fatalf("query after revocation = %v; want CodeAuth", err)
	}

	other := remoteClientOnHub(t, h, credA.Secret)
	if res, err := other.Query(ctx, `SELECT count(*) FROM event_annotations`); err != nil || res.RowCount != 1 {
		t.Fatalf("deviceA after deviceB revocation = %+v, %v", res, err)
	}
}

func TestRevocationCancelsInFlightQuery(t *testing.T) {
	h := startHub(t, 10*time.Millisecond, hubsync.Options{Retry: hubsync.RetryPolicy{Base: time.Millisecond}},
		&mcpserver.EngineOptions{Deadline: 30 * time.Second})
	deviceA := "0123456789abcdef0123456789abcdef"
	deviceB := "abababababababababababababababab"

	_, credA := pairOnHub(t, h, deviceA)
	_, credB := pairOnHub(t, h, deviceB)
	idle := remoteClientOnHub(t, h, credA.Secret)

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

	again := remoteClientOnHub(t, h, credB.Secret)
	if _, err := again.Query(context.Background(), `SELECT 1`); err == nil {
		t.Fatal("revoked credential still queries")
	}

	if res, err := idle.Query(context.Background(), `SELECT count(*) FROM events`); err != nil || res.RowCount != 1 {
		t.Fatalf("other device after revocation = %+v, %v", res, err)
	}
}
