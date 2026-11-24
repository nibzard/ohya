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

const DefaultIdleTimeout = 5 * time.Minute

const DefaultWriteTimeout = 30 * time.Second

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
	MaxConns            int
	Retry               RetryPolicy
	AuthTimeout         time.Duration
	IdleTimeout         time.Duration
	WriteTimeout        time.Duration
	Remote              RemoteEngine
	Diagnostics         DiagnosticsProvider
	DiagnosticsInterval time.Duration
	DiagnosticsTimeout  time.Duration
	Observer            observe.Observer
}

type Endpoint struct {
	db          *store.DB
	auth        Authenticator
	authz       SessionAuthorizer
	remote      RemoteEngine
	diagnostics DiagnosticsProvider
	opts        Options
	observer    observe.Observer
	slots       chan struct{}
	diagSlots   chan struct{}
	diagMu      sync.Mutex
	diagLast    map[string]time.Time
	gates       sourceGates
	mu          sync.Mutex
	conns       map[*connSession]struct{}
	byDev       map[string]map[*connSession]struct{}
	closed      bool
	wg          sync.WaitGroup
}

type connSession struct {
	ctx          context.Context
	conn         net.Conn
	deviceID     string
	cancel       context.CancelFunc
	writeTimeout time.Duration
}

func (s *connSession) Write(p []byte) (int, error) {
	if err := s.ctx.Err(); err != nil {
		return 0, err
	}
	if s.writeTimeout > 0 {
		_ = s.conn.SetWriteDeadline(time.Now().Add(s.writeTimeout))
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
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = DefaultIdleTimeout
	}
	if opts.WriteTimeout <= 0 {
		opts.WriteTimeout = DefaultWriteTimeout
	}
	if opts.DiagnosticsTimeout <= 0 {
		opts.DiagnosticsTimeout = DefaultDiagnosticsTimeout
	}
	if opts.DiagnosticsInterval == 0 {
		opts.DiagnosticsInterval = DefaultDiagnosticsInterval
	}
	authz, _ := auth.(SessionAuthorizer)
	observer := opts.Observer
	if observer == nil {
		observer = observe.Disabled
	}
	return &Endpoint{
		db:          db,
		auth:        auth,
		authz:       authz,
		remote:      opts.Remote,
		diagnostics: opts.Diagnostics,
		opts:        opts,
		observer:    observer,
		slots:       make(chan struct{}, maxConns),
		diagSlots:   make(chan struct{}, DefaultMaxDiagnostics),
		diagLast:    map[string]time.Time{},
		conns:       map[*connSession]struct{}{},
		byDev:       map[string]map[*connSession]struct{}{},
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
			for {
				err := e.authz.WatchRevocations(ctx, func(deviceID string) {
					e.observer.Add("hub_authorization", 1, "revoked")
					e.closeDevice(deviceID)
				})
				if err == nil || ctx.Err() != nil {
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(100 * time.Millisecond):
				}
			}
		}()
	}
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				e.wg.Wait()
				return nil
			}
			cancel()
			e.closeAll()
			e.wg.Wait()
			return fmt.Errorf("hubsync: accept: %w", err)
		}
		select {
		case e.slots <- struct{}{}:
			e.observer.Add("hub_connections", 1, "admitted")
		default:
			e.observer.Add("hub_connections", 1, "rejected")
			_ = conn.SetWriteDeadline(time.Now().Add(e.opts.WriteTimeout))
			_ = WriteError(conn, CodeBusy,
				fmt.Sprintf("hub is serving its limit of %d connections", e.opts.MaxConns))
			_ = conn.Close()
			continue
		}
		cctx, ccancel := context.WithCancel(ctx)
		session := &connSession{ctx: cctx, conn: conn, cancel: ccancel, writeTimeout: e.opts.WriteTimeout}
		if !e.track(session) {
			ccancel()
			_ = conn.Close()
			<-e.slots
			continue
		}
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			defer ccancel()
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
		code := CodeAuth
		switch {
		case errors.Is(err, ErrVersion):
			code = CodeVersion
		case errors.Is(err, ErrTooLarge):
			code = CodeTooLarge
		}
		_ = WriteError(session, code, err.Error())
		return
	}
	e.observer.Add("hub_connections", 1, "active")
	e.bind(session, deviceID)
	if e.authz != nil {
		if err := e.authz.Authorize(ctx, deviceID); err != nil {
			e.observer.Add("hub_authorization", 1, "rejected")
			_ = WriteError(session, CodeAuth, err.Error())
			return
		}
	}
	br := bufio.NewReaderSize(conn, 64<<10)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(e.opts.IdleTimeout))
		frame, err := ReadFrame(br)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				e.observer.Add("hub_failures", 1, "read")
			}
			var ne net.Error
			if !(errors.As(err, &ne) && ne.Timeout()) && !errors.Is(err, io.EOF) {
				e.replyDecodeError(session, err)
			}
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
		case frame.SourceWorkspace != nil:
			closeConn = e.handleSourceWorkspace(ctx, session, deviceID, *frame.SourceWorkspace)
		case frame.Batch != nil:
			closeConn = e.handleBatch(ctx, session, deviceID, *frame.Batch)
		case frame.Capabilities != nil:
			closeConn = e.handleCapabilities(session, *frame.Capabilities)
		case frame.Query != nil:
			closeConn = e.handleQuery(ctx, session, deviceID, *frame.Query)
		case frame.Annotate != nil:
			closeConn = e.handleAnnotate(ctx, session, deviceID, *frame.Annotate)
		case frame.Workspaces != nil:
			closeConn = e.handleWorkspaces(ctx, session, deviceID, *frame.Workspaces)
		case frame.Diagnostics != nil:
			closeConn = e.handleDiagnostics(ctx, session, deviceID, *frame.Diagnostics)
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
	return WriteAck(w, Ack{}) != nil
}

