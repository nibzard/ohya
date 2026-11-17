package hubsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nibzard/ohya/internal/mcpserver"
	"github.com/nibzard/ohya/internal/observe"
	"github.com/nibzard/ohya/internal/observe/observertest"
)

func startUsageSessionHub(t *testing.T, engineOpts mcpserver.EngineOptions) *server {
	t.Helper()
	db := newTestDB(t)
	readers, err := db.Reader()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { readers.Close() })
	for _, q := range []string{
		`INSERT INTO sessions(id,agent,device_id,native_id,first_prompt)
		 VALUES('claude:dev-b:sess-1','claude','` + deviceB + `','sess-1','Probe')`,
		`INSERT INTO sources(id,device_id,agent,path) VALUES('src-b','` + deviceB + `','claude','/repo/b.jsonl')`,
		`INSERT INTO events(id,session_id,source_id,source_offset,kind,raw_json,text)
		 VALUES(1,'claude:dev-b:sess-1','src-b',0,'message','{}','hello usage world'),
		       (2,'claude:dev-b:sess-1','src-b',100,'message','{}','second usage record'),
		       (3,'claude:dev-b:sess-1','src-b',200,'message','{}','third usage record'),
		       (4,'claude:dev-b:sess-1','src-b',300,'message','{}','fourth usage record'),
		       (5,'claude:dev-b:sess-1','src-b',400,'message','{}','fifth usage record')`,
	} {
		if _, err := db.Writer().Exec(q); err != nil {
			t.Fatalf("seed %q: %v", q, err)
		}
	}
	engine := mcpserver.NewEngine(readers, db.Writer(), engineOpts)
	return startEndpoint(t, db, &helloAuth{secret: "cred-1", device: deviceB}, Options{Remote: engine})
}

func recordingSession(t *testing.T, s *server, sink *remoteUsageSink, observer *observertest.Recorder) *RemoteSession {
	t.Helper()
	session := NewRemoteSession(
		func(ctx context.Context) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", s.addr)
		}, "cred-1",
		RemoteSessionOptions{DeviceID: deviceB, Usage: sink, Observer: observer})
	t.Cleanup(func() { session.Close() })
	return session
}

func clientContext() context.Context {
	return mcpserver.WithOrigin(context.Background(), mcpserver.Origin{
		Client:        "claude-code",
		ClientVersion: "2.1.280",
		Path:          observe.UsagePathLocal,
		SessionHint:   "sess-7",
	})
}

func TestRemoteSessionRecordsClientOutcome(t *testing.T) {
	sink := &remoteUsageSink{}
	observer := observertest.New()
	s := startUsageSessionHub(t, mcpserver.EngineOptions{})
	session := recordingSession(t, s, sink, observer)
	ctx := clientContext()

	result, err := session.Query(ctx, `SELECT count(*) FROM events`)
	if err != nil {
		t.Fatal(err)
	}
	if result.RowCount != 1 {
		t.Fatalf("query result = %+v", result)
	}
	records := sink.all()
	if len(records) != 1 {
		t.Fatalf("device-side records = %d, want 1", len(records))
	}
	record := records[0]
	if record.Tool != observe.UsageToolSQL || record.Outcome != observe.UsageOutcomeOK {
		t.Fatalf("tool/outcome = %s/%s", record.Tool, record.Outcome)
	}
	if record.Path != observe.UsagePathRemote {
		t.Fatalf("path = %s, want remote", record.Path)
	}
	if record.Client != observe.UsageClientClaudeCode || record.ClientVersion != "2.1" {
		t.Fatalf("client/version = %s/%s, want claude-code/2.1", record.Client, record.ClientVersion)
	}
	if record.DeviceID != deviceB {
		t.Fatalf("device_id = %s, want %s", record.DeviceID, deviceB)
	}
	if record.SessionHint != "sess-7" {
		t.Fatalf("session_hint = %s", record.SessionHint)
	}
	if record.Rows != 1 {
		t.Fatalf("rows = %d, want the client-observed row count 1", record.Rows)
	}
	if record.PhaseMS.TotalMS <= 0 {
		t.Fatalf("total_ms = %v, want the client-observed call time", record.PhaseMS.TotalMS)
	}
	if record.PhaseMS.ReaderWaitMS != 0 || record.PhaseMS.FirstRowMS != 0 || record.PhaseMS.IterationMS != 0 {
		t.Fatalf("device-side record carries hub phases: %+v", record.PhaseMS)
	}
	if len(record.Reads) != 0 || len(record.Functions) != 0 || record.FTS {
		t.Fatalf("device-side record carries hub authorizer data: %v %v %v", record.Reads, record.Functions, record.FTS)
	}
	if err := record.Validate(); err != nil {
		t.Fatalf("record invalid: %v", err)
	}

	if observer.Counter("sql_requests", "sql", "ok") != 1 {
		t.Fatalf("sql_requests(sql,ok) = %d", observer.Counter("sql_requests", "sql", "ok"))
	}
	if observer.DurationCount("sql_total_duration") != 1 {
		t.Fatalf("sql_total_duration samples = %d", observer.DurationCount("sql_total_duration"))
	}
	if observer.Counter("sql_rows") != 1 {
		t.Fatalf("sql_rows = %d", observer.Counter("sql_rows"))
	}
	if observer.UnknownAddCalls() != 0 {
		t.Fatalf("unknown Add calls = %d", observer.UnknownAddCalls())
	}
}

