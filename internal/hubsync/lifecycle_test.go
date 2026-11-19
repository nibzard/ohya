package hubsync

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

type raceRevokeAuth struct {
	device       string
	authCalls    atomic.Int32
	watchCalls   atomic.Int32
	revokeOnAuth bool
	revoked      atomic.Bool
}

func (a *raceRevokeAuth) Authenticate(context.Context, net.Conn) (string, error) {
	a.authCalls.Add(1)
	if a.revokeOnAuth {
		a.revoked.Store(true)
	}
	return a.device, nil
}

func (a *raceRevokeAuth) Authorize(context.Context, string) error {
	if a.revoked.Load() {
		return errors.New("revoked")
	}
	return nil
}

func (a *raceRevokeAuth) WatchRevocations(ctx context.Context, fn func(string)) error {
	if a.watchCalls.Add(1) == 1 {
		return errors.New("transient watcher startup error")
	}
	<-ctx.Done()
	return nil
}

func TestEndpointRejectsRevocationBetweenAuthAndBindAndRetriesWatcher(t *testing.T) {
	db := newTestDB(t)
	auth := &raceRevokeAuth{device: deviceA, revokeOnAuth: true}
	s := startEndpoint(t, db, auth, Options{})

	deadline := time.Now().Add(2 * time.Second)
	for auth.watchCalls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if auth.watchCalls.Load() < 2 {
		t.Fatalf("WatchRevocations calls = %d, want retry after transient error", auth.watchCalls.Load())
	}

	d := dial(t, s.addr)
	_ = d.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, perr, err := ReadReply(d.br)
	if err != nil {
		t.Fatalf("ReadReply: %v", err)
	}
	if perr == nil || perr.Code != CodeAuth {
		t.Fatalf("reply = %+v, want CodeAuth for revocation between Authenticate and bind", perr)
	}
}

func TestEndpointSourceGateCancelsWithContext(t *testing.T) {
	var gates sourceGates
	release, err := gates.acquire(context.Background(), "src-1")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := gates.acquire(ctx, "src-1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("acquire on held gate = %v, want context.DeadlineExceeded", err)
	}
}

func TestEndpointTrackRefusesAfterClose(t *testing.T) {
	db := newTestDB(t)
	ep, err := NewEndpoint(db, StaticAuthenticator(deviceA), Options{})
	if err != nil {
		t.Fatal(err)
	}
	ep.closeAll()
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if ep.track(&connSession{ctx: ctx, conn: c1, cancel: cancel}) {
		t.Fatal("track returned true after closeAll")
	}
}

type errAuth struct {
	err error
}

func (a errAuth) Authenticate(context.Context, net.Conn) (string, error) {
	return "", a.err
}

func TestEndpointClassifiesAuthProtocolErrors(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want ErrorCode
	}{
		{"version", ErrVersion, CodeVersion},
		{"too large", ErrTooLarge, CodeTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := newTestDB(t)
			s := startEndpoint(t, db, errAuth{err: tc.err}, Options{})
			d := dial(t, s.addr)
			_ = d.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, perr, err := ReadReply(d.br)
			if err != nil {
				t.Fatalf("ReadReply: %v", err)
			}
			if perr == nil || perr.Code != tc.want {
				t.Fatalf("reply = %+v, want %q", perr, tc.want)
			}
		})
	}
}

func TestEndpointIdleTimeoutReleasesSlot(t *testing.T) {
	db := newTestDB(t)
	s := startEndpoint(t, db, StaticAuthenticator(deviceA), Options{
		MaxConns:    1,
		IdleTimeout: 80 * time.Millisecond,
	})
	stalled := dial(t, s.addr)
	_ = stalled
	time.Sleep(180 * time.Millisecond)

	fresh := dial(t, s.addr)
	if err := WriteRegistration(fresh.conn, Registration{
		DeviceID: deviceA, SourceID: "src-idle", Agent: "claude-code", Path: "/a.jsonl",
	}); err != nil {
		t.Fatal(err)
	}
	_ = fresh.conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, perr, err := ReadReply(fresh.br); err != nil || perr != nil {
		t.Fatalf("fresh conn after idle eviction: perr=%v err=%v", perr, err)
	}
}
