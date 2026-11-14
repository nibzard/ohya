package hubsync

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nibzard/ohya/internal/claudecode"
	"github.com/nibzard/ohya/internal/codex"
	"github.com/nibzard/ohya/internal/events"
	"github.com/nibzard/ohya/internal/pi"
	"github.com/nibzard/ohya/internal/store"
)

const (
	deviceA = "0123456789abcdef0123456789abcdef"
	deviceB = "ffffffffffffffffffffffffffffffff"
)

// fixtureRecords frames the main-session fixture as one batch's original
// records, preserving byte offsets.
func fixtureRecords(t *testing.T) []Record {
	t.Helper()
	recs := framedFixture(t, "claude-code", "main-session.jsonl")
	if len(recs) != 16 {
		t.Fatalf("fixture records = %d, want 16", len(recs))
	}
	return recs
}

// framedFixture frames one fixture's complete records, preserving byte
// offsets.
func framedFixture(t *testing.T, agent, name string) []Record {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", agent, name))
	if err != nil {
		t.Fatal(err)
	}
	var recs []Record
	off := int64(0)
	for len(data) > 0 {
		idx := bytes.IndexByte(data, '\n')
		if idx < 0 {
			break // the incomplete tail stays pending
		}
		recs = append(recs, Record{Offset: off, JSON: data[:idx:idx]})
		off += int64(idx) + 1
		data = data[idx+1:]
	}
	return recs
}

func fixtureBatch(deviceID, sourceID string, recs []Record) Batch {
	return Batch{DeviceID: deviceID, SourceID: sourceID,
		Start: recs[0].Offset, End: recs[len(recs)-1].End(), Records: recs}
}

// rawLine builds one minimal Claude Code user record for a session ID.
func rawLine(sessionID, content string) []byte {
	return []byte(fmt.Sprintf(`{"type":"user","sessionId":%q,"message":{"role":"user","content":%q}}`, sessionID, content))
}

// server runs one endpoint for its tests and stops it afterwards.
type server struct {
	t      *testing.T
	ep     *Endpoint
	addr   string
	cancel context.CancelFunc
	done   chan error
	wait   sync.Once
}

// stop cancels the serving context and waits for Serve to return; it is safe
// to call from the test and from the cleanup.
func (s *server) stop() {
	s.cancel()
	s.wait.Do(func() {
		select {
		case err := <-s.done:
			if err != nil {
				s.t.Errorf("serve: %v", err)
			}
		case <-time.After(10 * time.Second):
			s.t.Error("serve did not stop within 10 s")
		}
	})
}

func startEndpoint(t *testing.T, db *store.DB, auth Authenticator, opts Options) *server {
	t.Helper()
	if auth == nil {
		auth = StaticAuthenticator(deviceA)
	}
	ep, err := NewEndpoint(db, auth, opts)
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
	s := &server{t: t, ep: ep, addr: ln.Addr().String(), cancel: cancel, done: done}
	t.Cleanup(s.stop)
	return s
}

// deviceConn is a test uploader: one connection, sequential exchanges.
type deviceConn struct {
	t    *testing.T
	conn net.Conn
	br   *bufio.Reader
}

func dial(t *testing.T, addr string) *deviceConn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	d := &deviceConn{t: t, conn: conn, br: bufio.NewReader(conn)}
	t.Cleanup(func() { conn.Close() })
	return d
}

func (d *deviceConn) deadline() {
	d.t.Helper()
	if err := d.conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		d.t.Fatal(err)
	}
}

func (d *deviceConn) register(reg Registration) {
	d.t.Helper()
	d.deadline()
	if err := WriteRegistration(d.conn, reg); err != nil {
		d.t.Fatal(err)
	}
}

func (d *deviceConn) send(b Batch) {
	d.t.Helper()
	d.deadline()
	if err := WriteBatch(d.conn, b); err != nil {
		d.t.Fatal(err)
	}
}

// reply reads one hub reply: an ack or a rejection.
func (d *deviceConn) reply() (Ack, *Error) {
	d.t.Helper()
	d.deadline()
	ack, perr, err := ReadReply(d.br)
	if err != nil {
		d.t.Fatalf("read reply: %v", err)
	}
	return ack, perr
}

// mustAck asserts the reply is an acknowledgment with the wanted fields.
func (d *deviceConn) mustAck(want Ack) Ack {
	d.t.Helper()
	ack, perr := d.reply()
	if perr != nil {
		d.t.Fatalf("reply error = %+v; want ack %+v", perr, want)
	}
	if ack != want {
		d.t.Fatalf("ack = %+v; want %+v", ack, want)
	}
	return ack
}

func (d *deviceConn) wantError(code ErrorCode, sub string) *Error {
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
	return perr
}

// expectClosed asserts the hub closed the connection: the next read fails.
func (d *deviceConn) expectClosed() {
	d.t.Helper()
	d.deadline()
	if _, _, err := ReadReply(d.br); err == nil {
		d.t.Fatal("connection is still open; want the hub to close it")
	}
}

// peerAuth maps each connection's remote address to a device identity, so
// several devices can be simulated deterministically regardless of accept
// order. The hub may authenticate a connection before the test assigns its
// identity, so authentication waits briefly for the assignment; a peer with
// none is refused.
type peerAuth struct {
	mu  sync.Mutex
	ids map[string]string
}

func newPeerAuth() *peerAuth { return &peerAuth{ids: map[string]string{}} }

// assign fixes the identity the hub will report for this connection. The
// hub sees the connection's peer, which is the local address on the device
// side, so key by that.
func (a *peerAuth) assign(conn net.Conn, deviceID string) {
	a.mu.Lock()
	a.ids[conn.LocalAddr().String()] = deviceID
	a.mu.Unlock()
}

