// The hub sync endpoint: it accepts device connections, authenticates the
// peer, registers sources under that identity, and commits batches through
// the transactional write path, acknowledging only after the durable commit.
// It implements the HubCommit action of tla/Sync.tla — insert only new
// records, count them once, advance the cursor over contiguous bytes — with
// the ownership binding of tla/Auth.tla enforced by the write path.

package hubsync

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/nibzard/ohya/internal/claudecode"
	"github.com/nibzard/ohya/internal/events"
	"github.com/nibzard/ohya/internal/store"
)

// DefaultMaxConns bounds the device connections one endpoint serves at once.
// A connection past the bound receives CodeBusy and closes; the listener
// backlog bounds the rest.
const DefaultMaxConns = 16

// Authenticator maps one accepted connection to the device ID of its
// authenticated peer. Implementations derive the identity from the transport
// (Tailcat WhoIs) and the pairing registry; they never trust a device ID
// carried inside a frame. Pairing and revocation (T018) implement this with
// the hub-owned credential registry.
type Authenticator interface {
	Authenticate(ctx context.Context, conn net.Conn) (deviceID string, err error)
}

// StaticAuthenticator authenticates every connection as one fixed device: a
// single-device trusted hub, or a test double.
type StaticAuthenticator string

// Authenticate reports the fixed identity.
func (a StaticAuthenticator) Authenticate(context.Context, net.Conn) (string, error) {
	return string(a), nil
}

// Options configures one Endpoint. The zero value is usable: every field has
// a default.
type Options struct {
	// MaxConns bounds concurrent device connections. Values <= 0 select
	// DefaultMaxConns.
	MaxConns int
	// Retry bounds retrying transient write-path failures, such as an
	// external process holding the database lock past the busy timeout.
	// Deterministic rejections never retry.
	Retry RetryPolicy
}

// Endpoint serves the sync protocol to devices. Serve it on the listener of
// any transport.Hub; the endpoint itself is transport-independent.
type Endpoint struct {
	db     *store.DB
	auth   Authenticator
	opts   Options
	slots  chan struct{}
	gates  sourceGates
	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
	wg     sync.WaitGroup
}

// NewEndpoint builds an endpoint over the hub database.
func NewEndpoint(db *store.DB, auth Authenticator, opts Options) (*Endpoint, error) {
	if db == nil {
		return nil, fmt.Errorf("hubsync: endpoint needs a hub database")
	}
	if auth == nil {
		return nil, fmt.Errorf("hubsync: endpoint needs an authenticator")
	}
	maxConns := opts.MaxConns
	if maxConns <= 0 {
		maxConns = DefaultMaxConns
	}
	opts.MaxConns = maxConns
	return &Endpoint{
		db:    db,
		auth:  auth,
		opts:  opts,
		slots: make(chan struct{}, maxConns),
		conns: map[net.Conn]struct{}{},
	}, nil
}

// Serve accepts device connections on ln until ctx is canceled or the
// listener closes. Canceling ctx closes every live connection and waits for
// their handlers; batches in flight either commit or leave the cursor
// untouched.
func (e *Endpoint) Serve(ctx context.Context, ln net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		_ = ln.Close()
		e.closeConns()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				e.wg.Wait()
				return nil
			}
			return fmt.Errorf("hubsync: accept: %w", err)
		}
		select {
		case e.slots <- struct{}{}:
		default:
			// The connection bound is full. Tell the device instead of
			// queueing it: it reconnects and retries with backoff.
			_ = WriteError(conn, CodeBusy,
				fmt.Sprintf("hub is serving its limit of %d connections", e.opts.MaxConns))
			_ = conn.Close()
			continue
		}
		e.track(conn)
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			defer e.untrack(conn)
			defer func() { <-e.slots }()
			e.serveConn(ctx, conn)
		}()
	}
}

// serveConn authenticates the peer, then serves one request frame per
// exchange until the device closes, a frame is undecodable, or the endpoint
// shuts down. Frames on one connection are strictly sequential, so a
// connection holds at most one decoded frame in memory.
func (e *Endpoint) serveConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	deviceID, err := e.auth.Authenticate(ctx, conn)
	if err != nil {
		_ = WriteError(conn, CodeAuth, err.Error())
		return
	}
	br := bufio.NewReaderSize(conn, 64<<10)
	for {
		frame, err := ReadFrame(br)
		if err != nil {
			e.replyDecodeError(conn, err)
			return // the stream is not resynchronizable
		}
		var closeConn bool
		switch {
		case frame.Registration != nil:
			closeConn = e.handleRegistration(ctx, conn, deviceID, *frame.Registration)
		case frame.Batch != nil:
			closeConn = e.handleBatch(ctx, conn, deviceID, *frame.Batch)
		}
		if closeConn {
			return
		}
	}
}