func (e *Endpoint) handleSourceWorkspace(ctx context.Context, w io.Writer, deviceID string, sw SourceWorkspace) bool {
	if sw.DeviceID != deviceID {
		_ = WriteError(w, CodeAuth, fmt.Sprintf(
			"source workspace names device %s, but the connection belongs to %s", sw.DeviceID, deviceID))
		return true
	}
	storeStart := time.Now()
	err := Retry(ctx, e.opts.Retry, permanentStoreError, func() error {
		return e.db.SetSourceWorkspace(ctx, deviceID, sw.SourceID, sw.Path)
	})
	e.observer.Observe("hub_store_duration", time.Since(storeStart))
	if err != nil {
		e.observer.Add("hub_failures", 1, "commit")
		_ = WriteError(w, storeErrorCode(err), err.Error())
		return false
	}
	return WriteAck(w, Ack{}) != nil
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
	release, err := e.gates.acquire(ctx, b.SourceID)
	e.observer.Observe("hub_gate_wait_duration", time.Since(gateStart))
	if err != nil {
		return true
	}
	defer release()

	var st store.SourceState
	err = Retry(ctx, e.opts.Retry, permanentStoreError, func() error {
		var err error
		st, err = e.db.SourceStateForDevice(ctx, b.SourceID, deviceID)
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
	werr := WriteAck(w, ack)
	e.observer.Observe("hub_ack_write_duration", time.Since(ackStart))
	return werr != nil
}

func (e *Endpoint) capabilities() Capabilities {
	caps := CapZstdBatches | CapSourceWorkspace
	if e.diagnostics != nil {
		caps |= CapRemoteStatus
	}
	return caps
}

func (e *Endpoint) handleCapabilities(w io.Writer, offered Capabilities) bool {
	return WriteWelcome(w, offered&e.capabilities()) != nil
}

func (e *Endpoint) handleDiagnostics(ctx context.Context, w io.Writer, deviceID string, req DiagnosticsRequest) bool {
	if e.diagnostics == nil {
		_ = WriteError(w, CodeNoRemote, "remote status aggregates are not enabled on this hub")
		return false
	}
	if req.FormatVersion != DiagnosticsFormatVersion {
		_ = WriteError(w, CodeVersion, fmt.Sprintf("diagnostics format version %d is unsupported; want %d", req.FormatVersion, DiagnosticsFormatVersion))
		return false
	}
	if !e.allowDiagnosticsRate(deviceID, time.Now()) {
		_ = WriteError(w, CodeBusy, "remote status rate limit exceeded; wait before retrying")
		return false
	}
	select {
	case e.diagSlots <- struct{}{}:
		defer func() { <-e.diagSlots }()
	default:
		_ = WriteError(w, CodeBusy, fmt.Sprintf("hub is serving its limit of %d concurrent remote status requests", DefaultMaxDiagnostics))
		return false
	}
	dctx, cancel := context.WithTimeout(ctx, e.opts.DiagnosticsTimeout)
	defer cancel()
	status, err := e.diagnostics.RemoteStatus(dctx)
	if err != nil {
		if ctx.Err() != nil {
			return true
		}
		_ = WriteError(w, CodeInternal, "remote status collection failed")
		return false
	}
	if err := WriteDiagnosticsReply(w, status); err != nil {
		if errors.Is(err, ErrTooLarge) {
			_ = WriteError(w, CodeInternal, err.Error())
			return false
		}
		return true
	}
	return false
}

func (e *Endpoint) allowDiagnosticsRate(deviceID string, now time.Time) bool {
	if e.opts.DiagnosticsInterval < 0 {
		return true
	}
	e.diagMu.Lock()
	defer e.diagMu.Unlock()
	if prev, ok := e.diagLast[deviceID]; ok && now.Sub(prev) < e.opts.DiagnosticsInterval {
		return false
	}
	if len(e.diagLast) > 256 {
		for k, ts := range e.diagLast {
			if now.Sub(ts) >= e.opts.DiagnosticsInterval {
				delete(e.diagLast, k)
			}
		}
	}
	e.diagLast[deviceID] = now
	return true
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
		if errors.Is(err, ErrTooLarge) {
			return WriteError(w, CodeInternal, err.Error()) != nil
		}
		return true
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
	return WriteAnnotateResult(w, result) != nil
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
	if err := WriteWorkspacesReply(wr, reply); err != nil {
		e.observer.Add("hub_failures", 1, "write")
		if errors.Is(err, ErrTooLarge) {
			return WriteError(wr, CodeInternal, err.Error()) != nil
		}
		return true
	}
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
	switch {
	case errors.Is(err, mcpserver.ErrBusy):
		return CodeBusy
	case errors.Is(err, mcpserver.ErrAnnotateRejected),
		errors.Is(err, store.ErrAnnotationRejected),
		errors.Is(err, mcpserver.ErrDeadline),
		errors.Is(err, context.Canceled),
		errors.Is(err, context.DeadlineExceeded):
		return CodeRejected
	default:
		return CodeInternal
	}
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
		SourceScope: b.SourceID, SourcePath: st.Path, FirstNative: st.FirstNativeID, MaxRecordBytes: MaxRecordBytes}
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
		errors.Is(err, store.ErrSourceWorkspaceRejected) ||
		errors.Is(err, store.ErrSourceOwnership) ||
		errors.Is(err, store.ErrSourceNotRegistered) ||
		errors.Is(err, store.ErrSourcePurged)
}

func storeErrorCode(err error) ErrorCode {
	switch {
	case errors.Is(err, store.ErrSourceOwnership):
		return CodeOwnership
	case errors.Is(err, store.ErrSourceNotRegistered):
		return CodeUnknownSource
	case errors.Is(err, store.ErrBatchRejected), errors.Is(err, store.ErrObservationRejected),
		errors.Is(err, store.ErrSourceWorkspaceRejected), errors.Is(err, store.ErrSourcePurged):
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

func (e *Endpoint) track(s *connSession) bool {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return false
	}
	e.conns[s] = struct{}{}
	open := len(e.conns)
	e.mu.Unlock()
	e.observer.Set("hub_active_connections", float64(open))
	return true
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
	ch   chan struct{}
	refs int
}

func (g *sourceGates) acquire(ctx context.Context, sourceID string) (func(), error) {
	g.mu.Lock()
	if g.locks == nil {
		g.locks = map[string]*sourceGate{}
	}
	gate := g.locks[sourceID]
	if gate == nil {
		gate = &sourceGate{ch: make(chan struct{}, 1)}
		g.locks[sourceID] = gate
	}
	gate.refs++
	g.mu.Unlock()

	select {
	case gate.ch <- struct{}{}:
		return func() {
			<-gate.ch
			g.releaseRef(sourceID, gate)
		}, nil
	case <-ctx.Done():
		g.releaseRef(sourceID, gate)
		return nil, ctx.Err()
	}
}

func (g *sourceGates) releaseRef(sourceID string, gate *sourceGate) {
	g.mu.Lock()
	gate.refs--
	if gate.refs == 0 {
		delete(g.locks, sourceID)
	}
	g.mu.Unlock()
}