func (a *peerAuth) Authenticate(_ context.Context, conn net.Conn) (string, error) {
	key := conn.RemoteAddr().String()
	deadline := time.Now().Add(2 * time.Second)
	for {
		a.mu.Lock()
		id, ok := a.ids[key]
		a.mu.Unlock()
		if ok {
			return id, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("hubsync test: peer %s has no assigned identity", key)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func newTestDB(t *testing.T) *store.DB {
	t.Helper()
	hub, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hub.Close() })
	return hub
}

func mustRegisterVia(t *testing.T, d *deviceConn, deviceID, sourceID, path string) {
	t.Helper()
	d.register(Registration{DeviceID: deviceID, SourceID: sourceID,
		Agent: claudecode.AgentName, Path: path})
	d.mustAck(Ack{})
}

func queryInt(t *testing.T, hub *store.DB, query string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := hub.Writer().QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// dumpEvents renders every events row so two databases can be compared
// byte for byte.
func dumpEvents(t *testing.T, hub *store.DB) []string {
	t.Helper()
	rows, err := hub.Writer().Query(
		`SELECT id, session_id, source_id, source_offset, time_ms, kind, role, tool_name, text, raw_json, metadata,
		        ordinal, duplicate_of
		 FROM events ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, offset int64
		var sid, sourceID, kind, raw, metadata string
		var timeMS, ordinal, duplicateOf sql.NullInt64
		var role, tool, text sql.NullString
		if err := rows.Scan(&id, &sid, &sourceID, &offset, &timeMS, &kind, &role, &tool, &text, &raw, &metadata,
			&ordinal, &duplicateOf); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%d|%s|%s|%d|%+v|%s|%+v|%+v|%q|%s|%s|%+v|%+v",
			id, sid, sourceID, offset, timeMS, kind, role, tool, text.String, raw, metadata, ordinal, duplicateOf))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// dumpSessions renders every sessions row the same way.
func dumpSessions(t *testing.T, hub *store.DB) []string {
	t.Helper()
	rows, err := hub.Writer().Query(
		`SELECT id, agent, device_id, native_id, repo, cwd, branch, model, first_prompt,
		        started_at_ms, last_event_at_ms, event_count, metadata
		 FROM sessions ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, agent, device, native, metadata string
		var count int
		var repo, cwd, branch, model, firstPrompt sql.NullString
		var started, last sql.NullInt64
		if err := rows.Scan(&id, &agent, &device, &native, &repo, &cwd, &branch, &model,
			&firstPrompt, &started, &last, &count, &metadata); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s|%s|%s|%s|%+v|%+v|%+v|%+v|%q|%+v|%+v|%d|%s",
			id, agent, device, native, repo, cwd, branch, model, firstPrompt.String,
			started, last, count, metadata))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestEndpointCommitsFixtureLikeLocalIngestion drives a full device session:
// registration, the fixture batch, then an incremental follow-up. The hub
// normalizes with the same parser as local ingestion — every events and
// sessions row equals what the local write path produces from the same
// bytes — FTS follows, and the acknowledged cursor matches the committed
// one.
func TestEndpointCommitsFixtureLikeLocalIngestion(t *testing.T) {
	db := newTestDB(t)
	s := startEndpoint(t, db, nil, Options{Retry: RetryPolicy{Base: time.Millisecond}})

	recs := fixtureRecords(t)
	batch := fixtureBatch(deviceA, "src-1", recs)

	d := dial(t, s.addr)
	mustRegisterVia(t, d, deviceA, "src-1", "/home/dev/s.jsonl")
	d.send(batch)
	ack := d.mustAck(Ack{Cursor: 6528, Inserted: 16})

	if got := queryInt(t, db, `SELECT cursor_bytes FROM sources WHERE id='src-1'`); got != ack.Cursor {
		t.Fatalf("committed cursor = %d; want the acknowledged %d", got, ack.Cursor)
	}
	if got := queryInt(t, db, `SELECT count(*) FROM events`); got != 16 {
		t.Fatalf("events = %d, want 16", got)
	}
	if got := queryInt(t, db, `SELECT event_count FROM sessions`); got != 16 {
		t.Fatalf("event_count = %d, want 16", got)
	}

	// The incremental follow-up record extends the session on a second
	// batch, like an agent appending while sync runs.
	next := rawLine("00000000-0000-4000-8000-0000000000a1", "one more prompt")
	d.send(Batch{DeviceID: deviceA, SourceID: "src-1",
		Start: 6528, End: 6528 + int64(len(next)) + 1,
		Records: []Record{{Offset: 6528, JSON: next}}})
	d.mustAck(Ack{Cursor: 6529 + int64(len(next)), Inserted: 1})

	// FTS carries the committed text.
	if got := queryInt(t, db,
		`SELECT count(*) FROM events_fts WHERE events_fts MATCH 'prime'`); got == 0 {
		t.Fatal("FTS holds no match for the fixture text")
	}

	// Reference run: the same bytes through the local write path.
	local := newTestDB(t)
	if err := local.RegisterSource(context.Background(), "src-1", deviceA,
		claudecode.AgentName, "/home/dev/s.jsonl"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "claude-code", "main-session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	parsed, perr := claudecode.Parse(data, 0, claudecode.Options{SourceScope: "src-1"})
	if perr != nil {
		t.Fatal(perr)
	}
	if _, err := local.Ingest(context.Background(), store.Batch{
		SourceID: "src-1", DeviceID: deviceA,
		Events: events.FromClaudeCodeRecords(parsed.Records)}); err != nil {
		t.Fatal(err)
	}
	tail := append(append([]byte{}, next...), '\n')
	parsedTail, perr := claudecode.Parse(tail, 6528, claudecode.Options{SourceScope: "src-1"})
	if perr != nil {
		t.Fatal(perr)
	}
	if _, err := local.Ingest(context.Background(), store.Batch{
		SourceID: "src-1", DeviceID: deviceA,
		Events: events.FromClaudeCodeRecords(parsedTail.Records)}); err != nil {
		t.Fatal(err)
	}

	gotEvents, wantEvents := dumpEvents(t, db), dumpEvents(t, local)
	if len(gotEvents) != len(wantEvents) {
		t.Fatalf("events via endpoint = %d rows; local = %d", len(gotEvents), len(wantEvents))
	}
	for i := range gotEvents {
		if gotEvents[i] != wantEvents[i] {
			t.Fatalf("events row %d differs:\n endpoint: %s\n local:    %s", i, gotEvents[i], wantEvents[i])
		}
	}
	gotSessions, wantSessions := dumpSessions(t, db), dumpSessions(t, local)
	if len(gotSessions) != len(wantSessions) || len(gotSessions) != 1 {
		t.Fatalf("sessions via endpoint = %d; local = %d; want 1", len(gotSessions), len(wantSessions))
	}
	if gotSessions[0] != wantSessions[0] {
		t.Fatalf("sessions row differs:\n endpoint: %s\n local:    %s", gotSessions[0], wantSessions[0])
	}
}

// TestEndpointCommitsCodexLikeLocalIngestion uploads a Codex thread the way
// the uploader does — the main rollout in two batches, then its resumed page
// as another source — and compares the hub's rows with local ingestion of
// the same bytes. The second batch starts past offset 0, so the hub must take
// the thread from the source's committed first record; the page's replayed
// ordinals become duplicates.
func TestEndpointCommitsCodexLikeLocalIngestion(t *testing.T) {
	db := newTestDB(t)
	s := startEndpoint(t, db, nil, Options{Retry: RetryPolicy{Base: time.Millisecond}})
	d := dial(t, s.addr)
	register := func(source, path string) {
		d.register(Registration{DeviceID: deviceA, SourceID: source, Agent: codex.AgentName, Path: path})
		d.mustAck(Ack{})
	}

	main := framedFixture(t, "codex", "main-session.jsonl")
	page := framedFixture(t, "codex", "resumed-page.jsonl")
	register("src-main", "/main.jsonl")
	register("src-page", "/page.jsonl")
	d.send(fixtureBatch(deviceA, "src-main", main[:5]))
	d.mustAck(Ack{Cursor: main[4].End(), Inserted: 5})
	// A retry overlapping the committed prefix inserts only the rest.
	d.send(fixtureBatch(deviceA, "src-main", main[3:]))
	d.mustAck(Ack{Cursor: 4780, Inserted: 9})
	d.send(fixtureBatch(deviceA, "src-page", page))
	d.mustAck(Ack{Cursor: 2048, Inserted: 5})

	// A batch past offset 0 of a source with no committed first record
	// cannot name its thread: rejected, nothing stored.
	register("src-gap", "/gap.jsonl")
	d.send(fixtureBatch(deviceA, "src-gap", main[1:3]))
	d.wantError(CodeRejected, "")
	if got := queryInt(t, db, `SELECT count(*) FROM events WHERE source_id='src-gap'`); got != 0 {
		t.Fatalf("gap source stored %d events", got)
	}

	// Reference run: the same bytes through the local write path.
	local := newTestDB(t)
	ctx := context.Background()
	for _, src := range []struct{ id, path, name string }{
		{"src-main", "/main.jsonl", "main-session.jsonl"},
		{"src-page", "/page.jsonl", "resumed-page.jsonl"},
		{"src-gap", "/gap.jsonl", ""},
	} {
		if err := local.RegisterSource(ctx, src.id, deviceA, codex.AgentName, src.path); err != nil {
			t.Fatal(err)
		}
		if src.name == "" {
			continue
		}
		data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "codex", src.name))
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := events.Parse(codex.AgentName, data, 0, events.Options{SourceScope: src.id})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := local.Ingest(ctx, store.Batch{SourceID: src.id, DeviceID: deviceA, Events: parsed.Events}); err != nil {
			t.Fatal(err)
		}
	}
	gotEvents, wantEvents := dumpEvents(t, db), dumpEvents(t, local)
	if len(gotEvents) != len(wantEvents) || len(gotEvents) != 19 {
		t.Fatalf("events via endpoint = %d rows; local = %d; want 19", len(gotEvents), len(wantEvents))
	}
	for i := range gotEvents {
		if gotEvents[i] != wantEvents[i] {
			t.Fatalf("events row %d differs:\n endpoint: %s\n local:    %s", i, gotEvents[i], wantEvents[i])
		}
	}
	gotSessions, wantSessions := dumpSessions(t, db), dumpSessions(t, local)
	if len(gotSessions) != 1 || len(wantSessions) != 1 || gotSessions[0] != wantSessions[0] {
		t.Fatalf("sessions differ:\n endpoint: %v\n local:    %v", gotSessions, wantSessions)
	}
	if got := queryInt(t, db, `SELECT count(*) FROM events WHERE duplicate_of IS NOT NULL`); got != 2 {
		t.Fatalf("duplicates = %d, want the page's 2 replayed records", got)
	}

	// A malformed Codex line commits the valid prefix and stops there.
	register("src-bad", "/bad.jsonl")
	bad := framedFixture(t, "codex", "malformed-line.jsonl")
	d.send(fixtureBatch(deviceA, "src-bad", bad))
	ack, _ := d.reply()
	if !ack.Stopped || ack.StopOffset != 502 || ack.Cursor != 502 || ack.Inserted != 2 {
		t.Fatalf("malformed ack = %+v", ack)
	}
}

// TestAckFollowsDurableCommit proves the acknowledged cursor survives
// closing the endpoint and its database handle: what the device may
// checkpoint was on disk before the acknowledgment left the hub.
func TestAckFollowsDurableCommit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hub.db")
	db, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	s := startEndpoint(t, db, nil, Options{Retry: RetryPolicy{Base: time.Millisecond}})

	recs := fixtureRecords(t)
	d := dial(t, s.addr)
	mustRegisterVia(t, d, deviceA, "src-1", "/p/s.jsonl")
	d.send(fixtureBatch(deviceA, "src-1", recs))
	ack := d.mustAck(Ack{Cursor: 6528, Inserted: 16})

	// Close the device, the endpoint, and the database handle — a hub
	// restart — then reopen from disk.
	d.conn.Close()
	s.stop()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var cursor int64
	var count int
	if err := reopened.Writer().QueryRow(
		`SELECT cursor_bytes, (SELECT count(*) FROM events) FROM sources WHERE id='src-1'`).
		Scan(&cursor, &count); err != nil {
		t.Fatal(err)
	}
	if cursor != ack.Cursor || count != 16 {
		t.Fatalf("after reopen: cursor = %d, events = %d; want %d and 16", cursor, count, ack.Cursor)
	}
}

