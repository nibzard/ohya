// End-to-end tests of the device-facing remote path: query and annotate
// frames on the sync connection, served through a real mcpserver engine
// over the hub database. They pin the MVP read rule (every paired device
// reads all history), the engine's bounds as they survive the wire, the
// writer derivation from the authenticated identity, and the upload-only
// endpoint's refusal.

package hubsync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/nibzard/ohya/internal/mcpserver"
	"github.com/nibzard/ohya/internal/store"
)

// remoteFixtureHub starts an endpoint with a remote engine over a fresh hub
// database and uploads the main-session fixture as deviceA, so tests start
// from shared history another device can read.
func remoteFixtureHub(t *testing.T, engineOpts mcpserver.EngineOptions) (*server, *store.DB, *mcpserver.Engine) {
	t.Helper()
	db := newTestDB(t)
	readers, err := db.Reader()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { readers.Close() })
	engine := mcpserver.NewEngine(readers, db.Writer(), engineOpts)
	auth := newPeerAuth()
	s := startEndpoint(t, db, auth, Options{Remote: engine})
	a := dial(t, s.addr)
	auth.assign(a.conn, deviceA)
	mustRegisterVia(t, a, deviceA, "src-1", "/a.jsonl")
	a.send(fixtureBatch(deviceA, "src-1", fixtureRecords(t)))
	a.mustAck(Ack{Cursor: 6528, Inserted: 16})
	return s, db, engine
}

// dialDevice dials one connection the peer authenticator maps to deviceID
// and wraps it in a RemoteClient, the way a joined device's bridge would.
func dialDevice(t *testing.T, s *server, auth *peerAuth, deviceID string) *RemoteClient {
	t.Helper()
	conn, err := net.Dial("tcp", s.addr)
	if err != nil {
		t.Fatal(err)
	}
	auth.assign(conn, deviceID)
	client := NewRemoteClient(conn)
	t.Cleanup(func() { client.Close() })
	return client
}

// remoteError asserts client.Query or Annotate failed with the wanted
// protocol code and message text, and returns the rejection.
func remoteError(t *testing.T, err error, code ErrorCode, sub string) *Error {
	t.Helper()
	if err == nil {
		t.Fatalf("remote call succeeded; want error %v", code)
	}
	var perr *Error
	if !errors.As(err, &perr) {
		t.Fatalf("remote error = %v; want a protocol rejection", err)
	}
	if perr.Code != code {
		t.Fatalf("rejection code = %v (%q); want %v", perr.Code, perr.Message, code)
	}
	if sub != "" && !strings.Contains(perr.Message, sub) {
		t.Fatalf("rejection message = %q; want text %q", perr.Message, sub)
	}
	return perr
}

// TestRemoteQueryServesSharedHistory pins the MVP read rule end to end: a
// second paired device — not the one that uploaded — queries the shared
// history, including FTS search, and reads back its own annotation through
// the same remote path.
func TestRemoteQueryServesSharedHistory(t *testing.T) {
	s, db, _ := remoteFixtureHub(t, mcpserver.EngineOptions{})
	auth := s.ep.auth.(*peerAuth)
	b := dialDevice(t, s, auth, deviceB)
	ctx := context.Background()

	res, err := b.Query(ctx, `SELECT count(*) FROM events`)
	if err != nil {
		t.Fatal(err)
	}
	if res.RowCount != 1 || fmt.Sprint(res.Rows) != "[[16]]" || res.Truncated {
		t.Fatalf("remote count = %+v", res)
	}
	if want := []string{"count(*)"}; fmt.Sprint(res.Columns) != fmt.Sprint(want) {
		t.Fatalf("columns = %v", res.Columns)
	}

	// The hub's FTS index answers remote MATCH queries.
	res, err = b.Query(ctx, `SELECT count(*) FROM events_fts WHERE events_fts MATCH 'prime'`)
	if err != nil {
		t.Fatal(err)
	}
	if res.RowCount != 1 || res.Rows[0][0].(float64) == 0 {
		t.Fatalf("remote MATCH = %+v; want hits", res)
	}

	// deviceB annotates; the row names deviceB, read back through the wire.
	if res, err := b.Annotate(ctx, []byte(
		`{"operations":[{"op":"upsert","event_id":1,"namespace":"tags","key":"seen","value":{"by":"b"}}]}`)); err != nil ||
		res != (store.AnnotateResult{Applied: 1}) {
		t.Fatalf("remote annotate = %+v, %v", res, err)
	}
	res, err = b.Query(ctx, `SELECT writer_id FROM event_annotations`)
	if err != nil {
		t.Fatal(err)
	}
	if res.RowCount != 1 || res.Rows[0][0] != deviceB {
		t.Fatalf("annotation writer = %v; want %s", res.Rows, deviceB)
	}
	if got := queryInt(t, db, `SELECT count(*) FROM event_annotations`); got != 1 {
		t.Fatalf("annotations = %d, want 1", got)
	}
}

