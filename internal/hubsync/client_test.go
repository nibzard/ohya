package hubsync

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/nibzard/ohya/internal/mcpserver"
)

type deadlineTrackingConn struct {
	net.Conn
	mu        sync.Mutex
	deadlines []time.Time
}

func (c *deadlineTrackingConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadlines = append(c.deadlines, t)
	c.mu.Unlock()
	return c.Conn.SetDeadline(t)
}

func (c *deadlineTrackingConn) recordedDeadlines() []time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]time.Time, len(c.deadlines))
	copy(out, c.deadlines)
	return out
}

func TestRemoteClientRoundTripClearsDeadline(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		rawClient, serverConn := net.Pipe()
		defer rawClient.Close()
		defer serverConn.Close()

		tracked := &deadlineTrackingConn{Conn: rawClient}
		client := NewRemoteClient(tracked)
		client.Timeout = time.Second

		go func() {
			frame, err := ReadFrame(serverConn)
			if err != nil || frame.Query == nil {
				return
			}
			_ = WriteQueryResult(serverConn, mcpserver.Result{})
		}()

		_, err := client.Query(context.Background(), "SELECT 1")
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		deadlines := tracked.recordedDeadlines()
		if len(deadlines) < 2 {
			t.Fatalf("recorded deadlines = %v; want set and clear", deadlines)
		}
		if !deadlines[len(deadlines)-1].IsZero() {
			t.Fatalf("last SetDeadline = %v; want zero time", deadlines[len(deadlines)-1])
		}
	})

	t.Run("canceled", func(t *testing.T) {
		rawClient, serverConn := net.Pipe()
		defer rawClient.Close()
		defer serverConn.Close()

		tracked := &deadlineTrackingConn{Conn: rawClient}
		client := NewRemoteClient(tracked)
		client.Timeout = 5 * time.Second

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			_, _ = ReadFrame(serverConn)
			cancel()
		}()

		_, err := client.Query(ctx, "SELECT 1")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Query err = %v; want context.Canceled", err)
		}
		deadlines := tracked.recordedDeadlines()
		if len(deadlines) < 2 {
			t.Fatalf("recorded deadlines = %v; want set and clear", deadlines)
		}
		if !deadlines[len(deadlines)-1].IsZero() {
			t.Fatalf("last SetDeadline = %v; want zero time", deadlines[len(deadlines)-1])
		}
	})
}