// TestReplayAndOverlap covers lost acknowledgments: a fully repeated batch
// returns the existing cursor without changing aggregates, and an
// overlapping retry inserts only its new suffix.
func TestReplayAndOverlap(t *testing.T) {
	db := newTestDB(t)
	s := startEndpoint(t, db, nil, Options{Retry: RetryPolicy{Base: time.Millisecond}})
	recs := fixtureRecords(t)

	d := dial(t, s.addr)
	mustRegisterVia(t, d, deviceA, "src-1", "/p/s.jsonl")
	d.send(fixtureBatch(deviceA, "src-1", recs))
	d.mustAck(Ack{Cursor: 6528, Inserted: 16})

	// Full replay after a lost acknowledgment: nothing inserts, nothing
	// re-aggregates, same cursor.
	d.send(fixtureBatch(deviceA, "src-1", recs))
	d.mustAck(Ack{Cursor: 6528, Inserted: 0})
	if got := queryInt(t, db, `SELECT event_count FROM sessions`); got != 16 {
		t.Fatalf("event_count after replay = %d, want 16", got)
	}
	if got := queryInt(t, db,
		`SELECT count(*) FROM session_token_usage`); got != 2 {
		t.Fatalf("usage rows after replay = %d, want 2", got)
	}

	// Overlapping retry: the whole first batch plus one fresh record.
	next := rawLine("00000000-0000-4000-8000-0000000000a1", "overlap tail")
	extended := append(append([]Record{}, recs...),
		Record{Offset: 6528, JSON: next})
	d.send(Batch{DeviceID: deviceA, SourceID: "src-1",
		Start: 0, End: 6528 + int64(len(next)) + 1, Records: extended})
	d.mustAck(Ack{Cursor: 6529 + int64(len(next)), Inserted: 1})
	if got := queryInt(t, db, `SELECT event_count FROM sessions`); got != 17 {
		t.Fatalf("event_count after overlap = %d, want 17", got)
	}

	// A cursor probe with no records reports the cursor and writes nothing.
	d.send(Batch{DeviceID: deviceA, SourceID: "src-1",
		Start: 6529 + int64(len(next)), End: 6529 + int64(len(next))})
	d.mustAck(Ack{Cursor: 6529 + int64(len(next)), Inserted: 0})
}