// TestRemoteQueryBoundsSurviveTheWire proves the engine's limits bound the
// remote path exactly as the stdio tool's: row and byte truncation is
// explicit, the read-only policy denies writes, one statement per call
// holds, and a slow statement is interrupted at the deadline.
func TestRemoteQueryBoundsSurviveTheWire(t *testing.T) {
	s, _, _ := remoteFixtureHub(t, mcpserver.EngineOptions{
		MaxRows: 100, MaxOutputBytes: 2048, Deadline: 300 * time.Millisecond})
	auth := s.ep.auth.(*peerAuth)
	b := dialDevice(t, s, auth, deviceB)
	ctx := context.Background()

	// The row limit cuts an unbounded generator at 100 rows, explicitly.
	res, err := b.Query(ctx,
		`WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM c WHERE n < 200) SELECT n FROM c`)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || res.TruncationReason != "row_limit" || res.RowCount != 100 {
		t.Fatalf("row-limited result = %+v", res)
	}

	// Rows past the byte bound are dropped whole, never split.
	res, err = b.Query(ctx, `SELECT raw_json FROM events ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || res.TruncationReason != "byte_limit" || res.RowCount < 1 {
		t.Fatalf("byte-limited result = %+v", res)
	}
	for i, row := range res.Rows {
		text, ok := row[0].(string)
		if !ok || !strings.HasPrefix(text, `{"`) {
			t.Fatalf("byte-limited row %d = %#v", i, row[0])
		}
	}

	// A single row above the byte bound is dropped, never split: nothing is
	// returned and the truncation says why.
	res, err = b.Query(ctx, `SELECT group_concat(raw_json) FROM events`)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || res.TruncationReason != "byte_limit" || res.RowCount != 0 {
		t.Fatalf("single-wide-row result = %+v", res)
	}

	// The remote path runs the same read-only policy.
	_, err = b.Query(ctx, `DELETE FROM events`)
	remoteError(t, err, CodeRejected, "not authorized")
	_, err = b.Query(ctx, `SELECT 1; SELECT 2`)
	remoteError(t, err, CodeRejected, "exactly one")
	_, err = b.Query(ctx, `   `)
	remoteError(t, err, CodeRejected, "empty")

	// A slow statement is interrupted at the engine deadline.
	_, err = b.Query(ctx, `WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM c) SELECT count(*) FROM c`)
	remoteError(t, err, CodeRejected, "deadline")

	// Rejected statements leave the connection usable.
	res, err = b.Query(ctx, `SELECT count(*) FROM events`)
	if err != nil || res.RowCount != 1 {
		t.Fatalf("query after rejections = %+v, %v", res, err)
	}

	// A query frame above the wire bound is refused before allocation and
	// closes the connection.
	big := dial(t, s.addr)
	auth.assign(big.conn, deviceB)
	head := []byte{'Q', ProtocolVersion, 0, 1, 0, 1} // declares 65537 bytes
	if _, err := big.conn.Write(head); err != nil {
		t.Fatal(err)
	}
	big.wantError(CodeTooLarge, "query")
	big.expectClosed()
}

// TestRemoteQueryResultFrameBound pins the wire's own output bound: an
// engine configured past the frame limit produces a refused result — an
// explicit internal error — never a silently cut payload and never an
// unbounded read on the device.
func TestRemoteQueryResultFrameBound(t *testing.T) {
	s, _, _ := remoteFixtureHub(t, mcpserver.EngineOptions{
		MaxOutputBytes: 8 << 20, Deadline: 30 * time.Second})
	auth := s.ep.auth.(*peerAuth)
	b := dialDevice(t, s, auth, deviceB)

	// 12 rows of ~400 KiB each (hex doubles the blob): the engine's 8 MiB
	// bound admits the full result, the 4 MiB frame bound must refuse it.
	_, err := b.Query(context.Background(),
		`WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM c WHERE n < 12)
		 SELECT n, hex(zeroblob(200000)) AS x FROM c`)
	remoteError(t, err, CodeInternal, "exceeds")
}

