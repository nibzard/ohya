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
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/nibzard/ohya/internal/events"
	"github.com/nibzard/ohya/internal/mcpserver"
	"github.com/nibzard/ohya/internal/store"
)

// DefaultMaxConns bounds the device connections one endpoint serves at once.
// A connection past the bound receives CodeBusy and closes; the listener
// backlog bounds the rest.
const DefaultMaxConns = 16

// DefaultAuthTimeout bounds the first frame of a connection — the
// authentication exchange — so a silent stranger cannot hold a connection
// slot indefinitely.
const DefaultAuthTimeout = 30 * time.Second

// Authenticator maps one accepted connection to the device ID of its
// authenticated peer. Implementations derive the identity from the transport
// (Tailcat WhoIs) and the pairing registry; they never trust a device ID
// carried inside a frame. The credential registry (internal/registry)
// implements it with one-time pairing invitations and device credentials.
type Authenticator interface {
	Authenticate(ctx context.Context, conn net.Conn) (deviceID string, err error)
}

// SessionAuthorizer extends Authenticator with the per-request registry
// check and the revocation notification of tla/Auth.tla. When an endpoint's
// authenticator implements it, the endpoint re-checks authorization before
// every request frame and, whenever a device becomes revoked, closes that
// device's live connections and cancels their in-flight requests. Data a
// request already returned before the revocation cannot be recalled.
type SessionAuthorizer interface {
	Authenticator
	// Authorize reports an error unless the device is currently active in
	// the registry. The endpoint applies it to every request frame.
	Authorize(ctx context.Context, deviceID string) error
	// WatchRevocations calls fn with each device as it becomes revoked,
	// until ctx is done. Revocations from any process sharing the registry
	// file are reported.
	WatchRevocations(ctx context.Context, fn func(deviceID string)) error
}

// StaticAuthenticator authenticates every connection as one fixed device: a
// single-device trusted hub, or a test double.
type StaticAuthenticator string

// Authenticate reports the fixed identity.
func (a StaticAuthenticator) Authenticate(context.Context, net.Conn) (string, error) {
	return string(a), nil
}

// RemoteEngine is the hub-side read and annotation surface paired devices
// reach through query and annotate frames on the sync connection. The query
// engine of internal/mcpserver implements it, so the remote path runs under
// the same bounds and the same read-only policy as the stdio tools. The
// endpoint supplies the annotation writer from the authenticated device; a
// request can never name one.
type RemoteEngine interface {
	// Query runs exactly one bounded read-only statement and returns its
	// truncated-to-limits result.
	Query(ctx context.Context, query string) (mcpserver.Result, error)
	// AnnotateAs applies one annotate request's JSON arguments, attributing
	// every operation of the batch to writerID.
	AnnotateAs(ctx context.Context, writerID string, raw json.RawMessage) (store.AnnotateResult, error)
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
	// AuthTimeout bounds the authentication exchange of one connection.
	// Values <= 0 select DefaultAuthTimeout.
	AuthTimeout time.Duration
	// Remote serves the device-facing query and annotate frames through one
	// RemoteEngine, usually an mcpserver.Engine over this hub's database.
	// Nil keeps the endpoint upload-only: query and annotate frames are
	// answered with CodeNoRemote, and the connection stays usable.
	Remote RemoteEngine
}

// Endpoint serves the sync protocol to devices. Serve it on the listener of
// any transport.Hub; the endpoint itself is transport-independent.
type Endpoint struct {
	db     *store.DB
	auth   Authenticator
	authz  SessionAuthorizer
	remote RemoteEngine
	opts   Options
	slots  chan struct{}
	gates  sourceGates
	mu     sync.Mutex
	conns  map[*connSession]struct{}
	byDev  map[string]map[*connSession]struct{}
	closed bool
	wg     sync.WaitGroup
}

// connSession is one served connection: its per-connection context, which
// revocation cancels, and its authenticated device, empty before
// authentication.
type connSession struct {
	conn     net.Conn
	deviceID string
	cancel   context.CancelFunc
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
	if opts.AuthTimeout <= 0 {
		opts.AuthTimeout = DefaultAuthTimeout
	}
	authz, _ := auth.(SessionAuthorizer)
	return &Endpoint{
		db:     db,
		auth:   auth,
		authz:  authz,
		remote: opts.Remote,
		opts:   opts,
		slots:  make(chan struct{}, maxConns),
		conns:  map[*connSession]struct{}{},
		byDev:  map[string]map[*connSession]struct{}{},
	}, nil
}

// Serve accepts device connections on ln until ctx is canceled or the
// listener closes. Canceling ctx closes every live connection and waits for
// their handlers; batches in flight either commit or leave the cursor
// untouched. When the authenticator is a SessionAuthorizer, revocations it
// reports close the revoked device's connections and cancel their in-flight
// requests the same way.
func (e *Endpoint) Serve(ctx context.Context, ln net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		<-ctx.Done()
		_ = ln.Close()
		e.closeAll()
	}()
	if e.authz != nil {
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			// A watch failure leaves the per-request registry check in
			// force; the endpoint keeps serving.
			_ = e.authz.WatchRevocations(ctx, e.closeDevice)
		}()
	}
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
		cctx, ccancel := context.WithCancel(ctx)
		session := &connSession{conn: conn, cancel: ccancel}
		e.track(session)
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			defer e.untrack(session)
			defer func() { <-e.slots }()
			e.serveConn(cctx, session)
		}()
	}
}