// TestConflictingRetryRejected proves changed content at a stored offset is
// rejected, the connection stays usable, and the stored history is intact.
func TestConflictingRetryRejected(t *testing.T) {
	db := newTestDB(t)
	s := startEndpoint(t, db, nil, Options{Retry: RetryPolicy{Base: time.Millisecond}})
	recs := fixtureRecords(t)

	d := dial(t, s.addr)
	mustRegisterVia(t, d, deviceA, "src-1", "/p/s.jsonl")
	d.send(fixtureBatch(deviceA, "src-1", recs))
	d.mustAck(Ack{Cursor: 6528, Inserted: 16})

	// Same first record, different bytes; the rest of the batch differs too.
	tampered := append([]Record{}, recs...)
	tampered[0].JSON = []byte(`{"type":"user","sessionId":"00000000-0000-4000-8000-0000000000ff","message":{"role":"user","content":"rewritten"}}`)
	tampered[0].JSON = append([]byte{}, tampered[0].JSON...)
	// Keep boundaries honest: rebuild offsets for the tampered length.
	for i := 1; i < len(tampered); i++ {
		tampered[i].Offset = tampered[i-1].End()
	}
	d.send(Batch{DeviceID: deviceA, SourceID: "src-1",
		Start: 0, End: tampered[len(tampered)-1].End(), Records: tampered})
	d.wantError(CodeRejected, "conflicting retry")

	// The connection stays open and the history is untouched.
	if got := queryInt(t, db, `SELECT count(*) FROM events`); got != 16 {
		t.Fatalf("events after conflict = %d, want 16", got)
	}
	if got := queryInt(t, db, `SELECT cursor_bytes FROM sources WHERE id='src-1'`); got != 6528 {
		t.Fatalf("cursor after conflict = %d, want 6528", got)
	}
	next := rawLine("00000000-0000-4000-8000-0000000000a1", "still works")
	d.send(Batch{DeviceID: deviceA, SourceID: "src-1",
		Start: 6528, End: 6529 + int64(len(next)),
		Records: []Record{{Offset: 6528, JSON: next}}})
	d.mustAck(Ack{Cursor: 6529 + int64(len(next)), Inserted: 1})
}

// TestGapRejected proves bytes are never skipped: a batch starting ahead of
// the stored cursor is refused without any write.
func TestGapRejected(t *testing.T) {
	db := newTestDB(t)
	s := startEndpoint(t, db, nil, Options{Retry: RetryPolicy{Base: time.Millisecond}})
	recs := fixtureRecords(t)

	d := dial(t, s.addr)
	mustRegisterVia(t, d, deviceA, "src-1", "/p/s.jsonl")
	d.send(fixtureBatch(deviceA, "src-1", recs))
	d.mustAck(Ack{Cursor: 6528, Inserted: 16})

	next := rawLine("00000000-0000-4000-8000-0000000000a1", "ahead of the cursor")
	skipping := Batch{DeviceID: deviceA, SourceID: "src-1",
		Start: 6528 + 100, End: 6528 + 100 + int64(len(next)) + 1,
		Records: []Record{{Offset: 6528 + 100, JSON: next}}}
	d.send(skipping)
	d.wantError(CodeRejected, "bytes would be skipped")
	if got := queryInt(t, db, `SELECT cursor_bytes FROM sources WHERE id='src-1'`); got != 6528 {
		t.Fatalf("cursor after gap = %d, want 6528", got)
	}
	if got := queryInt(t, db, `SELECT count(*) FROM events`); got != 16 {
		t.Fatalf("events after gap = %d, want 16", got)
	}

	// A batch whose records are internally non-contiguous is also rejected.
	discontinuous := Batch{DeviceID: deviceA, SourceID: "src-1", Start: 6528, End: 6528 + 2*(int64(len(next))+1),
		Records: []Record{
			{Offset: 6528, JSON: next},
			{Offset: 6528 + int64(len(next)) + 10, JSON: next},
		}}
	d.send(discontinuous)
	d.wantError(CodeRejected, "record 1 starts at offset")

	// The end of the batch must match its last record.
	badEnd := fixtureBatch(deviceA, "src-1", recs)
	badEnd.End += 7
	d.send(badEnd)
	d.wantError(CodeRejected, "last record ends at")
}

