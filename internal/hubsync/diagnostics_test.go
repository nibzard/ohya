package hubsync

import (
	"bytes"
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nibzard/ohya/internal/observe"
	"github.com/nibzard/ohya/internal/store"
)

type stubDiagnosticsProvider struct {
	fn func(context.Context) (RemoteStatus, error)
}

func (s stubDiagnosticsProvider) RemoteStatus(ctx context.Context) (RemoteStatus, error) {
	return s.fn(ctx)
}

type revocableTestAuth struct {
	mu      sync.Mutex
	dev     string
	revoked bool
}

func (a *revocableTestAuth) Authenticate(_ context.Context, _ net.Conn) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.dev, nil
}

func (a *revocableTestAuth) Authorize(_ context.Context, _ string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.revoked {
		return errors.New("device revoked")
	}
	return nil
}

func (a *revocableTestAuth) WatchRevocations(ctx context.Context, _ func(string)) error {
	<-ctx.Done()
	return nil
}

func startDiagnosticsEndpoint(t *testing.T, auth Authenticator, opts Options) (string, func()) {
	t.Helper()
	hub, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "hub.db"))
	if err != nil {
		t.Fatal(err)
	}
	ep, err := NewEndpoint(hub, auth, opts)
	if err != nil {
		hub.Close()
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		hub.Close()
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = ep.Serve(ctx, ln)
	}()
	cleanup := func() {
		cancel()
		<-done
		hub.Close()
	}
	return ln.Addr().String(), cleanup
}

func TestDiagnosticsFrameRoundTripAndBound(t *testing.T) {
	var reqBuf bytes.Buffer
	if err := WriteDiagnosticsRequest(&reqBuf, DiagnosticsFormatVersion); err != nil {
		t.Fatal(err)
	}
	frame, err := ReadFrame(&reqBuf)
	if err != nil {
		t.Fatal(err)
	}
	if frame.Diagnostics == nil || frame.Diagnostics.FormatVersion != DiagnosticsFormatVersion {
		t.Fatalf("decoded request = %+v", frame.Diagnostics)
	}

	uptime := int64(12345)
	sampled := int64(1763740000000)
	want := RemoteStatus{
		FormatVersion: DiagnosticsFormatVersion,
		Hub: RemoteIdentity{
			Version:  "v0.3.0",
			Revision: "abc1234",
			Dirty:    "false",
			Go:       "go1.27.3",
			OS:       "linux",
			Arch:     "amd64",
			Schema:   8,
			Parsers:  map[string]int{"claude-code": 2, "codex": 2, "pi": 2},
		},
		GeneratedWallMS:       sampled + 100,
		UptimeMS:              &uptime,
		SampledWallMS:         &sampled,
		Roles:                 []string{"serve"},
		StorageFiles:          map[string]int64{"db": 4096, "wal": 512},
		StorageObservedWallMS: sampled,
		Resources:             map[string]observe.Reading{observe.ResourceGoroutines: observe.Supported(12)},
		Limits:                RemoteLimits{MaxConns: 16, MaxBatchBytes: MaxBatchBytes, MaxRecordBytes: MaxRecordBytes},
	}
	var repBuf bytes.Buffer
	if err := WriteDiagnosticsReply(&repBuf, want); err != nil {
		t.Fatal(err)
	}
	got, perr, err := ReadDiagnosticsReply(&repBuf)
	if err != nil || perr != nil {
		t.Fatalf("ReadDiagnosticsReply: perr=%v err=%v", perr, err)
	}
	if got.Hub.Version != "v0.3.0" || got.StorageFiles["db"] != 4096 || got.UptimeMS == nil || *got.UptimeMS != 12345 {
		t.Fatalf("got = %+v", got)
	}

	oversize := RemoteStatus{
		FormatVersion: DiagnosticsFormatVersion,
		Roles:         []string{strings.Repeat("x", MaxDiagnosticsResultBytes+1)},
	}
	if err := WriteDiagnosticsReply(&bytes.Buffer{}, oversize); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversize err = %v; want ErrTooLarge", err)
	}
}