// handleRegistration binds one source to the authenticated device. It
// returns true when the connection must close: a frame naming another
// device's identity is a protocol violation.
func (e *Endpoint) handleRegistration(ctx context.Context, conn net.Conn, deviceID string, reg Registration) bool {
	if reg.DeviceID != deviceID {
		_ = WriteError(conn, CodeAuth, fmt.Sprintf(
			"registration names device %s, but the connection belongs to %s", reg.DeviceID, deviceID))
		return true
	}
	if reg.Agent != claudecode.AgentName {
		_ = WriteError(conn, CodeUnsupported,
			fmt.Sprintf("agent %q has no parser at this hub; this hub parses %q", reg.Agent, claudecode.AgentName))
		return false
	}
	err := Retry(ctx, e.opts.Retry, permanentStoreError, func() error {
		return e.db.RegisterSource(ctx, reg.SourceID, deviceID, reg.Agent, reg.Path)
	})
	if err != nil {
		_ = WriteError(conn, storeErrorCode(err), err.Error())
		return false
	}
	_ = WriteAck(conn, Ack{})
	return false
}

// handleBatch validates, normalizes and commits one batch, then acknowledges
// the committed cursor. It returns true when the connection must close.
func (e *Endpoint) handleBatch(ctx context.Context, conn net.Conn, deviceID string, b Batch) bool {
	if b.DeviceID != deviceID {
		_ = WriteError(conn, CodeAuth, fmt.Sprintf(
			"batch names device %s, but the connection belongs to %s", b.DeviceID, deviceID))
		return true
	}
	if err := validateBoundaries(b); err != nil {
		_ = WriteError(conn, CodeRejected, err.Error())
		return false
	}

	// One in-flight batch per source: a second uploader of the same source
	// waits here, and its validation runs against the first commit.
	release := e.gates.acquire(b.SourceID)
	defer release()

	prefix, stop, err := normalize(b)
	if err != nil {
		_ = WriteError(conn, CodeRejected, err.Error())
		return false
	}
	var res store.IngestResult
	err = Retry(ctx, e.opts.Retry, permanentStoreError, func() error {
		var err error
		res, err = e.db.Ingest(ctx, store.Batch{
			SourceID: b.SourceID,
			DeviceID: deviceID,
			Events:   prefix,
		})
		return err
	})
	if err != nil {
		_ = WriteError(conn, storeErrorCode(err), err.Error())
		return false
	}
	ack := Ack{Cursor: res.Cursor, Inserted: int64(res.Inserted)}
	if stop != nil {
		ack.Stopped = true
		ack.StopOffset = stop.Offset
		ack.Stop = stop.Error()
	}
	// The write path has committed durably (WAL, synchronous FULL) before
	// this acknowledgment leaves the process.
	_ = WriteAck(conn, ack)
	return false
}

// validateBoundaries proves the batch's own shape before any parsing: the
// records are contiguous, they start at the batch start, and the last record
// ends exactly at the batch end. An empty batch must be a cursor probe.
func validateBoundaries(b Batch) error {
	if len(b.Records) == 0 {
		if b.Start != b.End {
			return fmt.Errorf("hubsync: batch spans [%d, %d) but carries no records", b.Start, b.End)
		}
		return nil
	}
	if b.Records[0].Offset != b.Start {
		return fmt.Errorf("hubsync: batch starts at %d, but its first record sits at offset %d",
			b.Start, b.Records[0].Offset)
	}
	for i := 1; i < len(b.Records); i++ {
		if b.Records[i].Offset != b.Records[i-1].End() {
			return fmt.Errorf("hubsync: batch record %d starts at offset %d, but record %d ends at %d",
				i, b.Records[i].Offset, i-1, b.Records[i-1].End())
		}
	}
	if last := b.Records[len(b.Records)-1].End(); last != b.End {
		return fmt.Errorf("hubsync: batch ends at %d, but its last record ends at %d", b.End, last)
	}
	return nil
}