// TestRemoteAnnotateDerivesWriter pins the authorization rule of
// tla/Auth.tla on the remote path: the writer is the authenticated device —
// the request cannot name one — each device owns only its own annotations,
// and content rejections leave the connection usable.
func TestRemoteAnnotateDerivesWriter(t *testing.T) {
	s, db, _ := remoteFixtureHub(t, mcpserver.EngineOptions{})
	auth := s.ep.auth.(*peerAuth)
	a := dialDevice(t, s, auth, deviceA)
	b := dialDevice(t, s, auth, deviceB)
	ctx := context.Background()

	// Both devices annotate the same identity; both rows exist, each named
	// by its own authenticated device.
	batch := func(v string) []byte {
		return []byte(fmt.Sprintf(
			`{"operations":[{"op":"upsert","event_id":1,"namespace":"tags","key":"seen","value":{"by":%q}}]}`, v))
	}
	if res, err := b.Annotate(ctx, batch("b")); err != nil || res.Applied != 1 {
		t.Fatalf("deviceB annotate = %+v, %v", res, err)
	}
	if res, err := a.Annotate(ctx, batch("a")); err != nil || res.Applied != 1 {
		t.Fatalf("deviceA annotate = %+v, %v", res, err)
	}
	if got := queryInt(t, db, `SELECT count(*) FROM event_annotations`); got != 2 {
		t.Fatalf("annotations for two writers = %d, want 2", got)
	}
	res, err := a.Query(ctx, `SELECT writer_id FROM event_annotations ORDER BY writer_id`)
	if err != nil || res.RowCount != 2 || res.Rows[0][0] != deviceA || res.Rows[1][0] != deviceB {
		t.Fatalf("writers = %+v, %v", res, err)
	}

	// A device deletes only its own annotation.
	if res, err := b.Annotate(ctx, []byte(
		`{"operations":[{"op":"delete","event_id":1,"namespace":"tags","key":"seen"}]}`)); err != nil ||
		res.Applied != 1 {
		t.Fatalf("deviceB delete = %+v, %v", res, err)
	}
	if got := queryInt(t, db,
		`SELECT count(*) FROM event_annotations WHERE writer_id = '`+deviceA+`'`); got != 1 {
		t.Fatalf("deviceA row after deviceB delete = %d, want 1", got)
	}

	// The request cannot carry a writer identity: strict decode rejects the
	// unknown field and nothing is stored.
	_, err = b.Annotate(ctx, []byte(
		`{"operations":[{"op":"upsert","event_id":1,"namespace":"tags","key":"spoof","value":1,"writer_id":"attacker"}]}`))
	remoteError(t, err, CodeRejected, "writer_id")
	if got := queryInt(t, db, `SELECT count(*) FROM event_annotations WHERE key='spoof'`); got != 0 {
		t.Fatalf("spoofed rows = %d, want 0", got)
	}

	// Content rejections: a missing event refuses the whole batch; an empty
	// batch is accepted with zeros. The connection stays usable.
	_, err = b.Annotate(ctx, []byte(
		`{"operations":[{"op":"upsert","event_id":1,"namespace":"tags","key":"ok","value":1},
		                {"op":"upsert","event_id":999,"namespace":"tags","key":"missing","value":2}]}`))
	remoteError(t, err, CodeRejected, "999")
	if got := queryInt(t, db, `SELECT count(*) FROM event_annotations WHERE key='ok'`); got != 0 {
		t.Fatalf("rejected batch applied rows = %d, want 0", got)
	}
	if res, err := b.Annotate(ctx, []byte(`{"operations":[]}`)); err != nil || res != (store.AnnotateResult{}) {
		t.Fatalf("empty batch = %+v, %v", res, err)
	}
	if res, err := b.Query(ctx, `SELECT count(*) FROM events`); err != nil || res.RowCount != 1 {
		t.Fatalf("query after annotate rejections = %+v, %v", res, err)
	}

	// An annotate frame above the wire bound is refused before allocation.
	big := dial(t, s.addr)
	auth.assign(big.conn, deviceB)
	var head []byte = []byte{'N', ProtocolVersion}
	head = append(head, 0, 16, 0, 1) // declares 1048577 bytes
	if _, err := big.conn.Write(head); err != nil {
		t.Fatal(err)
	}
	big.wantError(CodeTooLarge, "annotate request")
	big.expectClosed()
}

