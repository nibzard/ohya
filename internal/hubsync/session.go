// The device-side session behind the remote MCP bridge: one authenticated
// hub connection carrying query and annotate exchanges, replaced once when it
// died — a hub restart, a transport reset — before a call gives up. It
// implements mcpserver.Backend, so the stdio tools of a joined device run
// over the same wire and the same bounds as the hub's local tools; the hub
// derives the annotation writer from the authenticated device, so the
// writerID argument of AnnotateAs is deliberately ignored here.

package hubsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/nibzard/ohya/internal/mcpserver"
	"github.com/nibzard/ohya/internal/store"
)

// ErrRemoteUnauthorized reports that the hub refused this device's
// credential — a revoked device. Retrying cannot help; revocation is final.
var ErrRemoteUnauthorized = errors.New("hubsync: the hub refused this device's credential")

// RemoteSession keeps one authenticated hub connection for the MCP bridge of
// a joined device. It is safe for concurrent use: exchanges serialize on the
// connection, like every other exchange class of the protocol.
type RemoteSession struct {
	dial       func(ctx context.Context) (net.Conn, error)
	credential string
	timeout    time.Duration

	mu     sync.Mutex
	client *RemoteClient
}

// NewRemoteSession builds a session over a dialer — for example the device
// transport bound to the hub address — and the device's pairing credential
// secret. The first call connects; a call after a dead connection replaces
// it. Close releases the connection.
func NewRemoteSession(dial func(ctx context.Context) (net.Conn, error), credential string) *RemoteSession {
	return &RemoteSession{dial: dial, credential: credential, timeout: DefaultRemoteTimeout}
}

// Query runs exactly one read-only statement on the hub and returns its
// bounded result. A transport failure — not a protocol rejection — replaces
// the connection once and retries: a query is read-only, so a retry cannot
// change hub state.
func (s *RemoteSession) Query(ctx context.Context, query string) (mcpserver.Result, error) {
	var result mcpserver.Result
	err := s.call(ctx, func(c *RemoteClient) error {
		var cerr error
		result, cerr = c.Query(ctx, query)
		return cerr
	})
	return result, err
}

// AnnotateAs applies one annotate batch; the hub derives the writer from the
// authenticated device behind the connection, so writerID is ignored. A
// transport failure replaces the connection once and retries: the annotate
// contract makes an identical retry converge to a no-op, so a batch that
// committed before the failure is never applied twice.
func (s *RemoteSession) AnnotateAs(ctx context.Context, _ string, raw json.RawMessage) (store.AnnotateResult, error) {
	var result store.AnnotateResult
	err := s.call(ctx, func(c *RemoteClient) error {
		var cerr error
		result, cerr = c.Annotate(ctx, raw)
		return cerr
	})
	return result, err
}

// Close closes the current connection, if one is open.
func (s *RemoteSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropLocked()
}

// call runs one exchange on the live connection, reconnecting once when the
// connection died. A protocol rejection is returned as-is: it is the hub's
// answer, not a transport failure.
func (s *RemoteSession) call(ctx context.Context, exchange func(*RemoteClient) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client == nil {
		if err := s.connectLocked(ctx); err != nil {
			return err
		}
	}
	err := exchange(s.client)
	if err == nil {
		return nil
	}
	var perr *Error
	if errors.As(err, &perr) {
		if perr.Code == CodeAuth {
			return fmt.Errorf("%w: %v", ErrRemoteUnauthorized, perr)
		}
		return err // the hub's answer, not a transport failure
	}
	// The connection broke mid-exchange: replace it and try exactly once
	// more on a fresh, authenticated connection.
	_ = s.dropLocked()
	if err := s.connectLocked(ctx); err != nil {
		return err
	}
	return exchange(s.client)
}

// connectLocked dials and authenticates one connection. A refused credential
// is terminal: revocation is final for a device identity.
func (s *RemoteSession) connectLocked(ctx context.Context) error {
	dctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	conn, err := s.dial(dctx)
	if err != nil {
		return fmt.Errorf("hubsync: remote session dial: %w", err)
	}
	if err := WriteHello(conn, s.credential); err != nil {
		conn.Close()
		return fmt.Errorf("hubsync: remote session hello: %w", err)
	}
	s.client = NewRemoteClient(conn)
	s.client.Timeout = s.timeout
	return nil
}

// dropLocked closes and forgets the current connection.
func (s *RemoteSession) dropLocked() error {
	if s.client == nil {
		return nil
	}
	err := s.client.Close()
	s.client = nil
	return err
}
