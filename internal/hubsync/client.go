package hubsync

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/nibzard/ohya/internal/mcpserver"
	"github.com/nibzard/ohya/internal/store"
)

const DefaultRemoteTimeout = 60 * time.Second

const DefaultKeepaliveInterval = 3 * time.Minute

var ErrKeepaliveUnsupported = errors.New("hubsync: hub does not accept keepalive capability frames")

type RemoteClient struct {
	conn       net.Conn
	br         *bufio.Reader
	bw         *bufio.Writer
	mu         sync.Mutex
	Timeout    time.Duration
	negotiated bool
	caps       Capabilities
}

func NewRemoteClient(conn net.Conn) *RemoteClient {
	return &RemoteClient{
		conn:    conn,
		br:      bufio.NewReaderSize(conn, 64<<10),
		bw:      bufio.NewWriterSize(conn, 64<<10),
		Timeout: DefaultRemoteTimeout,
	}
}

func (c *RemoteClient) Close() error { return c.conn.Close() }

func (c *RemoteClient) Alive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.conn.SetReadDeadline(time.Now().Add(time.Millisecond))
	_, err := c.br.Peek(1)
	_ = c.conn.SetReadDeadline(time.Time{})
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func (c *RemoteClient) Keepalive(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var caps Capabilities
	var perr *Error
	err := c.roundTrip(ctx,
		func(w io.Writer) error { return WriteCapabilities(w, CapRemoteStatus) },
		func(r io.Reader) error {
			var rerr error
			caps, perr, rerr = ReadWelcome(r)
			return rerr
		})
	if err != nil {
		return fmt.Errorf("hubsync: remote keepalive: %w", err)
	}
	if perr != nil {
		if perr.Code == CodeMalformed || perr.Code == CodeVersion {
			c.negotiated = true
			c.caps = 0
			return ErrKeepaliveUnsupported
		}
		return perr
	}
	c.negotiated = true
	c.caps = caps
	return nil
}

func (c *RemoteClient) Query(ctx context.Context, query string) (mcpserver.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var result mcpserver.Result
	var perr *Error
	err := c.roundTrip(ctx,
		func(w io.Writer) error { return WriteQuery(w, query) },
		func(r io.Reader) error {
			var rerr error
			result, perr, rerr = ReadQueryReply(r)
			return rerr
		})
	if err != nil {
		return mcpserver.Result{}, fmt.Errorf("hubsync: remote query: %w", err)
	}
	if perr != nil {
		return mcpserver.Result{}, perr
	}
	return result, nil
}

func (c *RemoteClient) Annotate(ctx context.Context, request []byte) (store.AnnotateResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var result store.AnnotateResult
	var perr *Error
	err := c.roundTrip(ctx,
		func(w io.Writer) error { return WriteAnnotate(w, request) },
		func(r io.Reader) error {
			var rerr error
			result, perr, rerr = ReadAnnotateReply(r)
			return rerr
		})
	if err != nil {
		return store.AnnotateResult{}, fmt.Errorf("hubsync: remote annotate: %w", err)
	}
	if perr != nil {
		return store.AnnotateResult{}, perr
	}
	return result, nil
}

func (c *RemoteClient) Diagnostics(ctx context.Context) (RemoteStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.negotiated {
		var caps Capabilities
		var perr *Error
		err := c.roundTrip(ctx,
			func(w io.Writer) error { return WriteCapabilities(w, CapRemoteStatus) },
			func(r io.Reader) error {
				var rerr error
				caps, perr, rerr = ReadWelcome(r)
				return rerr
			})
		if err != nil {
			return RemoteStatus{}, fmt.Errorf("hubsync: remote status negotiate: %w", err)
		}
		if perr != nil {
			if perr.Code == CodeMalformed {
				c.negotiated = true
				c.caps = 0
				return RemoteStatus{}, ErrRemoteStatusUnsupported
			}
			return RemoteStatus{}, perr
		}
		c.negotiated = true
		c.caps = caps
	}
	if c.caps&CapRemoteStatus == 0 {
		return RemoteStatus{}, ErrRemoteStatusUnsupported
	}
	var status RemoteStatus
	var perr *Error
	err := c.roundTrip(ctx,
		func(w io.Writer) error { return WriteDiagnosticsRequest(w, DiagnosticsFormatVersion) },
		func(r io.Reader) error {
			var rerr error
			status, perr, rerr = ReadDiagnosticsReply(r)
			return rerr
		})
	if err != nil {
		return RemoteStatus{}, fmt.Errorf("hubsync: remote status: %w", err)
	}
	if perr != nil {
		if perr.Code == CodeNoRemote {
			return RemoteStatus{}, ErrRemoteStatusUnsupported
		}
		return RemoteStatus{}, perr
	}
	return status, nil
}

func IsUnsupportedDiagnostics(err error) bool {
	return errors.Is(err, ErrRemoteStatusUnsupported)
}

func (c *RemoteClient) roundTrip(ctx context.Context, write func(io.Writer) error, read func(io.Reader) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultRemoteTimeout
	}
	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = c.conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = c.conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	c.bw.Reset(c.conn)
	if err := write(c.bw); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	if err := c.bw.Flush(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	if err := read(c.br); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	return nil
}