// TestRemoteFramesWithoutEngine pins the upload-only endpoint: query and
// annotate frames are answered with CodeNoRemote and the connection keeps
// serving uploads.
func TestRemoteFramesWithoutEngine(t *testing.T) {
	db := newTestDB(t)
	s := startEndpoint(t, db, nil, Options{})

	d := dial(t, s.addr)
	client := NewRemoteClient(d.conn)
	_, err := client.Query(context.Background(), `SELECT 1`)
	remoteError(t, err, CodeNoRemote, "upload protocol only")
	_, err = client.Annotate(context.Background(), []byte(`{"operations":[]}`))
	remoteError(t, err, CodeNoRemote, "upload protocol only")

	// The connection stays usable for uploads.
	mustRegisterVia(t, d, deviceA, "src-1", "/a.jsonl")
}

// TestRemoteClientSequentialExchanges proves one client connection carries
// interleaved query and annotate exchanges, and that a protocol error from
// one exchange does not desynchronize the next.
func TestRemoteClientSequentialExchanges(t *testing.T) {
	s, _, _ := remoteFixtureHub(t, mcpserver.EngineOptions{})
	auth := s.ep.auth.(*peerAuth)
	b := dialDevice(t, s, auth, deviceB)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if res, err := b.Query(ctx, `SELECT count(*) FROM events`); err != nil || res.RowCount != 1 {
			t.Fatalf("query %d = %+v, %v", i, res, err)
		} else if res.Rows[0][0] != float64(16) {
			t.Fatalf("query %d count = %v", i, res.Rows[0])
		}
		if res, err := b.Annotate(ctx, []byte(fmt.Sprintf(
			`{"operations":[{"op":"upsert","event_id":1,"namespace":"loop","key":"n","value":%d}]}`, i))); err != nil ||
			res.Applied+res.Unchanged != 1 {
			t.Fatalf("annotate %d = %+v, %v", i, res, err)
		}
		_, err := b.Query(ctx, `SELECT no_such_column FROM events`)
		remoteError(t, err, CodeInternal, "no_such_column")
	}
	if res, err := b.Query(ctx,
		`SELECT json_extract(value, '$') FROM event_annotations WHERE namespace='loop'`); err != nil ||
		res.RowCount != 1 || res.Rows[0][0] != float64(4) {
		t.Fatalf("last write wins = %+v, %v", res, err)
	}
}

// TestRemoteResultJSONShape pins the wire shape of a result: the same JSON
// the local sql tool returns, so a bridge can hand it to agents unchanged.
func TestRemoteResultJSONShape(t *testing.T) {
	var buf bytes.Buffer
	want := mcpserver.Result{
		Columns:          []string{"id", "text"},
		Rows:             [][]any{{float64(1), "hello"}, {float64(2), nil}},
		RowCount:         2,
		Truncated:        true,
		TruncationReason: "row_limit",
	}
	if err := WriteQueryResult(&buf, want); err != nil {
		t.Fatal(err)
	}
	got, perr, err := ReadQueryReply(&buf)
	if err != nil || perr != nil {
		t.Fatalf("read query reply = %+v, %+v, %v", got, perr, err)
	}
	wantJSON, _ := json.Marshal(want)
	gotJSON, _ := json.Marshal(got)
	if !bytes.Equal(wantJSON, gotJSON) {
		t.Fatalf("result round trip:\n got %s\nwant %s", gotJSON, wantJSON)
	}

	buf.Reset()
	if err := WriteAnnotateResult(&buf, store.AnnotateResult{Applied: 2, Unchanged: 3}); err != nil {
		t.Fatal(err)
	}
	counts, perr, err := ReadAnnotateReply(&buf)
	if err != nil || perr != nil || counts != (store.AnnotateResult{Applied: 2, Unchanged: 3}) {
		t.Fatalf("annotate result round trip = %+v, %+v, %v", counts, perr, err)
	}
}