// TestOfflineRecoveryDropsAndReplays covers the DropAck window of the TLA+
// model: the hub commits and the acknowledgment is lost with the
// connection. The device reconnects, re-registers (idempotent), replays the
// same batch from its stale checkpoint, and the hub answers with the
// existing cursor and no new events.
func TestOfflineRecoveryDropsAndReplays(t *testing.T) {
	db := newTestDB(t)
	s := startEndpoint(t, db, nil, Options{Retry: RetryPolicy{Base: time.Millisecond}})
	recs := fixtureRecords(t)
	batch := fixtureBatch(deviceA, "src-1", recs)

	d := dial(t, s.addr)
	mustRegisterVia(t, d, deviceA, "src-1", "/p/s.jsonl")
	d.send(batch)
	// Drop the connection without reading the acknowledgment.
	if err := d.conn.Close(); err != nil {
		t.Fatal(err)
	}

	// The commit happened durably even though the device never saw the ack.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if queryInt(t, db, `SELECT cursor_bytes FROM sources WHERE id='src-1'`) == 6528 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("hub did not commit the batch after the connection dropped")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Reconnect and replay from the stale checkpoint.
	d2 := dial(t, s.addr)
	mustRegisterVia(t, d2, deviceA, "src-1", "/p/s.jsonl")
	d2.send(batch)
	d2.mustAck(Ack{Cursor: 6528, Inserted: 0})
	if got := queryInt(t, db, `SELECT event_count FROM sessions`); got != 16 {
		t.Fatalf("event_count after replay = %d, want 16 (no duplicate events)", got)
	}

	// Then the source continues with fresh bytes.
	next := rawLine("00000000-0000-4000-8000-0000000000a1", "after recovery")
	d2.send(Batch{DeviceID: deviceA, SourceID: "src-1",
		Start: 6528, End: 6529 + int64(len(next)),
		Records: []Record{{Offset: 6528, JSON: next}}})
	d2.mustAck(Ack{Cursor: 6529 + int64(len(next)), Inserted: 1})
}

// TestRolledBackTransactionLeavesCursorUnchanged breaks the commit
// mid-transaction with an aborting trigger, the way TestIngest's store tests
// break it with invalid metadata: the transaction rolls back, the device
// learns an internal failure, and neither the cursor nor any event moves.
// The same bytes then commit whole once the failure clears.
func TestRolledBackTransactionLeavesCursorUnchanged(t *testing.T) {
	db := newTestDB(t)
	s := startEndpoint(t, db, nil, Options{Retry: RetryPolicy{Base: time.Millisecond, Attempts: 2}})

	l0 := rawLine("sess-1", "record zero")
	l1 := rawLine("sess-1", "record one")
	l2 := rawLine("sess-1", "record two")
	off1 := int64(len(l0) + 1)
	// The aborting trigger breaks the commit at the second record, after the
	// first inserted cleanly. Trigger bodies take no bound parameters.
	if _, err := db.Writer().Exec(fmt.Sprintf(
		`CREATE TRIGGER rollback_probe BEFORE INSERT ON events
		 WHEN NEW.source_offset = %d
		 BEGIN SELECT RAISE(ABORT, 'probe failure'); END`, off1)); err != nil {
		t.Fatal(err)
	}

	batch := Batch{DeviceID: deviceA, SourceID: "src-1",
		Start: 0, End: int64(len(l0) + 1 + len(l1) + 1 + len(l2) + 1),
		Records: []Record{{Offset: 0, JSON: l0}, {Offset: off1, JSON: l1}, {JSON: l2}}}
	batch.Records[2].Offset = batch.Records[1].End()

	d := dial(t, s.addr)
	mustRegisterVia(t, d, deviceA, "src-1", "/p/s.jsonl")
	d.send(batch)
	d.wantError(CodeInternal, "probe failure")
	if got := queryInt(t, db, `SELECT cursor_bytes FROM sources WHERE id='src-1'`); got != 0 {
		t.Fatalf("cursor after rolled-back commit = %d, want 0", got)
	}
	if got := queryInt(t, db, `SELECT count(*) FROM events`); got != 0 {
		t.Fatalf("events after rolled-back commit = %d, want 0", got)
	}
	if got := queryInt(t, db, `SELECT count(*) FROM sessions WHERE event_count > 0`); got != 0 {
		t.Fatal("a session kept an aggregate from a rolled-back commit")
	}

	// Clearing the failure lets the same bytes commit whole.
	if _, err := db.Writer().Exec(`DROP TRIGGER rollback_probe`); err != nil {
		t.Fatal(err)
	}
	d.send(batch)
	d.mustAck(Ack{Cursor: batch.End, Inserted: 3})
}

