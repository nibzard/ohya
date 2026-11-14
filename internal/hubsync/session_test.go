package hubsync

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/nibzard/ohya/internal/mcpserver"
)

// helloAuth authenticates one fixed credential, consuming the hello frame
// exactly as the registry-backed authenticator does, so RemoteSession's
// connect-and-authenticate path runs over the real frame protocol.
type helloAuth struct {
	secret string
	device string
}

func (a *helloAuth) Authenticate(_ context.Context, conn net.Conn) (string, error) {
	hello, _, err := ReadAuthFrame(conn)
	if err != nil {
		return "", err
	}
	if hello == nil || hello.Credential != a.secret {
		return "", fmt.Errorf("hubsync test: unknown credential")
	}
	return a.device, nil
}

// TestRemoteSessionReconnectsAfterHubRestart pins the bridge contract: a
// query rides one authenticated connection; when the hub went away — the
// connection broke mid-life — the next call replaces the connection and
// answers, and a refused credential never reconnects.
func TestRemoteSessionReconnectsAfterHubRestart(t *testing.T) {
	db := newTestDB(t)
	readers, err := db.Reader()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { readers.Close() })
	engine := mcpserver.NewEngine(readers, db.Writer(), mcpserver.EngineOptions{WriterID: "hub"})
	auth := &helloAuth{secret: "cred-1", device: deviceB}

	runHub := func() (string, func()) {
		ep, err := NewEndpoint(db, auth, Options{Remote: engine})
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
		return ln.Addr().String(), func() {
			cancel()
			ln.Close()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("endpoint did not stop")
			}
		}
	}

	addr1, stop1 := runHub()
	addr := addr1
	session := NewRemoteSession(
		func(ctx context.Context) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", addr)
		}, "cred-1")
	t.Cleanup(func() { session.Close() })

	ctx := context.Background()
	res, err := session.Query(ctx, "SELECT count(*) AS n FROM events")
	if err != nil {
		t.Fatalf("first query: %v", err)
	}
	if res.RowCount != 1 || res.Rows[0][0] == nil || fmt.Sprint(res.Rows[0][0]) != "0" {
		t.Fatalf("first query result = %+v; want one row counting zero events", res)
	}

	// The hub restarts under the session: the old connection dies and the
	// endpoint returns on a new port.
	stop1()
	addr2, stop2 := runHub()
	defer stop2()
	addr = addr2

	res, err = session.Query(ctx, "SELECT count(*) AS n FROM events")
	if err != nil {
		t.Fatalf("query after hub restart: %v", err)
	}
	if res.RowCount != 1 || fmt.Sprint(res.Rows[0][0]) != "0" {
		t.Fatalf("post-restart result = %+v; want one row counting zero events", res)
	}

	// An annotate batch rides the same recovered session; the hub derives
	// the writer from the credential. The empty hub holds no events yet, so
	// a missing-event rejection proves the batch reached the engine and was
	// attributed without the client naming a writer.
	_, err = session.AnnotateAs(ctx, "spoofed-writer", []byte(
		`{"operations":[{"op":"upsert","event_id":1,"namespace":"t","key":"k","value":1}]}`))
	if err == nil {
		t.Fatal("annotate against a missing event succeeded")
	}
}

// TestRemoteSessionRefusesWrongCredential pins that a CodeAuth answer is
// terminal: the session reports ErrRemoteUnauthorized instead of silently
// reconnecting with the same refused credential.
func TestRemoteSessionRefusesWrongCredential(t *testing.T) {
	db := newTestDB(t)
	readers, err := db.Reader()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { readers.Close() })
	engine := mcpserver.NewEngine(readers, db.Writer(), mcpserver.EngineOptions{WriterID: "hub"})
	auth := &helloAuth{secret: "cred-1", device: deviceB}
	s := startEndpoint(t, db, auth, Options{Remote: engine})

	session := NewRemoteSession(
		func(ctx context.Context) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", s.addr)
		}, "wrong-credential")
	t.Cleanup(func() { session.Close() })

	// The refused credential is answered at the exchange after the hello;
	// whether the connection reset before or after the frame arrived, the
	// error must classify as unauthorized, never as a retriable transport
	// failure that loops.
	_, err = session.Query(context.Background(), "SELECT 1")
	if !errors.Is(err, ErrRemoteUnauthorized) {
		t.Fatalf("query with a wrong credential err = %v; want ErrRemoteUnauthorized", err)
	}
}
