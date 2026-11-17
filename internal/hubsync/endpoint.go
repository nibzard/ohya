package hubsync

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/nibzard/ohya/internal/events"
	"github.com/nibzard/ohya/internal/mcpserver"
	"github.com/nibzard/ohya/internal/observe"
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
	Observer    observe.Observer
}

type Endpoint struct {
	db       *store.DB
	auth     Authenticator
	authz    SessionAuthorizer
	remote   RemoteEngine
	opts     Options
	observer observe.Observer
	slots    chan struct{}
	gates    sourceGates
	mu       sync.Mutex
	conns    map[*connSession]struct{}
	byDev    map[string]map[*connSession]struct{}
	closed   bool
	wg       sync.WaitGroup
}

type connSession struct {
	ctx      context.Context
	conn     net.Conn
	deviceID string
	cancel   context.CancelFunc
}

func (s *connSession) Write(p []byte) (int, error) {
	if err := s.ctx.Err(); err != nil {
		return 0, err
	}
	return s.conn.Write(p)
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
	observer := opts.Observer
	if observer == nil {
		observer = observe.Disabled
	}
	return &Endpoint{
		db:       db,
		auth:     auth,
		authz:    authz,
		remote:   opts.Remote,
		opts:     opts,
		observer: observer,
		slots:    make(chan struct{}, maxConns),
		conns:    map[*connSession]struct{}{},
		byDev:    map[string]map[*connSession]struct{}{},
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
			_ = e.authz.WatchRevocations(ctx, func(deviceID string) {
				e.observer.Add("hub_authorization", 1, "revoked")
				e.closeDevice(deviceID)
			})
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
			e.observer.Add("hub_connections", 1, "admitted")
		default:
			e.observer.Add("hub_connections", 1, "rejected")
			_ = WriteError(conn, CodeBusy,
				fmt.Sprintf("hub is serving its limit of %d connections", e.opts.MaxConns))
			_ = conn.Close()
			continue
		}
		cctx, ccancel := context.WithCancel(ctx)
		session := &connSession{ctx: cctx, conn: conn, cancel: ccancel}
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
		e.observer.Add("hub_authorization", 1, "rejected")
		_ = WriteError(session, CodeAuth, err.Error())
		return
	}
	e.observer.Add("hub_connections", 1, "active")
	e.bind(session, deviceID)
	br := bufio.NewReaderSize(conn, 64<<10)
	for {
		frame, err := ReadFrame(br)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				e.observer.Add("hub_failures", 1, "read")
			}
			e.replyDecodeError(session, err)
			return
		}
		if e.authz != nil {
			if err := e.authz.Authorize(ctx, deviceID); err != nil {
				e.observer.Add("hub_authorization", 1, "rejected")
				_ = WriteError(session, CodeAuth, err.Error())
				return
			}
			e.observer.Add("hub_authorization", 1, "ok")
		}
		var closeConn bool
		switch {
		case frame.Registration != nil:
			closeConn = e.handleRegistration(ctx, session, deviceID, *frame.Registration)
		case frame.Batch != nil:
			closeConn = e.handleBatch(ctx, session, deviceID, *frame.Batch)
		case frame.Query != nil:
			closeConn = e.handleQuery(ctx, session, deviceID, *frame.Query)
		case frame.Annotate != nil:
			closeConn = e.handleAnnotate(ctx, session, deviceID, *frame.Annotate)
		case frame.Workspaces != nil:
			closeConn = e.handleWorkspaces(ctx, session, deviceID, *frame.Workspaces)
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

func (e *Endpoint) handleRegistration(ctx context.Context, w io.Writer, deviceID string, reg Registration) bool {
	if reg.DeviceID != deviceID {
		_ = WriteError(w, CodeAuth, fmt.Sprintf(
			"registration names device %s, but the connection belongs to %s", reg.DeviceID, deviceID))
		return true
	}
	if !events.Supported(reg.Agent) {
		_ = WriteError(w, CodeUnsupported,
			fmt.Sprintf("agent %q has no parser at this hub; this hub parses %q", reg.Agent, events.Agents))
		return false
	}
	storeStart := time.Now()
	err := Retry(ctx, e.opts.Retry, permanentStoreError, func() error {
		return e.db.RegisterSource(ctx, reg.SourceID, deviceID, reg.Agent, reg.Path)
	})
	e.observer.Observe("hub_store_duration", time.Since(storeStart))
	if err != nil {
		e.observer.Add("hub_failures", 1, "commit")
		_ = WriteError(w, storeErrorCode(err), err.Error())
		return false
	}
	_ = WriteAck(w, Ack{})
	return false
}

func (e *Endpoint) handleBatch(ctx context.Context, w io.Writer, deviceID string, b Batch) bool {
	if b.DeviceID != deviceID {
		_ = WriteError(w, CodeAuth, fmt.Sprintf(
			"batch names device %s, but the connection belongs to %s", b.DeviceID, deviceID))
		return true
	}
	if err := validateBoundaries(b); err != nil {
		_ = WriteError(w, CodeRejected, err.Error())
		return false
	}

	gateStart := time.Now()
	release := e.gates.acquire(b.SourceID)
	e.observer.Observe("hub_gate_wait_duration", time.Since(gateStart))
	defer release()

	var st store.SourceState
	err := Retry(ctx, e.opts.Retry, permanentStoreError, func() error {
		var err error
		st, err = e.db.SourceState(ctx, b.SourceID)
		return err
	})
	if err != nil {
		e.observer.Add("hub_failures", 1, "commit")
		_ = WriteError(w, storeErrorCode(err), err.Error())
		return false
	}
	prefix, stop, err := e.normalize(b, st)
	if err != nil {
		_ = WriteError(w, CodeRejected, err.Error())
		return false
	}
	var res store.IngestResult
	storeStart := time.Now()
	err = Retry(ctx, e.opts.Retry, permanentStoreError, func() error {
		var err error
		res, err = e.db.Ingest(ctx, store.Batch{
			SourceID: b.SourceID,
			DeviceID: deviceID,
			Events:   prefix,
		})
		return err
	})
	e.observer.Observe("hub_store_duration", time.Since(storeStart))
	if err != nil {
		e.observer.Add("hub_failures", 1, "commit")
		_ = WriteError(w, storeErrorCode(err), err.Error())
		return false
	}
	ack := Ack{Cursor: res.Cursor, Inserted: int64(res.Inserted)}
	if stop != nil {
		ack.Stopped = true
		ack.StopOffset = stop.Offset
		ack.Stop = stop.Error()
	}
	ackStart := time.Now()
	_ = WriteAck(w, ack)
	e.observer.Observe("hub_ack_write_duration", time.Since(ackStart))
	return false
}

func (e *Endpoint) handleQuery(ctx context.Context, w io.Writer, deviceID string, q Query) bool {
	if e.remote == nil {
		_ = WriteError(w, CodeNoRemote,
			"this endpoint serves the upload protocol only; query and annotate frames are not enabled")
		return false
	}
	ctx = mcpserver.WithOrigin(ctx, mcpserver.Origin{
		Path:     observe.UsagePathRemote,
		DeviceID: deviceID,
	})
	result, err := e.remote.Query(ctx, q.SQL)
	if err != nil {
		e.observer.Add("hub_failures", 1, "other")
		_ = WriteError(w, queryErrorCode(err), err.Error())
		return false
	}
	if err := WriteQueryResult(w, result); err != nil {
		e.observer.Add("hub_failures", 1, "write")
		_ = WriteError(w, CodeInternal, err.Error())
		return false
	}
	return false
}

func (e *Endpoint) handleAnnotate(ctx context.Context, w io.Writer, deviceID string, a Annotate) bool {
	if e.remote == nil {
		_ = WriteError(w, CodeNoRemote,
			"this endpoint serves the upload protocol only; query and annotate frames are not enabled")
		return false
	}
	ctx = mcpserver.WithOrigin(ctx, mcpserver.Origin{
		Path:     observe.UsagePathRemote,
		DeviceID: deviceID,
	})
	result, err := e.remote.AnnotateAs(ctx, deviceID, a.Request)
	if err != nil {
		e.observer.Add("hub_failures", 1, "other")
		_ = WriteError(w, annotateErrorCode(err), err.Error())
		return false
	}
	_ = WriteAnnotateResult(w, result)
	return false
}

func (e *Endpoint) handleWorkspaces(ctx context.Context, wr io.Writer, deviceID string, w Workspaces) bool {
	if w.DeviceID != deviceID {
		_ = WriteError(wr, CodeAuth, fmt.Sprintf(
			"workspaces frame names device %s, but the connection belongs to %s", w.DeviceID, deviceID))
		return true
	}
	if !events.Supported(w.Agent) {
		_ = WriteError(wr, CodeUnsupported,
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
		_ = WriteError(wr, storeErrorCode(err), err.Error())
		return false
	}
	_ = WriteWorkspacesReply(wr, reply)
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

func (e *Endpoint) normalize(b Batch, st store.SourceState) (prefix []store.Event, stop *events.StopError, err error) {
	opts := events.Options{
		SourceScope: b.SourceID, FirstNative: st.FirstNativeID, MaxRecordBytes: MaxRecordBytes}
	normalizeStart := time.Now()
	var parseBytes, normalized uint64
	defer func() {
		e.observer.Observe("hub_normalize_duration", time.Since(normalizeStart))
		e.observer.Add("parse_bytes", parseBytes)
		e.observer.Add("parse_records", normalized, "normalized")
	}()
	e.observer.Add("parse_records", uint64(len(b.Records)), "attempted")
	prefix = make([]store.Event, 0, len(b.Records))
	for i, r := range b.Records {
		line := make([]byte, 0, len(r.JSON)+1)
		line = append(line, r.JSON...)
		line = append(line, '\n')
		parseBytes += uint64(len(line))
		res, perr := events.Parse(st.Agent, line, r.Offset, opts)
		var parseStop *events.StopError
		if perr != nil {
			if errors.As(perr, &parseStop) {
				e.observer.Add("parse_records", 1, "stopped")
				if parseStop.RecordTooLarge {
					e.observer.Add("parse_stops", 1, "oversize")
				} else {
					e.observer.Add("parse_stops", 1, "invalid")
				}
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
		normalized++
	}
	e.observer.Observe("parse_duration", time.Since(normalizeStart), st.Agent)
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

func (e *Endpoint) replyDecodeError(w io.Writer, err error) {
	code := CodeMalformed
	switch {
	case errors.Is(err, ErrTooLarge):
		code = CodeTooLarge
	case errors.Is(err, ErrVersion):
		code = CodeVersion
	}
	_ = WriteError(w, code, err.Error())
}

func (e *Endpoint) track(s *connSession) {
	e.mu.Lock()
	e.conns[s] = struct{}{}
	open := len(e.conns)
	e.mu.Unlock()
	e.observer.Set("hub_active_connections", float64(open))
}

func (e *Endpoint) untrack(s *connSession) {
	e.mu.Lock()
	delete(e.conns, s)
	open := len(e.conns)
	if s.deviceID != "" {
		if set := e.byDev[s.deviceID]; set != nil {
			delete(set, s)
			if len(set) == 0 {
				delete(e.byDev, s.deviceID)
			}
		}
	}
	e.mu.Unlock()
	e.observer.Set("hub_active_connections", float64(open))
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