func TestRemoteSessionRecordsAnnotateOutcome(t *testing.T) {
	sink := &remoteUsageSink{}
	observer := observertest.New()
	s := startUsageSessionHub(t, mcpserver.EngineOptions{})
	session := recordingSession(t, s, sink, observer)
	ctx := clientContext()

	if _, err := session.AnnotateAs(ctx, "spoofed", []byte(
		`{"operations":[{"op":"upsert","event_id":1,"namespace":"usage","key":"probe","value":1}]}`)); err != nil {
		t.Fatal(err)
	}
	records := sink.all()
	if len(records) != 1 {
		t.Fatalf("device-side records = %d, want 1", len(records))
	}
	record := records[0]
	if record.Tool != observe.UsageToolAnnotate || record.Outcome != observe.UsageOutcomeOK {
		t.Fatalf("tool/outcome = %s/%s", record.Tool, record.Outcome)
	}
	if record.Path != observe.UsagePathRemote || record.DeviceID != deviceB {
		t.Fatalf("path/device = %s/%s", record.Path, record.DeviceID)
	}
	if record.Client != observe.UsageClientClaudeCode {
		t.Fatalf("client = %s", record.Client)
	}
	if record.AnnotateOps == nil {
		t.Fatal("annotate record carries no annotate_ops")
	}
	if record.Rows != 0 || record.ResultBytes != 0 {
		t.Fatalf("annotate rows/bytes = %d/%d", record.Rows, record.ResultBytes)
	}
	if err := record.Validate(); err != nil {
		t.Fatalf("record invalid: %v", err)
	}

	_, err := session.AnnotateAs(ctx, "spoofed", []byte(
		`{"operations":[{"op":"upsert","event_id":1,"namespace":"usage","key":"probe","value":1,"writer_id":"attacker"}]}`))
	remoteError(t, err, CodeRejected, "writer_id")
	records = sink.all()
	if len(records) != 2 {
		t.Fatalf("device-side records = %d, want 2", len(records))
	}
	record = records[1]
	if record.Tool != observe.UsageToolAnnotate || record.Outcome != observe.UsageOutcomeRejected {
		t.Fatalf("rejected tool/outcome = %s/%s", record.Tool, record.Outcome)
	}
	if err := record.Validate(); err != nil {
		t.Fatalf("rejected record invalid: %v", err)
	}

	if observer.Counter("sql_requests", "annotate", "ok") != 1 {
		t.Fatalf("sql_requests(annotate,ok) = %d", observer.Counter("sql_requests", "annotate", "ok"))
	}
	if observer.Counter("sql_requests", "annotate", "rejected") != 1 {
		t.Fatalf("sql_requests(annotate,rejected) = %d", observer.Counter("sql_requests", "annotate", "rejected"))
	}
	if observer.Counter("annotate_validation_failures") != 1 {
		t.Fatalf("annotate_validation_failures = %d", observer.Counter("annotate_validation_failures"))
	}
	if observer.UnknownAddCalls() != 0 {
		t.Fatalf("unknown Add calls = %d", observer.UnknownAddCalls())
	}
}

