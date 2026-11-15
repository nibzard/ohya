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

const DefaultMaxConns = 16

const DefaultAuthTimeout = 30 * time.Second

type Authenticator interface {
	Authenticate(ctx context.Context, conn net.Conn) (deviceID string, err error)
}

type SessionAuthorizer interface {
	Authenticator
	Authorize(ctx context.Context, deviceID string) error
	WatchRevocations(ctx context.Context, fn func(deviceID string)) error
}

type StaticAuthenticator string

func (a StaticAuthenticator) Authenticate(context.Context, net.Conn) (string, error) {
	return string(a), nil
}

type RemoteEngine interface {
	Query(ctx context.Context, query string) (mcpserver.Result, error)
	AnnotateAs(ctx context.Context, writerID string, raw json.RawMessage) (store.AnnotateResult, error)
}

type Options struct {
	MaxConns    int
	Retry       RetryPolicy
	AuthTimeout time.Duration
	Remote      RemoteEngine
}

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

type connSession struct {
	conn     net.Conn
	deviceID string
	cancel   context.CancelFunc
}

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
			return
		}
		if e.authz != nil {
			if err := e.authz.Authorize(ctx, deviceID); err != nil {
				_ = WriteError(conn, CodeAuth, err.Error())
				return
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
		case frame.Workspaces != nil:
			closeConn = e.handleWorkspaces(ctx, conn, deviceID, *frame.Workspaces)
		}
		if closeConn {
			return
		}
	}
}

func (e *Endpoint) authenticate(ctx context.Context, conn net.Conn) (string, error) {
	_ = conn.SetReadDeadline(time.Now().Add(e.opts.AuthTimeout))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()
	return e.auth.Authenticate(ctx, conn)
}

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

	release := e.gates.acquire(b.SourceID)
	defer release()

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
	_ = WriteAck(conn, ack)
	return false
}

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
		_ = WriteError(conn, CodeInternal, err.Error())
		return false
	}
	return false
}

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

func (e *Endpoint) handleWorkspaces(ctx context.Context, conn net.Conn, deviceID string, w Workspaces) bool {
	if w.DeviceID != deviceID {
		_ = WriteError(conn, CodeAuth, fmt.Sprintf(
			"workspaces frame names device %s, but the connection belongs to %s", w.DeviceID, deviceID))
		return true
	}
	if !events.Supported(w.Agent) {
		_ = WriteError(conn, CodeUnsupported,
			fmt.Sprintf("agent %q has no parser at this hub; this hub parses %q", w.Agent, events.Agents))
		return false
	}
	var reply WorkspacesReply
	err := Retry(ctx, e.opts.Retry, permanentStoreError, func() error {
		var err error
		reply.Result, err = e.db.ObserveWorkspaces(ctx, deviceID, w.Observations)
		return err
	})
	if err == nil {
		err = Retry(ctx, e.opts.Retry, permanentStoreError, func() error {
			var err error
			reply.Pending, err = e.db.PendingWorkspaces(ctx, deviceID, w.Agent, MaxWorkspaces)
			return err
		})
	}
	if err != nil {
		_ = WriteError(conn, storeErrorCode(err), err.Error())
		return false
	}
	_ = WriteWorkspacesReply(conn, reply)
	return false
}

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

func annotateErrorCode(err error) ErrorCode {
	if errors.Is(err, mcpserver.ErrAnnotateRejected) || errors.Is(err, store.ErrAnnotationRejected) {
		return CodeRejected
	}
	return CodeInternal
}

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

func normalize(b Batch, st store.SourceState) (prefix []store.Event, stop *events.StopError, err error) {
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
		if len(res.Events) != 1 || res.Events[0].Offset != r.Offset ||
			res.Events[0].End != r.End() || res.Cursor != r.End() {
			return nil, nil, fmt.Errorf(
				"hubsync: record %d at offset %d does not decode to exactly one record spanning [%d, %d)",
				i, r.Offset, r.Offset, r.End())
		}
		if r.Offset == 0 {
			opts.FirstNative = res.Events[0].NativeID
		}
		prefix = append(prefix, res.Events[0])
	}
	return prefix, nil, nil
}

func permanentStoreError(err error) bool {
	return errors.Is(err, store.ErrBatchRejected) ||
		errors.Is(err, store.ErrObservationRejected) ||
		errors.Is(err, store.ErrSourceOwnership) ||
		errors.Is(err, store.ErrSourceNotRegistered)
}

func storeErrorCode(err error) ErrorCode {
	switch {
	case errors.Is(err, store.ErrSourceOwnership):
		return CodeOwnership
	case errors.Is(err, store.ErrSourceNotRegistered):
		return CodeUnknownSource
	case errors.Is(err, store.ErrBatchRejected), errors.Is(err, store.ErrObservationRejected):
		return CodeRejected
	default:
		return CodeInternal
	}
}

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

func (e *Endpoint) bind(s *connSession, deviceID string) {
	e.mu.Lock()
	s.deviceID = deviceID
	set := e.byDev[deviceID]
	if set == nil {
		set = map[*connSession]struct{}{}
		e.byDev[deviceID] = set
	}
	set[s] = struct{}{}
	e.mu.Unlock()
}

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
