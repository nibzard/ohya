package hubsync

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/nibzard/ohya/internal/mcpserver"
	"github.com/nibzard/ohya/internal/store"
)

const DefaultRemoteTimeout = 60 * time.Second

type RemoteClient struct {
	conn    net.Conn
	br      *bufio.Reader
	mu      sync.Mutex
	Timeout time.Duration
}

func NewRemoteClient(conn net.Conn) *RemoteClient {
	return &RemoteClient{
		conn:    conn,
		br:      bufio.NewReaderSize(conn, 64<<10),
		Timeout: DefaultRemoteTimeout,
	}
}

func (c *RemoteClient) Close() error { return c.conn.Close() }

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

	bw := bufio.NewWriterSize(c.conn, 64<<10)
	if err := write(bw); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	if err := bw.Flush(); err != nil {
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