// TestMalformedRecordCommitsPrefixAndStops mirrors the local source-stop
// semantics: the valid prefix before an unparseable record commits, the ack
// reports the stop offset and reason, and bytes are never skipped past it.
func TestMalformedRecordCommitsPrefixAndStops(t *testing.T) {
	db := newTestDB(t)
	s := startEndpoint(t, db, nil, Options{Retry: RetryPolicy{Base: time.Millisecond}})

	good := rawLine("sess-1", "commits")
	bad := []byte(`{"type":"user","sessionId":`) // no closing brace
	after := rawLine("sess-1", "valid after the malformed record")
	off1 := int64(len(good) + 1)
	off2 := off1 + int64(len(bad)) + 1
	batch := Batch{DeviceID: deviceA, SourceID: "src-1",
		Start: 0, End: off2 + int64(len(after)) + 1,
		Records: []Record{{Offset: 0, JSON: good}, {Offset: off1, JSON: bad}, {Offset: off2, JSON: after}}}

	d := dial(t, s.addr)
	mustRegisterVia(t, d, deviceA, "src-1", "/p/s.jsonl")
	d.send(batch)
	ack, perr := d.reply()
	if perr != nil {
		t.Fatalf("stopped batch was rejected: %+v", perr)
	}
	if !ack.Stopped || ack.Cursor != off1 || ack.StopOffset != off1 || ack.Inserted != 1 {
		t.Fatalf("stop ack = %+v; want stopped, cursor %d, stop offset %d, 1 insert", ack, off1, off1)
	}
	if !strings.Contains(ack.Stop, fmt.Sprintf("offset %d", off1)) {
		t.Fatalf("stop reason = %q; want offset %d in it", ack.Stop, off1)
	}
	if got := queryInt(t, db, `SELECT cursor_bytes FROM sources WHERE id='src-1'`); got != off1 {
		t.Fatalf("cursor after stop = %d, want %d", got, off1)
	}
	if got := queryInt(t, db, `SELECT count(*) FROM events`); got != 1 {
		t.Fatalf("events after stop = %d, want 1", got)
	}

	// The replayed stop is idempotent, and skipping the malformed bytes is
	// rejected.
	d.send(batch)
	ack, perr = d.reply()
	if perr != nil || !ack.Stopped || ack.Cursor != off1 || ack.StopOffset != off1 || ack.Inserted != 0 {
		t.Fatalf("replayed stop = %+v, %+v; want the same stop with 0 inserts", ack, perr)
	}
	skip := Batch{DeviceID: deviceA, SourceID: "src-1",
		Start: off2, End: off2 + int64(len(after)) + 1,
		Records: []Record{{Offset: off2, JSON: after}}}
	d.send(skip)
	d.wantError(CodeRejected, "bytes would be skipped")

	// A record containing a line terminator lies about its boundaries and is
	// rejected outright.
	embedded := append(append([]byte{}, good...), '\n')
	embedded = append(embedded, []byte(`{"type":"user","sessionId":"x"}`)...)
	d.send(Batch{DeviceID: deviceA, SourceID: "src-1",
		Start: off1, End: off1 + int64(len(embedded)) + 1,
		Records: []Record{{Offset: off1, JSON: embedded}}})
	d.wantError(CodeRejected, "exactly one record")
}

// TestRegistrationLifecycle covers idempotent re-registration, path moves
// and the unsupported agent.
func TestRegistrationLifecycle(t *testing.T) {
	db := newTestDB(t)
	s := startEndpoint(t, db, nil, Options{})

	d := dial(t, s.addr)
	mustRegisterVia(t, d, deviceA, "src-1", "/a.jsonl")
	// Same registration again: idempotent.
	mustRegisterVia(t, d, deviceA, "src-1", "/a.jsonl")
	// A path move is followed, like the local watcher's rename handling.
	d.register(Registration{DeviceID: deviceA, SourceID: "src-1",
		Agent: claudecode.AgentName, Path: "/moved.jsonl"})
	d.mustAck(Ack{})
	var path string
	if err := db.Writer().QueryRow(`SELECT path FROM sources WHERE id='src-1'`).Scan(&path); err != nil || path != "/moved.jsonl" {
		t.Fatalf("path = %q, %v; want /moved.jsonl", path, err)
	}
	// The hub parses what its parsers cover; anything else is explicit.
	d.register(Registration{DeviceID: deviceA, SourceID: "src-2",
		Agent: "opencode", Path: "/c.jsonl"})
	d.wantError(CodeUnsupported, "no parser")
	d.register(Registration{DeviceID: deviceA, SourceID: "src-3",
		Agent: codex.AgentName, Path: "/rollout.jsonl"})
	d.mustAck(Ack{})
}

// TestOwnershipAndIdentity pins the authorization rules the endpoint can
// enforce before pairing exists: frames carry the authenticated identity,
// sources are bound to it, and another device — even an authenticated one —
// can neither claim the source nor upload under it.
func TestOwnershipAndIdentity(t *testing.T) {
	db := newTestDB(t)
	auth := newPeerAuth()
	s := startEndpoint(t, db, auth, Options{})
	recs := fixtureRecords(t)

	a := dial(t, s.addr)
	auth.assign(a.conn, deviceA)
	mustRegisterVia(t, a, deviceA, "src-1", "/a.jsonl")
	a.send(fixtureBatch(deviceA, "src-1", recs))
	a.mustAck(Ack{Cursor: 6528, Inserted: 16})

	// A frame naming another device's identity violates the protocol; the
	// hub closes the connection.
	liar := dial(t, s.addr)
	auth.assign(liar.conn, deviceA)
	liar.register(Registration{DeviceID: deviceB, SourceID: "src-9",
		Agent: claudecode.AgentName, Path: "/lie.jsonl"})
	liar.wantError(CodeAuth, "belongs to")
	liar.expectClosed()

	// An authenticated second device cannot register the first device's
	// source (ownership change) ...
	b := dial(t, s.addr)
	auth.assign(b.conn, deviceB)
	b.register(Registration{DeviceID: deviceB, SourceID: "src-1",
		Agent: claudecode.AgentName, Path: "/a.jsonl"})
	b.wantError(CodeOwnership, "ownership")
	// ... nor upload under it, and cannot batch an unregistered source.
	b.send(fixtureBatch(deviceB, "src-1", recs))
	b.wantError(CodeOwnership, "claims device")
	b.send(fixtureBatch(deviceB, "src-b", recs[:1]))
	b.wantError(CodeUnknownSource, "not registered")

	// The same source for the same device on a new connection still works.
	a2 := dial(t, s.addr)
	auth.assign(a2.conn, deviceA)
	mustRegisterVia(t, a2, deviceA, "src-1", "/a.jsonl")
	a2.send(fixtureBatch(deviceA, "src-1", recs))
	a2.mustAck(Ack{Cursor: 6528, Inserted: 0})
	if got := queryInt(t, db, `SELECT event_count FROM sessions`); got != 16 {
		t.Fatalf("event_count after foreign attempts = %d, want 16", got)
	}
}