func TestRemoteSessionRecordsTruncation(t *testing.T) {
	sink := &remoteUsageSink{}
	observer := observertest.New()
	s := startUsageSessionHub(t, mcpserver.EngineOptions{MaxRows: 2})
	session := recordingSession(t, s, sink, observer)

	result, err := session.Query(clientContext(), `SELECT id FROM events`)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Truncated || result.TruncationReason != mcpserver.TruncationReasonRows {
		t.Fatalf("truncated result = %+v", result)
	}
	records := sink.all()
	if len(records) != 1 || records[0].Outcome != observe.UsageOutcomeTruncatedRows {
		t.Fatalf("outcome = %+v, want truncated_rows", records)
	}
	if records[0].Rows != 2 {
		t.Fatalf("rows = %d, want 2", records[0].Rows)
	}
	if observer.Counter("sql_truncations", mcpserver.TruncationReasonRows) != 1 {
		t.Fatalf("sql_truncations(row_limit) = %d", observer.Counter("sql_truncations", mcpserver.TruncationReasonRows))
	}
	if observer.Counter("sql_rows") != 2 {
		t.Fatalf("sql_rows = %d", observer.Counter("sql_rows"))
	}
}

func TestRemoteSessionRecordsHubRejection(t *testing.T) {
	sink := &remoteUsageSink{}
	observer := observertest.New()
	s := startUsageSessionHub(t, mcpserver.EngineOptions{})
	session := recordingSession(t, s, sink, observer)

	_, err := session.Query(clientContext(), `DELETE FROM events`)
	remoteError(t, err, CodeRejected, "not authorized")
	records := sink.all()
	if len(records) != 1 || records[0].Outcome != observe.UsageOutcomeRejected {
		t.Fatalf("records = %+v, want one rejected outcome", records)
	}
	if err := records[0].Validate(); err != nil {
		t.Fatalf("record invalid: %v", err)
	}
	if observer.Counter("sql_requests", "sql", "rejected") != 1 {
		t.Fatalf("sql_requests(sql,rejected) = %d", observer.Counter("sql_requests", "sql", "rejected"))
	}
	if observer.Counter("sql_rows") != 0 {
		t.Fatalf("failed call reported rows: %d", observer.Counter("sql_rows"))
	}
}

func TestRemoteSessionRecordsTransportOutcomes(t *testing.T) {
	sink := &remoteUsageSink{}
	observer := observertest.New()
	session := NewRemoteSession(
		func(ctx context.Context) (net.Conn, error) { return nil, errors.New("no route to the hub") },
		"cred-1", RemoteSessionOptions{DeviceID: deviceB, Usage: sink, Observer: observer})
	t.Cleanup(func() { session.Close() })

	_, err := session.Query(clientContext(), `SELECT 1`)
	if err == nil {
		t.Fatal("query without a hub succeeded")
	}
	records := sink.all()
	if len(records) != 1 || records[0].Outcome != observe.UsageOutcomeSQLError {
		t.Fatalf("records = %+v, want one sql_error outcome", records)
	}
	if err := records[0].Validate(); err != nil {
		t.Fatalf("record invalid: %v", err)
	}
	if observer.Counter("sql_requests", "sql", "sql_error") != 1 {
		t.Fatalf("sql_requests(sql,sql_error) = %d", observer.Counter("sql_requests", "sql", "sql_error"))
	}

	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := silent.Accept()
			if err != nil {
				return
			}
			time.Sleep(5 * time.Second)
			conn.Close()
		}
	}()
	t.Cleanup(func() { silent.Close() })
	timed := NewRemoteSession(
		func(ctx context.Context) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", silent.Addr().String())
		}, "cred-1", RemoteSessionOptions{DeviceID: deviceB, Usage: sink, Observer: observer})
	t.Cleanup(func() { timed.Close() })
	ctx, cancel := context.WithTimeout(clientContext(), 300*time.Millisecond)
	defer cancel()
	if _, err := timed.Query(ctx, `SELECT 1`); err == nil {
		t.Fatal("query against a silent hub succeeded")
	}
	records = sink.all()
	if len(records) != 2 || records[1].Outcome != observe.UsageOutcomeTimeout {
		t.Fatalf("records = %+v, want a timeout outcome for the silent hub", records)
	}
	if observer.Counter("sql_requests", "sql", "timeout") != 1 {
		t.Fatalf("sql_requests(sql,timeout) = %d", observer.Counter("sql_requests", "sql", "timeout"))
	}
}

