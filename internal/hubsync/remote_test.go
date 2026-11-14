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

	res, err = b.Query(ctx, `SELECT count(*) FROM events_fts WHERE events_fts MATCH 'prime'`)
	if err != nil {
		t.Fatal(err)
	}
	if res.RowCount != 1 || res.Rows[0][0].(float64) == 0 {
		t.Fatalf("remote MATCH = %+v; want hits", res)
	}

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

func TestRemoteQueryBoundsSurviveTheWire(t *testing.T) {
	s, _, _ := remoteFixtureHub(t, mcpserver.EngineOptions{
		MaxRows: 100, MaxOutputBytes: 2048, Deadline: 300 * time.Millisecond})
	auth := s.ep.auth.(*peerAuth)
	b := dialDevice(t, s, auth, deviceB)
	ctx := context.Background()

	res, err := b.Query(ctx,
		`WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM c WHERE n < 200) SELECT n FROM c`)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || res.TruncationReason != "row_limit" || res.RowCount != 100 {
		t.Fatalf("row-limited result = %+v", res)
	}

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

	res, err = b.Query(ctx, `SELECT group_concat(raw_json) FROM events`)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || res.TruncationReason != "byte_limit" || res.RowCount != 0 {
		t.Fatalf("single-wide-row result = %+v", res)
	}

	_, err = b.Query(ctx, `DELETE FROM events`)
	remoteError(t, err, CodeRejected, "not authorized")
	_, err = b.Query(ctx, `SELECT 1; SELECT 2`)
	remoteError(t, err, CodeRejected, "exactly one")
	_, err = b.Query(ctx, `   `)
	remoteError(t, err, CodeRejected, "empty")

	_, err = b.Query(ctx, `WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM c) SELECT count(*) FROM c`)
	remoteError(t, err, CodeRejected, "deadline")

	res, err = b.Query(ctx, `SELECT count(*) FROM events`)
	if err != nil || res.RowCount != 1 {
		t.Fatalf("query after rejections = %+v, %v", res, err)
	}

	big := dial(t, s.addr)
	auth.assign(big.conn, deviceB)
	head := []byte{'Q', ProtocolVersion, 0, 1, 0, 1}
	if _, err := big.conn.Write(head); err != nil {
		t.Fatal(err)
	}
	big.wantError(CodeTooLarge, "query")
	big.expectClosed()
}

func TestRemoteQueryResultFrameBound(t *testing.T) {
	s, _, _ := remoteFixtureHub(t, mcpserver.EngineOptions{
		MaxOutputBytes: 8 << 20, Deadline: 30 * time.Second})
	auth := s.ep.auth.(*peerAuth)
	b := dialDevice(t, s, auth, deviceB)

	_, err := b.Query(context.Background(),
		`WITH RECURSIVE c(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM c WHERE n < 12)
		 SELECT n, hex(zeroblob(200000)) AS x FROM c`)
	remoteError(t, err, CodeInternal, "exceeds")
}

func TestRemoteAnnotateDerivesWriter(t *testing.T) {
	s, db, _ := remoteFixtureHub(t, mcpserver.EngineOptions{})
	auth := s.ep.auth.(*peerAuth)
	a := dialDevice(t, s, auth, deviceA)
	b := dialDevice(t, s, auth, deviceB)
	ctx := context.Background()

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

	if res, err := b.Annotate(ctx, []byte(
		`{"operations":[{"op":"delete","event_id":1,"namespace":"tags","key":"seen"}]}`)); err != nil ||
		res.Applied != 1 {
		t.Fatalf("deviceB delete = %+v, %v", res, err)
	}
	if got := queryInt(t, db,
		`SELECT count(*) FROM event_annotations WHERE writer_id = '`+deviceA+`'`); got != 1 {
		t.Fatalf("deviceA row after deviceB delete = %d, want 1", got)
	}

	_, err = b.Annotate(ctx, []byte(
		`{"operations":[{"op":"upsert","event_id":1,"namespace":"tags","key":"spoof","value":1,"writer_id":"attacker"}]}`))
	remoteError(t, err, CodeRejected, "writer_id")
	if got := queryInt(t, db, `SELECT count(*) FROM event_annotations WHERE key='spoof'`); got != 0 {
		t.Fatalf("spoofed rows = %d, want 0", got)
	}

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

	big := dial(t, s.addr)
	auth.assign(big.conn, deviceB)
	var head []byte = []byte{'N', ProtocolVersion}
	head = append(head, 0, 16, 0, 1)
	if _, err := big.conn.Write(head); err != nil {
		t.Fatal(err)
	}
	big.wantError(CodeTooLarge, "annotate request")
	big.expectClosed()
}

func TestRemoteFramesWithoutEngine(t *testing.T) {
	db := newTestDB(t)
	s := startEndpoint(t, db, nil, Options{})

	d := dial(t, s.addr)
	client := NewRemoteClient(d.conn)
	_, err := client.Query(context.Background(), `SELECT 1`)
	remoteError(t, err, CodeNoRemote, "upload protocol only")
	_, err = client.Annotate(context.Background(), []byte(`{"operations":[]}`))
	remoteError(t, err, CodeNoRemote, "upload protocol only")

	mustRegisterVia(t, d, deviceA, "src-1", "/a.jsonl")
}

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