// TestUnauthenticatedConnectionClose verifies a peer the authenticator
// refuses learns CodeAuth and gets no further.
func TestUnauthenticatedConnectionClose(t *testing.T) {
	db := newTestDB(t)
	auth := newPeerAuth()
	s := startEndpoint(t, db, auth, Options{})

	d := dial(t, s.addr)
	auth.assign(d.conn, deviceA)
	mustRegisterVia(t, d, deviceA, "src-1", "/a.jsonl")

	refused := dial(t, s.addr) // never assigned an identity
	refused.register(Registration{DeviceID: deviceA, SourceID: "src-2",
		Agent: claudecode.AgentName, Path: "/b.jsonl"})
	refused.wantError(CodeAuth, "no assigned identity")
	refused.expectClosed()
}

// TestBrokenFramesCloseConnection pins which violations end a connection.
func TestBrokenFramesCloseConnection(t *testing.T) {
	db := newTestDB(t)
	s := startEndpoint(t, db, nil, Options{})

	// Unsupported protocol version.
	badVersion := func() []byte {
		var buf bytes.Buffer
		batch := fixtureBatch(deviceA, "src-1", fixtureRecords(t)[:1])
		if err := WriteBatch(&buf, batch); err != nil {
			t.Fatal(err)
		}
		frame := buf.Bytes()
		frame[1] = ProtocolVersion + 1
		return frame
	}()
	d := dial(t, s.addr)
	if _, err := d.conn.Write(badVersion); err != nil {
		t.Fatal(err)
	}
	d.wantError(CodeVersion, "version")
	d.expectClosed()

	// A record above the byte bound: the frame declares its length up front,
	// so the hub refuses before allocating and closes.
	head := []byte{'B', ProtocolVersion}
	head = appendLen16(head, deviceA)
	head = append(head, deviceA...)
	head = appendLen16(head, "src-1")
	head = append(head, "src-1"...)
	head = append(head, 0, 0, 0, 0, 0, 0, 0, 0)  // start 0
	head = append(head, 0, 0, 0, 0, 0, 0, 0, 16) // end 16
	head = append(head, 0, 0, 0, 1)              // count 1
	head = append(head, 0, 0, 0, 0, 0, 0, 0, 0)  // offset 0
	var big [4]byte
	binary.BigEndian.PutUint32(big[:], uint32(MaxRecordBytes+1))
	head = append(head, big[:]...)
	d = dial(t, s.addr)
	if _, err := d.conn.Write(head); err != nil {
		t.Fatal(err)
	}
	d.wantError(CodeTooLarge, "record")
	d.expectClosed()

	// An unknown marker is undecodable.
	d = dial(t, s.addr)
	if _, err := d.conn.Write([]byte{'X', ProtocolVersion}); err != nil {
		t.Fatal(err)
	}
	d.wantError(CodeMalformed, "marker")
	d.expectClosed()

	// A truncated frame body is rejected once the device stops sending: the
	// mid-frame EOF reads as an undecodable frame, which is reported before
	// the connection closes.
	d = dial(t, s.addr)
	if _, err := d.conn.Write([]byte{'B', ProtocolVersion, 0}); err != nil {
		t.Fatal(err)
	}
	if tc, ok := d.conn.(*net.TCPConn); ok {
		if err := tc.CloseWrite(); err != nil {
			t.Fatal(err)
		}
	}
	d.wantError(CodeMalformed, "read frame")
	d.expectClosed()

	// The endpoint still serves the next device afterwards.
	ok := dial(t, s.addr)
	mustRegisterVia(t, ok, deviceA, "src-1", "/a.jsonl")
}

// TestConcurrentSameSourceSerializes runs overlapping batches for one source
// from two connections at once. The one-in-flight gate and the single writer
// keep the outcome consistent whatever the interleaving: every unique
// record is stored exactly once and the cursor ends at the largest commit.
func TestConcurrentSameSourceSerializes(t *testing.T) {
	db := newTestDB(t)
	s := startEndpoint(t, db, nil, Options{Retry: RetryPolicy{Base: time.Millisecond}})

	recs := fixtureRecords(t)
	mustRegisterVia(t, dial(t, s.addr), deviceA, "src-1", "/a.jsonl")

	// conn1 sends records [0, 8), conn2 sends [0, 16) — a full overlap.
	first := fixtureBatch(deviceA, "src-1", recs[:8])
	second := fixtureBatch(deviceA, "src-1", recs)

	for i := 0; i < 3; i++ { // shake the interleavings
		var wg sync.WaitGroup
		results := make([]Ack, 2)
		errs := make([]*Error, 2)
		var readErrs [2]error
		for j, batch := range []Batch{first, second} {
			wg.Add(1)
			go func(j int, batch Batch) {
				defer wg.Done()
				conn, err := net.Dial("tcp", s.addr)
				if err != nil {
					readErrs[j] = err
					return
				}
				defer conn.Close()
				if err := conn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
					readErrs[j] = err
					return
				}
				if err := WriteBatch(conn, batch); err != nil {
					readErrs[j] = err
					return
				}
				results[j], errs[j], readErrs[j] = ReadReply(bufio.NewReader(conn))
			}(j, batch)
		}
		wg.Wait()
		for j := range readErrs {
			if readErrs[j] != nil {
				t.Fatalf("batch %d: read reply: %v", j, readErrs[j])
			}
		}
		for j, perr := range errs {
			if perr != nil {
				t.Fatalf("batch %d rejected: %+v", j, perr)
			}
		}
		// Both acks never claim less than what they carried... the shorter
		// batch may commit first or replay entirely.
		if results[0].Cursor != recs[7].End() && results[0].Cursor != 6528 {
			t.Fatalf("first batch cursor = %d", results[0].Cursor)
		}
		if results[1].Cursor != 6528 {
			t.Fatalf("second batch cursor = %d", results[1].Cursor)
		}
		if got := queryInt(t, db, `SELECT count(*) FROM events`); got != 16 {
			t.Fatalf("events = %d, want 16", got)
		}
		if got := queryInt(t, db, `SELECT event_count FROM sessions`); got != 16 {
			t.Fatalf("event_count = %d, want 16 (each record counted once)", got)
		}
		if got := queryInt(t, db, `SELECT cursor_bytes FROM sources WHERE id='src-1'`); got != 6528 {
			t.Fatalf("cursor = %d, want 6528", got)
		}
	}
}