func TestRemoteSessionRecordsUnauthorized(t *testing.T) {
	sink := &remoteUsageSink{}
	db := newTestDB(t)
	readers, err := db.Reader()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { readers.Close() })
	engine := mcpserver.NewEngine(readers, db.Writer(), mcpserver.EngineOptions{WriterID: "hub"})
	s := startEndpoint(t, db, &helloAuth{secret: "cred-1", device: deviceB}, Options{Remote: engine})
	session := NewRemoteSession(
		func(ctx context.Context) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", s.addr)
		}, "wrong-credential",
		RemoteSessionOptions{DeviceID: deviceB, Usage: sink, Observer: observertest.New()})
	t.Cleanup(func() { session.Close() })

	if _, err := session.Query(clientContext(), `SELECT 1`); !errors.Is(err, ErrRemoteUnauthorized) {
		t.Fatalf("err = %v, want ErrRemoteUnauthorized", err)
	}
	records := sink.all()
	if len(records) != 1 || records[0].Outcome != observe.UsageOutcomeRejected {
		t.Fatalf("records = %+v, want one rejected outcome", records)
	}
	if err := records[0].Validate(); err != nil {
		t.Fatalf("record invalid: %v", err)
	}
}

func TestRemoteSessionClientOutcomeHoldsNoQueryText(t *testing.T) {
	const secret = "OHYA-SECRET-device-4b1e93"
	sink := &remoteUsageSink{}
	s := startUsageSessionHub(t, mcpserver.EngineOptions{})
	session := recordingSession(t, s, sink, observertest.New())

	if _, err := session.Query(clientContext(),
		`SELECT count(*) FROM events WHERE text != '`+secret+`'`); err != nil {
		t.Fatal(err)
	}
	records := sink.all()
	if len(records) != 1 {
		t.Fatalf("device-side records = %d, want 1", len(records))
	}
	encoded, err := json.Marshal(records[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("device-side usage record leaks the SQL literal: %s", encoded)
	}
}

func TestRemoteUsageOutcomeClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, observe.UsageOutcomeOK},
		{"canceled", context.Canceled, observe.UsageOutcomeCancelled},
		{"deadline", context.DeadlineExceeded, observe.UsageOutcomeTimeout},
		{"conn deadline", fmt.Errorf("read tcp: %w", os.ErrDeadlineExceeded), observe.UsageOutcomeTimeout},
		{"unauthorized", fmt.Errorf("%w: hubsync: auth: no", ErrRemoteUnauthorized), observe.UsageOutcomeRejected},
		{"too large", fmt.Errorf("%w: query holds 99999 bytes", ErrTooLarge), observe.UsageOutcomeRejected},
		{"busy", &Error{Code: CodeBusy, Message: "busy"}, observe.UsageOutcomeBusy},
		{"rejected", &Error{Code: CodeRejected, Message: "not authorized"}, observe.UsageOutcomeRejected},
		{"wire too large", &Error{Code: CodeTooLarge, Message: "result"}, observe.UsageOutcomeRejected},
		{"internal", &Error{Code: CodeInternal, Message: "query failed"}, observe.UsageOutcomeSQLError},
		{"transport", errors.New("hubsync: remote query: connection reset"), observe.UsageOutcomeSQLError},
	}
	for _, c := range cases {
		if got := remoteUsageOutcome(c.err, mcpserver.Result{}); got != c.want {
			t.Fatalf("%s: outcome = %s, want %s", c.name, got, c.want)
		}
	}
	truncatedRows := mcpserver.Result{Truncated: true, TruncationReason: mcpserver.TruncationReasonRows}
	if got := remoteUsageOutcome(nil, truncatedRows); got != observe.UsageOutcomeTruncatedRows {
		t.Fatalf("truncated rows outcome = %s", got)
	}
	truncatedBytes := mcpserver.Result{Truncated: true, TruncationReason: mcpserver.TruncationReasonBytes}
	if got := remoteUsageOutcome(nil, truncatedBytes); got != observe.UsageOutcomeTruncatedBytes {
		t.Fatalf("truncated bytes outcome = %s", got)
	}
}
