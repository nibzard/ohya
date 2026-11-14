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

var ErrRemoteUnauthorized = errors.New("hubsync: the hub refused this device's credential")

type RemoteSession struct {
	dial       func(ctx context.Context) (net.Conn, error)
	credential string
	timeout    time.Duration

	mu     sync.Mutex
	client *RemoteClient
}

func NewRemoteSession(dial func(ctx context.Context) (net.Conn, error), credential string) *RemoteSession {
	return &RemoteSession{dial: dial, credential: credential, timeout: DefaultRemoteTimeout}
}

func (s *RemoteSession) Query(ctx context.Context, query string) (mcpserver.Result, error) {
	var result mcpserver.Result
	err := s.call(ctx, func(c *RemoteClient) error {
		var cerr error
		result, cerr = c.Query(ctx, query)
		return cerr
	})
	return result, err
}

func (s *RemoteSession) AnnotateAs(ctx context.Context, _ string, raw json.RawMessage) (store.AnnotateResult, error) {
	var result store.AnnotateResult
	err := s.call(ctx, func(c *RemoteClient) error {
		var cerr error
		result, cerr = c.Annotate(ctx, raw)
		return cerr
	})
	return result, err
}

func (s *RemoteSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropLocked()
}

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
		return err
	}
	_ = s.dropLocked()
	if err := s.connectLocked(ctx); err != nil {
		return err
	}
	return exchange(s.client)
}

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

func (s *RemoteSession) dropLocked() error {
	if s.client == nil {
		return nil
	}
	err := s.client.Close()
	s.client = nil
	return err
}