// serveConn authenticates the peer, then serves one request frame per
// exchange until the device closes, a frame is undecodable, the endpoint
// shuts down, or the device is revoked. Frames on one connection are strictly
// sequential, so a connection holds at most one decoded frame in memory.
// Every frame re-checks the registry: a revocation lands at the next frame
// at the latest, and closing the connection reaches it sooner.
func (e *Endpoint) serveConn(ctx context.Context, session *connSession) {
	conn := session.conn
	defer conn.Close()
	deviceID, err := e.authenticate(ctx, conn)
	if err != nil {
		_ = WriteError(conn, CodeAuth, err.Error())
		return
	}
	e.bind(session, deviceID)
	br := bufio.NewReaderSize(conn, 64<<10)
	for {
		frame, err := ReadFrame(br)
		if err != nil {
			e.replyDecodeError(conn, err)
			return // the stream is not resynchronizable
		}
		// The registry check gates the request, not the idle wait: a
		// revocation that lands while the connection sits between frames is
		// caught here, as fresh as possible before the frame is processed.
		// One that lands mid-request is caught by cancelling the request
		// (closeDevice); already returned data cannot be recalled.
		if e.authz != nil {
			if err := e.authz.Authorize(ctx, deviceID); err != nil {
				_ = WriteError(conn, CodeAuth, err.Error())
				return // no longer an active device: the connection ends
			}
		}
		var closeConn bool
		switch {
		case frame.Registration != nil:
			closeConn = e.handleRegistration(ctx, conn, deviceID, *frame.Registration)
		case frame.Batch != nil:
			closeConn = e.handleBatch(ctx, conn, deviceID, *frame.Batch)
		case frame.Query != nil:
			closeConn = e.handleQuery(ctx, conn, *frame.Query)
		case frame.Annotate != nil:
			closeConn = e.handleAnnotate(ctx, conn, deviceID, *frame.Annotate)
		}
		if closeConn {
			return
		}
	}
}