// normalize parses the batch's original records with the same parser and
// options local ingestion uses, so hub-side normalization is identical. The
// records before a parse stop become the committable prefix; the stop itself
// is reported to the device, mirroring the local source-stop semantics: the
// valid prefix commits, and the source stops at the offending offset.
func normalize(b Batch) (prefix []store.Event, stop *claudecode.ParseError, err error) {
	opts := claudecode.Options{SourceScope: b.SourceID}
	prefix = make([]store.Event, 0, len(b.Records))
	for i, r := range b.Records {
		line := make([]byte, 0, len(r.JSON)+1)
		line = append(line, r.JSON...)
		line = append(line, '\n')
		res, perr := claudecode.Parse(line, r.Offset, opts)
		var parseStop *claudecode.ParseError
		if perr != nil {
			if errors.As(perr, &parseStop) {
				return prefix, parseStop, nil
			}
			return nil, nil, fmt.Errorf("hubsync: record %d at offset %d: %w", i, r.Offset, perr)
		}
		// The framed record must decode to exactly one record at exactly its
		// framed boundaries; anything else (for example JSON containing a
		// line terminator) means the frame lies about its bytes.
		if len(res.Records) != 1 || res.Records[0].Offset != r.Offset ||
			res.Records[0].End != r.End() || res.Cursor != r.End() {
			return nil, nil, fmt.Errorf(
				"hubsync: record %d at offset %d does not decode to exactly one record spanning [%d, %d)",
				i, r.Offset, r.Offset, r.End())
		}
		prefix = append(prefix, events.FromClaudeCode(res.Records[0]))
	}
	return prefix, nil, nil
}

// permanentStoreError reports which write-path errors retrying cannot fix.
// Everything else — a busy database, a failed commit — leaves no trace and
// may clear on a later attempt.
func permanentStoreError(err error) bool {
	return errors.Is(err, store.ErrBatchRejected) ||
		errors.Is(err, store.ErrSourceOwnership) ||
		errors.Is(err, store.ErrSourceNotRegistered)
}

// storeErrorCode maps a write-path error onto its protocol rejection class.
func storeErrorCode(err error) ErrorCode {
	switch {
	case errors.Is(err, store.ErrSourceOwnership):
		return CodeOwnership
	case errors.Is(err, store.ErrSourceNotRegistered):
		return CodeUnknownSource
	case errors.Is(err, store.ErrBatchRejected):
		return CodeRejected
	default:
		return CodeInternal
	}
}

// replyDecodeError reports why a frame could not be decoded, then leaves the
// closing to the caller.
func (e *Endpoint) replyDecodeError(conn net.Conn, err error) {
	code := CodeMalformed
	switch {
	case errors.Is(err, ErrTooLarge):
		code = CodeTooLarge
	case errors.Is(err, ErrVersion):
		code = CodeVersion
	}
	_ = WriteError(conn, code, err.Error())
}

func (e *Endpoint) track(conn net.Conn) {
	e.mu.Lock()
	e.conns[conn] = struct{}{}
	e.mu.Unlock()
}

func (e *Endpoint) untrack(conn net.Conn) {
	e.mu.Lock()
	delete(e.conns, conn)
	e.mu.Unlock()
}

// closeConns closes every live connection so blocked handlers return when
// the endpoint shuts down.
func (e *Endpoint) closeConns() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return
	}
	e.closed = true
	for conn := range e.conns {
		_ = conn.Close()
	}
}

// sourceGates holds one lock per source, enforcing the protocol's one
// in-flight batch per source across every connection. Entries free themselves
// when the last waiter releases.
type sourceGates struct {
	mu    sync.Mutex
	locks map[string]*sourceGate
}

type sourceGate struct {
	mu   sync.Mutex
	refs int
}

func (g *sourceGates) acquire(sourceID string) func() {
	g.mu.Lock()
	if g.locks == nil {
		g.locks = map[string]*sourceGate{}
	}
	gate := g.locks[sourceID]
	if gate == nil {
		gate = &sourceGate{}
		g.locks[sourceID] = gate
	}
	gate.refs++
	g.mu.Unlock()

	gate.mu.Lock()

	return func() {
		gate.mu.Unlock()
		g.mu.Lock()
		gate.refs--
		if gate.refs == 0 {
			delete(g.locks, sourceID)
		}
		g.mu.Unlock()
	}
}