// TestConnectionBoundRejectsBeyondMaxConns pins the bounded queue: with one
// connection allowed, the second learns CodeBusy and the first keeps
// working.
func TestConnectionBoundRejectsBeyondMaxConns(t *testing.T) {
	db := newTestDB(t)
	s := startEndpoint(t, db, nil, Options{MaxConns: 1, Retry: RetryPolicy{Base: time.Millisecond}})

	// Hold the one slot with a registered, idle connection.
	holder := dial(t, s.addr)
	mustRegisterVia(t, holder, deviceA, "src-1", "/a.jsonl")

	extra := dial(t, s.addr)
	extra.register(Registration{DeviceID: deviceA, SourceID: "src-2",
		Agent: claudecode.AgentName, Path: "/b.jsonl"})
	extra.wantError(CodeBusy, "connections")
	extra.expectClosed()

	// The holder still commits.
	recs := fixtureRecords(t)
	holder.send(fixtureBatch(deviceA, "src-1", recs))
	holder.mustAck(Ack{Cursor: 6528, Inserted: 16})
}

// TestEmptyBatchOnUnregisteredSource pins that an empty batch still needs
// the source to exist, so a probe cannot invent one.
func TestEmptyBatchOnUnregisteredSource(t *testing.T) {
	db := newTestDB(t)
	s := startEndpoint(t, db, nil, Options{})
	d := dial(t, s.addr)
	d.send(Batch{DeviceID: deviceA, SourceID: "ghost", Start: 0, End: 0})
	d.wantError(CodeUnknownSource, "not registered")
}

// TestEndpointCommitsPiLikeLocalIngestion uploads a Pi session the way the
// uploader does — the header batch first, then the rest past offset 0 — and
// compares the hub's rows with local ingestion of the same bytes. The second
// batch has no session field of its own, so the hub must take the session
// from the source's committed first record.
func TestEndpointCommitsPiLikeLocalIngestion(t *testing.T) {
	db := newTestDB(t)
	s := startEndpoint(t, db, nil, Options{Retry: RetryPolicy{Base: time.Millisecond}})
	d := dial(t, s.addr)
	d.register(Registration{DeviceID: deviceA, SourceID: "src-pi", Agent: pi.AgentName, Path: "/session.jsonl"})
	d.mustAck(Ack{})

	recs := framedFixture(t, "pi", "main-session.jsonl")
	d.send(fixtureBatch(deviceA, "src-pi", recs[:4]))
	d.mustAck(Ack{Cursor: recs[3].End(), Inserted: 4})
	d.send(fixtureBatch(deviceA, "src-pi", recs[4:]))
	d.mustAck(Ack{Cursor: 2953, Inserted: 4})

	// Reference run: the same bytes through the local write path.
	local := newTestDB(t)
	ctx := context.Background()
	if err := local.RegisterSource(ctx, "src-pi", deviceA, pi.AgentName, "/session.jsonl"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join("..", "..", "testdata", "pi", "main-session.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := events.Parse(pi.AgentName, data, 0, events.Options{SourceScope: "src-pi"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := local.Ingest(ctx, store.Batch{SourceID: "src-pi", DeviceID: deviceA, Events: parsed.Events}); err != nil {
		t.Fatal(err)
	}
	gotEvents, wantEvents := dumpEvents(t, db), dumpEvents(t, local)
	if len(gotEvents) != len(wantEvents) || len(gotEvents) != 8 {
		t.Fatalf("events via endpoint = %d rows; local = %d; want 8", len(gotEvents), len(wantEvents))
	}
	for i := range gotEvents {
		if gotEvents[i] != wantEvents[i] {
			t.Fatalf("events row %d differs:\n endpoint: %s\n local:    %s", i, gotEvents[i], wantEvents[i])
		}
	}
	gotSessions, wantSessions := dumpSessions(t, db), dumpSessions(t, local)
	if len(gotSessions) != 1 || len(wantSessions) != 1 || gotSessions[0] != wantSessions[0] {
		t.Fatalf("sessions differ:\n endpoint: %v\n local:    %v", gotSessions, wantSessions)
	}
	var input, output int64
	if err := db.Writer().QueryRow(`SELECT sum(input_tokens), sum(output_tokens) FROM session_token_usage`).
		Scan(&input, &output); err != nil || input != 1840+345 || output != 24+41 {
		t.Fatalf("tokens = %d/%d, %v; want %d/%d", input, output, err, 1840+345, 24+41)
	}

	// A malformed Pi entry commits the valid prefix and stops there.
	d.register(Registration{DeviceID: deviceA, SourceID: "src-bad", Agent: pi.AgentName, Path: "/bad.jsonl"})
	d.mustAck(Ack{})
	bad := framedFixture(t, "pi", "malformed-line.jsonl")
	d.send(fixtureBatch(deviceA, "src-bad", bad))
	ack, _ := d.reply()
	if !ack.Stopped || ack.StopOffset != 283 || ack.Cursor != 283 || ack.Inserted != 2 {
		t.Fatalf("malformed ack = %+v", ack)
	}
}