func TestDiagnosticsEndpointSuccessUnsupportedRateLimitAndConcurrency(t *testing.T) {
	t.Run("supported hub answers aggregates", func(t *testing.T) {
		uptime := int64(5000)
		addr, cleanup := startDiagnosticsEndpoint(t, StaticAuthenticator("dev1"), Options{
			Diagnostics: stubDiagnosticsProvider{fn: func(context.Context) (RemoteStatus, error) {
				return RemoteStatus{
					FormatVersion: DiagnosticsFormatVersion,
					Hub:           RemoteIdentity{Version: "v0.3.0", Schema: 8},
					UptimeMS:      &uptime,
					Roles:         []string{"serve"},
					StorageFiles:  map[string]int64{"db": 2048},
				}, nil
			}},
			DiagnosticsInterval: -1,
		})
		defer cleanup()

		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		client := NewRemoteClient(conn)
		defer client.Close()

		got, err := client.Diagnostics(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got.Hub.Version != "v0.3.0" || got.StorageFiles["db"] != 2048 {
			t.Fatalf("status = %+v", got)
		}
	})

	t.Run("hub without diagnostics provider reports unsupported via caps", func(t *testing.T) {
		addr, cleanup := startDiagnosticsEndpoint(t, StaticAuthenticator("dev1"), Options{})
		defer cleanup()

		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		client := NewRemoteClient(conn)
		defer client.Close()

		_, err = client.Diagnostics(context.Background())
		if !errors.Is(err, ErrRemoteStatusUnsupported) {
			t.Fatalf("err = %v; want ErrRemoteStatusUnsupported", err)
		}
	})

	t.Run("per-device rate limit returns CodeBusy", func(t *testing.T) {
		addr, cleanup := startDiagnosticsEndpoint(t, StaticAuthenticator("dev1"), Options{
			Diagnostics: stubDiagnosticsProvider{fn: func(context.Context) (RemoteStatus, error) {
				return RemoteStatus{FormatVersion: DiagnosticsFormatVersion}, nil
			}},
			DiagnosticsInterval: 5 * time.Second,
		})
		defer cleanup()

		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		client := NewRemoteClient(conn)
		defer client.Close()

		if _, err := client.Diagnostics(context.Background()); err != nil {
			t.Fatalf("first call failed: %v", err)
		}
		_, err = client.Diagnostics(context.Background())
		var perr *Error
		if !errors.As(err, &perr) || perr.Code != CodeBusy {
			t.Fatalf("second immediate call err = %v; want CodeBusy", err)
		}
	})

	t.Run("global concurrency cap rejects third simultaneous request", func(t *testing.T) {
		release := make(chan struct{})
		var active atomic.Int32
		entered := make(chan struct{}, 2)

		addr, cleanup := startDiagnosticsEndpoint(t, StaticAuthenticator("dev1"), Options{
			Diagnostics: stubDiagnosticsProvider{fn: func(ctx context.Context) (RemoteStatus, error) {
				if active.Add(1) <= 2 {
					entered <- struct{}{}
				}
				select {
				case <-release:
				case <-ctx.Done():
					return RemoteStatus{}, ctx.Err()
				}
				return RemoteStatus{FormatVersion: DiagnosticsFormatVersion}, nil
			}},
			DiagnosticsInterval: -1,
		})
		defer cleanup()

		var wg sync.WaitGroup
		for range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				conn, err := net.Dial("tcp", addr)
				if err != nil {
					return
				}
				c := NewRemoteClient(conn)
				defer c.Close()
				_, _ = c.Diagnostics(context.Background())
			}()
		}
		<-entered
		<-entered

		conn3, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		c3 := NewRemoteClient(conn3)
		defer c3.Close()
		_, err = c3.Diagnostics(context.Background())
		close(release)
		wg.Wait()

		var perr *Error
		if !errors.As(err, &perr) || perr.Code != CodeBusy {
			t.Fatalf("third concurrent call err = %v; want CodeBusy", err)
		}
	})

	t.Run("revoked device is rejected on diagnostic request", func(t *testing.T) {
		auth := &revocableTestAuth{dev: "dev1"}
		addr, cleanup := startDiagnosticsEndpoint(t, auth, Options{
			Diagnostics: stubDiagnosticsProvider{fn: func(context.Context) (RemoteStatus, error) {
				return RemoteStatus{FormatVersion: DiagnosticsFormatVersion}, nil
			}},
			DiagnosticsInterval: -1,
		})
		defer cleanup()

		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		client := NewRemoteClient(conn)
		defer client.Close()

		if _, err := client.Diagnostics(context.Background()); err != nil {
			t.Fatalf("initial call failed: %v", err)
		}
		auth.mu.Lock()
		auth.revoked = true
		auth.mu.Unlock()

		_, err = client.Diagnostics(context.Background())
		var perr *Error
		if !errors.As(err, &perr) || perr.Code != CodeAuth {
			t.Fatalf("revoked call err = %v; want CodeAuth", err)
		}
	})
}