// authenticate runs the authenticator under the connection's auth timeout.
// The read deadline bounds a silent peer; it is cleared afterwards so the
// request frames face no deadline of their own.
func (e *Endpoint) authenticate(ctx context.Context, conn net.Conn) (string, error) {
	_ = conn.SetReadDeadline(time.Now().Add(e.opts.AuthTimeout))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	return e.auth.Authenticate(ctx, conn)
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
	if !events.Supported(reg.Agent) {
		_ = WriteError(conn, CodeUnsupported,
			fmt.Sprintf("agent %q has no parser at this hub; this hub parses %q", reg.Agent, events.Agents))
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

	// The registered agent selects the parser, and a source whose records
	// do not name their session (Codex) needs its first record's native ID.
	var st store.SourceState
	err := Retry(ctx, e.opts.Retry, permanentStoreError, func() error {
		var err error
		st, err = e.db.SourceState(ctx, b.SourceID)
		return err
	})
	if err != nil {
		_ = WriteError(conn, storeErrorCode(err), err.Error())
		return false
	}
	prefix, stop, err := normalize(b, st)
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

// handleQuery runs one bounded read-only statement for a paired device.
// Reads carry no ownership state to check (tla/Auth.tla): every paired
// device may read all history, gated by the same per-frame registry check
// as uploads and cancelled the same way on revocation. Every query outcome
// is answerable in band, so the connection never closes here.
func (e *Endpoint) handleQuery(ctx context.Context, conn net.Conn, q Query) bool {
	if e.remote == nil {
		_ = WriteError(conn, CodeNoRemote,
			"this endpoint serves the upload protocol only; query and annotate frames are not enabled")
		return false
	}
	result, err := e.remote.Query(ctx, q.SQL)
	if err != nil {
		_ = WriteError(conn, queryErrorCode(err), err.Error())
		return false
	}
	if err := WriteQueryResult(conn, result); err != nil {
		// The result outran the frame bound or the connection broke. The
		// engine's limits already truncated the result; a bigger frame is a
		// hub configuration error, reported instead of silently cut bytes.
		_ = WriteError(conn, CodeInternal, err.Error())
		return false
	}
	return false
}

// handleAnnotate applies one annotation batch. The writer identity is the
// authenticated device — the request frame carries no writer at all — and
// the batch commits atomically under the annotate tool's limits. A rejected
// request applies nothing, and every outcome is answerable in band, so the
// connection never closes here.
func (e *Endpoint) handleAnnotate(ctx context.Context, conn net.Conn, deviceID string, a Annotate) bool {
	if e.remote == nil {
		_ = WriteError(conn, CodeNoRemote,
			"this endpoint serves the upload protocol only; query and annotate frames are not enabled")
		return false
	}
	result, err := e.remote.AnnotateAs(ctx, deviceID, a.Request)
	if err != nil {
		_ = WriteError(conn, annotateErrorCode(err), err.Error())
		return false
	}
	_ = WriteAnnotateResult(conn, result)
	return false
}

// queryErrorCode maps an engine rejection onto its protocol class: policy
// and caller-input rejections are deterministic (CodeRejected), hub-level
// contention is CodeBusy, and everything else is a hub-side failure a later
// retry may clear.
func queryErrorCode(err error) ErrorCode {
	switch {
	case errors.Is(err, mcpserver.ErrBusy):
		return CodeBusy
	case errors.Is(err, mcpserver.ErrDenied),
		errors.Is(err, mcpserver.ErrMultipleStatements),
		errors.Is(err, mcpserver.ErrEmptyQuery),
		errors.Is(err, mcpserver.ErrDeadline),
		errors.Is(err, context.Canceled),
		errors.Is(err, context.DeadlineExceeded):
		return CodeRejected
	default:
		return CodeInternal
	}
}

// annotateErrorCode maps an annotate failure onto its protocol class: a
// request the tool or the write path rejected on its content (unknown
// fields, limits, missing events) is deterministic, everything else —
// lock contention, a failed commit — is a hub-side failure a later retry
// of the same batch may clear; an identical retry converges to a no-op.
func annotateErrorCode(err error) ErrorCode {
	if errors.Is(err, mcpserver.ErrAnnotateRejected) || errors.Is(err, store.ErrAnnotationRejected) {
		return CodeRejected
	}
	return CodeInternal
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
func normalize(b Batch, st store.SourceState) (prefix []store.Event, stop *events.StopError, err error) {
	// The wire bound is passed explicitly, so normalization cannot drift from
	// it even if a parser default changes; the protocol already refused any
	// record above it before this code runs.
	opts := events.Options{
		SourceScope: b.SourceID, FirstNative: st.FirstNativeID, MaxRecordBytes: MaxRecordBytes}
	prefix = make([]store.Event, 0, len(b.Records))
	for i, r := range b.Records {
		line := make([]byte, 0, len(r.JSON)+1)
		line = append(line, r.JSON...)
		line = append(line, '\n')
		res, perr := events.Parse(st.Agent, line, r.Offset, opts)
		var parseStop *events.StopError
		if perr != nil {
			if errors.As(perr, &parseStop) {
				return prefix, parseStop, nil
			}
			return nil, nil, fmt.Errorf("hubsync: record %d at offset %d: %w", i, r.Offset, perr)
		}
		// The framed record must decode to exactly one record at exactly its
		// framed boundaries; anything else (for example JSON containing a
		// line terminator) means the frame lies about its bytes.
		if len(res.Events) != 1 || res.Events[0].Offset != r.Offset ||
			res.Events[0].End != r.End() || res.Cursor != r.End() {
			return nil, nil, fmt.Errorf(
				"hubsync: record %d at offset %d does not decode to exactly one record spanning [%d, %d)",
				i, r.Offset, r.Offset, r.End())
		}
		if r.Offset == 0 {
			// This batch carries the source's first record: it names the
			// native session of every later record.
			opts.FirstNative = res.Events[0].NativeID
		}
		prefix = append(prefix, res.Events[0])
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

func (e *Endpoint) track(s *connSession) {
	e.mu.Lock()
	e.conns[s] = struct{}{}
	e.mu.Unlock()
}

func (e *Endpoint) untrack(s *connSession) {
	e.mu.Lock()
	delete(e.conns, s)
	if s.deviceID != "" {
		if set := e.byDev[s.deviceID]; set != nil {
			delete(set, s)
			if len(set) == 0 {
				delete(e.byDev, s.deviceID)
			}
		}
	}
	e.mu.Unlock()
}

// bind records the session's authenticated device, so revocation can find
// and close it.
func (e *Endpoint) bind(s *connSession, deviceID string) {
	e.mu.Lock()
	s.deviceID = deviceID // written once, before the session joins the map
	set := e.byDev[deviceID]
	if set == nil {
		set = map[*connSession]struct{}{}
		e.byDev[deviceID] = set
	}
	set[s] = struct{}{}
	e.mu.Unlock()
}

// closeAll cancels and closes every live connection so blocked handlers
// return when the endpoint shuts down.
func (e *Endpoint) closeAll() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return
	}
	e.closed = true
	for s := range e.conns {
		s.cancel()
		_ = s.conn.Close()
	}
}

// closeDevice cancels and closes one device's live connections: the endpoint
// half of RevokeDevice in tla/Auth.tla. Cancelling stops the device's
// in-flight requests; closing unblocks its readers. Data a request already
// returned cannot be recalled.
func (e *Endpoint) closeDevice(deviceID string) {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	sessions := make([]*connSession, 0, len(e.byDev[deviceID]))
	for s := range e.byDev[deviceID] {
		sessions = append(sessions, s)
	}
	e.mu.Unlock()
	for _, s := range sessions {
		s.cancel()
		_ = s.conn.Close()
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
