// The device side of the remote query path: one authenticated hub
// connection carrying strictly sequential query and annotate exchanges, the
// same discipline the uploader applies to registrations and batches. The
// caller owns the connection up to authentication — WriteHello on a fresh
// connection, or Pair when joining — and may interleave upload exchanges:
// every exchange on one connection is sequential whatever its kind.

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

// DefaultRemoteTimeout bounds one query or annotate exchange, including the
// hub's execution time. The engine's own deadline is shorter; this bound
// covers the wire and a hub that went silent mid-reply.
const DefaultRemoteTimeout = 60 * time.Second

// RemoteClient runs bounded queries and annotation batches against one hub
// endpoint over one authenticated connection. It is safe for concurrent
// use: exchanges serialize on the connection, like the uploader's.
type RemoteClient struct {
	conn net.Conn
	br   *bufio.Reader
	mu   sync.Mutex
	// Timeout bounds one exchange. Values <= 0 select DefaultRemoteTimeout.
	Timeout time.Duration
}

// NewRemoteClient wraps one authenticated hub connection. The connection
// stays the caller's to close; Close is provided for symmetry.
func NewRemoteClient(conn net.Conn) *RemoteClient {
	return &RemoteClient{
		conn:    conn,
		br:      bufio.NewReaderSize(conn, 64<<10),
		Timeout: DefaultRemoteTimeout,
	}
}

// Close closes the connection.
func (c *RemoteClient) Close() error { return c.conn.Close() }

// Query runs exactly one read-only statement on the hub and returns its
// bounded result, the same shape the local sql tool returns. A protocol
// rejection — a denied statement, a policy violation, a busy hub — is
// returned as a *Error; use errors.As to read its code.
func (c *RemoteClient) Query(ctx context.Context, query string) (mcpserver.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.exchange(ctx, func(w io.Writer) error { return WriteQuery(w, query) }); err != nil {
		return mcpserver.Result{}, err
	}
	result, perr, err := ReadQueryReply(c.br)
	if err != nil {
		return mcpserver.Result{}, fmt.Errorf("hubsync: remote query: %w", err)
	}
	if perr != nil {
		return mcpserver.Result{}, perr
	}
	return result, nil
}

// Annotate submits one annotation batch — the JSON arguments of the annotate
// tool — and reports the committed counts. The hub derives the writer from
// the authenticated device behind the connection; the request cannot name
// one.
func (c *RemoteClient) Annotate(ctx context.Context, request []byte) (store.AnnotateResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.exchange(ctx, func(w io.Writer) error { return WriteAnnotate(w, request) }); err != nil {
		return store.AnnotateResult{}, err
	}
	result, perr, err := ReadAnnotateReply(c.br)
	if err != nil {
		return store.AnnotateResult{}, fmt.Errorf("hubsync: remote annotate: %w", err)
	}
	if perr != nil {
		return store.AnnotateResult{}, perr
	}
	return result, nil
}

// exchange writes one request frame and reads the hub's reply under the
// exchange timeout. A context cancellation interrupts the wait by expiring
// the connection deadline, mirroring the uploader's exchange.
func (c *RemoteClient) exchange(ctx context.Context, write func(io.Writer) error) error {
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
		return fmt.Errorf("hubsync: remote exchange: %w", err)
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("hubsync: remote exchange: %w", err)
	}
	return nil
}
