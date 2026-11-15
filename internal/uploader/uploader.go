package uploader

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/nibzard/ohya/internal/device"
	"github.com/nibzard/ohya/internal/hubsync"
	"github.com/nibzard/ohya/internal/watch"
	"github.com/nibzard/ohya/internal/workspace"
)

const DefaultRequestTimeout = 60 * time.Second

const (
	DefaultReconnectBase = time.Second
	DefaultReconnectCap  = time.Minute
)

var ErrDisconnected = errors.New("uploader: hub connection failed; unacknowledged bytes are sent again after reconnecting")

var ErrUnauthorized = errors.New("uploader: the hub refused this device's credential")

var ErrIncompatible = errors.New("uploader: the hub refused the wire protocol")

type Dialer func(ctx context.Context) (net.Conn, error)

type Options struct {
	Agent          string
	RequestTimeout time.Duration
	Reconnect      hubsync.RetryPolicy
	Interval       time.Duration
	Log            io.Writer
	Resolve        workspace.Resolver
}

type Stats struct {
	Sources      int
	Inserted     int64
	Acknowledged int64
}

type Uploader struct {
	watcher    *watch.Watcher
	credential device.Credential
	dial       Dialer
	opts       Options
	log        io.Writer

	conn         *conn
	ready        map[string]bool
	blocked      map[string]bool
	stops        map[string]stopMark
	seen         map[string]int64
	lossReported map[string]bool
	gapReported  map[string]int64
	offline      bool

	workspacesDue bool
	workspacesOff bool
}

type stopMark struct {
	offset, end int64
}

func New(w *watch.Watcher, cred device.Credential, dial Dialer, opts Options) (*Uploader, error) {
	switch {
	case w == nil:
		return nil, errors.New("uploader: needs a watcher")
	case dial == nil:
		return nil, errors.New("uploader: needs a dialer")
	case cred.DeviceID == "" || cred.Secret == "":
		return nil, errors.New("uploader: needs a paired device credential")
	case opts.Agent == "":
		return nil, errors.New("uploader: needs an agent name")
	}
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = DefaultRequestTimeout
	}
	if opts.Reconnect.Base <= 0 {
		opts.Reconnect.Base = DefaultReconnectBase
	}
	if opts.Reconnect.Cap <= 0 {
		opts.Reconnect.Cap = DefaultReconnectCap
	}
	log := opts.Log
	if log == nil {
		log = io.Discard
	}
	if opts.Resolve == nil {
		opts.Resolve = workspace.Resolve
	}
	return &Uploader{
		watcher: w, credential: cred, dial: dial, opts: opts, log: log,
		ready: map[string]bool{}, blocked: map[string]bool{},
		stops: map[string]stopMark{}, seen: map[string]int64{},
		lossReported: map[string]bool{}, gapReported: map[string]int64{},
	}, nil
}

func (u *Uploader) Close() error {
	if u.conn == nil {
		return nil
	}
	err := u.conn.c.Close()
	u.conn = nil
	return err
}

func (u *Uploader) Run(ctx context.Context, discover func() ([]string, error)) error {
	defer u.Close()
	failures := 0
	for {
		candidates, err := discover()
		if err != nil {
			return err
		}
		_, err = u.Sync(ctx, candidates)
		wait := u.opts.Interval
		switch {
		case ctx.Err() != nil:
			return nil
		case err == nil:
			failures = 0
		case errors.Is(err, ErrDisconnected):
			failures++
			wait = u.opts.Reconnect.Wait(failures)
		default:
			return err
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func (u *Uploader) Sync(ctx context.Context, candidates []string) (Stats, error) {
	var stats Stats
	outcomes, err := u.watcher.Poll(candidates)
	if err != nil {
		return stats, err
	}
	for _, o := range outcomes {
		u.noteLoss(o)
	}
	if err := u.connect(ctx); err != nil {
		return stats, err
	}
	for _, o := range outcomes {
		if err := u.syncSource(ctx, o, &stats); err != nil {
			return stats, err
		}
	}
	return stats, u.syncWorkspaces(ctx, stats.Inserted > 0)
}

func (u *Uploader) syncWorkspaces(ctx context.Context, inserted bool) error {
	if u.workspacesOff || u.conn == nil || (!u.workspacesDue && !inserted) {
		return nil
	}
	res, err := workspace.Sync(ctx, u.exchangeWorkspaces, u.opts.Resolve)
	if n := len(res.Failures); n > 0 {
		u.logf("%d session directories could not be read and stay pending: %v", n, res.Failures[0])
	}
	var perr *hubsync.Error
	switch {
	case err == nil:
		u.workspacesDue = false
	case errors.As(err, &perr) && perr.Code == hubsync.CodeUnsupported:
		u.workspacesOff = true
		u.logf("the hub refused workspace observations for agent %s: %v", u.opts.Agent, perr)
	case errors.As(err, &perr) && perr.Code == hubsync.CodeRejected:
		u.workspacesDue = false
		u.logf("the hub rejected workspace observations: %v", perr)
	case errors.As(err, &perr):
		u.logf("workspace observations failed at the hub, retrying next pass: %v", perr)
	case errors.Is(err, ErrIncompatible):
		u.workspacesOff = true
		u.logf("the hub does not accept workspace observations, so repositories of this device's sessions stay unknown until the hub is upgraded: %v", err)
	default:
		return err
	}
	return nil
}

func (u *Uploader) exchangeWorkspaces(ctx context.Context, observed []workspace.Observation) ([]string, error) {
	var reply hubsync.WorkspacesReply
	perr, err := u.roundTrip(ctx,
		func(w io.Writer) error {
			return hubsync.WriteWorkspaces(w, hubsync.Workspaces{
				DeviceID: u.credential.DeviceID, Agent: u.opts.Agent, Observations: observed})
		},
		func(r io.Reader) (*hubsync.Error, error) {
			var perr *hubsync.Error
			var err error
			reply, perr, err = hubsync.ReadWorkspacesReply(r)
			return perr, err
		})
	if err != nil {
		return nil, err
	}
	if perr != nil {
		return nil, perr
	}
	return reply.Pending, nil
}

func (u *Uploader) noteLoss(o watch.Outcome) {
	switch {
	case o.Fresh && o.ReplacedSourceID != "":
		old := o.ReplacedSourceID
		end, ok := u.seen[old]
		switch {
		case ok && end > o.ReplacedCursor:
			u.logf("%s: source %s was replaced (%s) before upload: %d bytes after acknowledged offset %d were never uploaded and are lost (no outgoing spool)",
				o.Path, old, o.Reason, end-o.ReplacedCursor, o.ReplacedCursor)
		case !ok:
			u.logf("%s: source %s was replaced (%s): any bytes it gained after acknowledged offset %d and before this poll were never uploaded and are lost (no outgoing spool)",
				o.Path, old, o.Reason, o.ReplacedCursor)
		}
		delete(u.seen, old)
		delete(u.lossReported, old)
	case o.Missing:
		if end, ok := u.seen[o.SourceID]; ok && end > o.Cursor && !u.lossReported[o.SourceID] {
			u.lossReported[o.SourceID] = true
			u.logf("%s: source %s disappeared before upload: bytes [%d, %d) were never uploaded; they upload if the file returns, otherwise they are lost (no outgoing spool)",
				o.Path, o.SourceID, o.Cursor, end)
		}
		return
	}
	u.seen[o.SourceID] = o.End
	delete(u.lossReported, o.SourceID)
}

func (u *Uploader) syncSource(ctx context.Context, o watch.Outcome, stats *Stats) error {
	if o.Missing {
		return u.checkGap(ctx, o)
	}
	stats.Sources++
	if u.blocked[o.SourceID] {
		return nil
	}
	if !u.ready[o.SourceID] {
		cursor, ok, err := u.prepare(ctx, o)
		if err != nil || !ok {
			return err
		}
		o.Cursor = cursor
	}
	for o.Cursor < o.End {
		if s, ok := u.stops[o.SourceID]; ok && s.offset == o.Cursor && s.end == o.End {
			return nil
		}
		b, stop, err := u.buildBatch(o)
		if err != nil {
			u.logf("%s: %v", o.Path, err)
			return nil
		}
		if len(b.Records) == 0 {
			if stop != nil {
				u.stopAt(o, stop.offset, stop.reason)
			}
			return nil
		}
		ack, perr, err := u.exchange(ctx, func(w io.Writer) error { return hubsync.WriteBatch(w, b) })
		if err != nil {
			return err
		}
		if perr != nil {
			cursor, retry, err := u.batchRefused(ctx, o, perr)
			if err != nil || !retry {
				return err
			}
			o.Cursor = cursor
			continue
		}
		if ack.Cursor < o.Cursor {
			cursor, ok := u.reconcile(o, ack.Cursor)
			if !ok {
				return nil
			}
			o.Cursor = cursor
			continue
		}
		if ack.Cursor > o.Cursor {
			if err := u.watcher.Acknowledge(o.Path, o.SourceID, ack.Cursor); err != nil {
				u.logf("%s: persist acknowledgment: %v", o.Path, err)
				return nil
			}
			stats.Acknowledged += ack.Cursor - o.Cursor
			stats.Inserted += ack.Inserted
			o.Cursor = ack.Cursor
		}
		if ack.Stopped {
			u.stopAt(o, ack.StopOffset, ack.Stop)
			return nil
		}
		if ack.Cursor < b.End {
			u.logf("%s: hub acknowledged %d for a batch ending at %d without a stop; retrying next pass",
				o.Path, ack.Cursor, b.End)
			return nil
		}
	}
	return nil
}

func (u *Uploader) prepare(ctx context.Context, o watch.Outcome) (int64, bool, error) {
	_, perr, err := u.exchange(ctx, func(w io.Writer) error {
		return hubsync.WriteRegistration(w, hubsync.Registration{
			DeviceID: u.credential.DeviceID, SourceID: o.SourceID, Agent: u.opts.Agent, Path: o.Path})
	})
	if err != nil {
		return 0, false, err
	}
	if perr != nil {
		return 0, false, u.sourceRefused(o, perr)
	}
	hub, perr, err := u.probe(ctx, o.SourceID, o.Cursor)
	if err != nil {
		return 0, false, err
	}
	if perr != nil {
		return 0, false, u.sourceRefused(o, perr)
	}
	cursor, ok := u.reconcile(o, hub)
	return cursor, ok, nil
}

func (u *Uploader) probe(ctx context.Context, sourceID string, at int64) (int64, *hubsync.Error, error) {
	ack, perr, err := u.exchange(ctx, func(w io.Writer) error {
		return hubsync.WriteBatch(w, hubsync.Batch{
			DeviceID: u.credential.DeviceID, SourceID: sourceID, Start: at, End: at})
	})
	return ack.Cursor, perr, err
}

func (u *Uploader) reconcile(o watch.Outcome, hub int64) (int64, bool) {
	switch {
	case hub > o.Cursor:
		if err := u.watcher.Acknowledge(o.Path, o.SourceID, hub); err != nil {
			u.block(o, fmt.Sprintf("hub cursor %d is ahead of the checkpoint %d but cannot be acknowledged: %v",
				hub, o.Cursor, err))
			return 0, false
		}
	case hub < o.Cursor:
		u.logf("%s: hub cursor of source %s regressed from %d to %d (hub restored from a backup?); replaying the retained file",
			o.Path, o.SourceID, o.Cursor, hub)
		if err := u.watcher.Rewind(o.Path, o.SourceID, hub); err != nil {
			u.block(o, fmt.Sprintf("unrecoverable gap: the hub lost bytes [%d, %d) and the retained file cannot replay them: %v",
				hub, o.Cursor, err))
			return 0, false
		}
	}
	u.ready[o.SourceID] = true
	return hub, true
}

func (u *Uploader) checkGap(ctx context.Context, o watch.Outcome) error {
	if o.Cursor == 0 || u.ready[o.SourceID] {
		return nil
	}
	hub, perr, err := u.probe(ctx, o.SourceID, o.Cursor)
	if err != nil {
		return err
	}
	if perr != nil {
		if perr.Code != hubsync.CodeUnknownSource {
			return u.sourceRefused(o, perr)
		}
		hub = 0
	}
	u.ready[o.SourceID] = true
	if hub < o.Cursor && u.gapReported[o.SourceID] != hub+1 {
		u.gapReported[o.SourceID] = hub + 1
		u.logf("%s: unrecoverable gap: the hub lost bytes [%d, %d) of source %s (hub restored from a backup?), and the file is no longer retained to replay them",
			o.Path, hub, o.Cursor, o.SourceID)
	}
	return nil
}

func (u *Uploader) batchRefused(ctx context.Context, o watch.Outcome, perr *hubsync.Error) (int64, bool, error) {
	switch perr.Code {
	case hubsync.CodeRejected:
		hub, perr2, err := u.probe(ctx, o.SourceID, o.Cursor)
		if err != nil {
			return 0, false, err
		}
		if perr2 == nil && hub < o.Cursor {
			cursor, ok := u.reconcile(o, hub)
			return cursor, ok, nil
		}
		return 0, false, u.sourceRefused(o, perr)
	case hubsync.CodeUnknownSource:
		delete(u.ready, o.SourceID)
		u.logf("%s: hub no longer knows source %s; registering it again next pass", o.Path, o.SourceID)
		return 0, false, nil
	default:
		return 0, false, u.sourceRefused(o, perr)
	}
}

func (u *Uploader) sourceRefused(o watch.Outcome, perr *hubsync.Error) error {
	switch perr.Code {
	case hubsync.CodeInternal:
		u.logf("%s: hub failed to commit source %s, retrying next pass: %s", o.Path, o.SourceID, perr.Message)
		return nil
	case hubsync.CodeRejected, hubsync.CodeOwnership, hubsync.CodeUnsupported, hubsync.CodeUnknownSource:
		u.block(o, perr.Error())
		return nil
	default:
		return connError(perr)
	}
}

func (u *Uploader) block(o watch.Outcome, reason string) {
	u.blocked[o.SourceID] = true
	u.logf("%s: source %s stops uploading: %s", o.Path, o.SourceID, reason)
}

func (u *Uploader) stopAt(o watch.Outcome, offset int64, reason string) {
	prev, seen := u.stops[o.SourceID]
	u.stops[o.SourceID] = stopMark{offset: offset, end: o.End}
	if !seen || prev.offset != offset {
		u.logf("%s: source %s stops at offset %d: %s", o.Path, o.SourceID, offset, reason)
	}
}

type localStop struct {
	offset int64
	reason string
}

func (u *Uploader) buildBatch(o watch.Outcome) (hubsync.Batch, *localStop, error) {
	chunk := o
	chunk.End = min(o.End, o.Cursor+hubsync.MaxBatchBytes)
	data, err := u.watcher.ReadPending(chunk)
	if err != nil {
		return hubsync.Batch{}, nil, err
	}
	b := hubsync.Batch{DeviceID: u.credential.DeviceID, SourceID: o.SourceID, Start: o.Cursor, End: o.Cursor}
	var stop *localStop
	rel := 0
	for rel < len(data) && len(b.Records) < hubsync.MaxRecords {
		idx := bytes.IndexByte(data[rel:], '\n')
		offset := o.Cursor + int64(rel)
		if idx < 0 {
			if int64(len(data)-rel) > hubsync.MaxRecordBytes {
				stop = &localStop{offset, fmt.Sprintf(
					"incomplete record already exceeds the %d-byte record size limit", hubsync.MaxRecordBytes)}
			}
			break
		}
		if int64(idx) > hubsync.MaxRecordBytes {
			stop = &localStop{offset, fmt.Sprintf("record exceeds the %d-byte record size limit", hubsync.MaxRecordBytes)}
			break
		}
		rec := hubsync.Record{Offset: offset, JSON: data[rel : rel+idx : rel+idx]}
		b.Records = append(b.Records, rec)
		b.End = rec.End()
		rel += idx + 1
	}
	return b, stop, nil
}

type conn struct {
	c  net.Conn
	br *bufio.Reader
}

func (c *conn) alive() bool {
	_ = c.c.SetReadDeadline(time.Now().Add(time.Millisecond))
	_, err := c.br.Peek(1)
	_ = c.c.SetReadDeadline(time.Time{})
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func (u *Uploader) connect(ctx context.Context) error {
	if u.conn != nil {
		if u.conn.alive() {
			return nil
		}
		u.Close()
	}
	dctx, cancel := context.WithTimeout(ctx, u.opts.RequestTimeout)
	defer cancel()
	c, err := u.dial(dctx)
	if err != nil {
		return u.disconnected(ctx, fmt.Errorf("dial hub: %w", err))
	}
	_ = c.SetWriteDeadline(time.Now().Add(u.opts.RequestTimeout))
	if err := hubsync.WriteHello(c, u.credential.Secret); err != nil {
		c.Close()
		return u.disconnected(ctx, err)
	}
	u.conn = &conn{c: c, br: bufio.NewReaderSize(c, 64<<10)}
	u.ready = map[string]bool{}
	u.workspacesDue = true
	if u.offline {
		u.offline = false
		u.logf("reconnected to the hub; catching up from the retained files")
	}
	return nil
}

func (u *Uploader) exchange(ctx context.Context, write func(io.Writer) error) (hubsync.Ack, *hubsync.Error, error) {
	var ack hubsync.Ack
	perr, err := u.roundTrip(ctx, write, func(r io.Reader) (*hubsync.Error, error) {
		var perr *hubsync.Error
		var err error
		ack, perr, err = hubsync.ReadReply(r)
		return perr, err
	})
	return ack, perr, err
}

func (u *Uploader) roundTrip(ctx context.Context, write func(io.Writer) error, read func(io.Reader) (*hubsync.Error, error)) (*hubsync.Error, error) {
	c := u.conn
	deadline := time.Now().Add(u.opts.RequestTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = c.c.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = c.c.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	bw := bufio.NewWriterSize(c.c, 64<<10)
	err := write(bw)
	if err == nil {
		err = bw.Flush()
	}
	if err != nil {
		_ = c.c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		if _, perr, rerr := hubsync.ReadReply(c.br); rerr == nil && perr != nil {
			u.Close()
			return nil, connError(perr)
		}
		u.Close()
		return nil, u.disconnected(ctx, err)
	}
	perr, err := read(c.br)
	if err != nil {
		u.Close()
		return nil, u.disconnected(ctx, err)
	}
	if perr != nil {
		switch perr.Code {
		case hubsync.CodeAuth, hubsync.CodeVersion, hubsync.CodeMalformed, hubsync.CodeTooLarge, hubsync.CodeBusy:
			u.Close()
			return nil, connError(perr)
		}
	}
	return perr, nil
}

func connError(perr *hubsync.Error) error {
	switch perr.Code {
	case hubsync.CodeAuth:
		return fmt.Errorf("%w: %v", ErrUnauthorized, perr)
	case hubsync.CodeBusy:
		return fmt.Errorf("%w: %v", ErrDisconnected, perr)
	default:
		return fmt.Errorf("%w: %v", ErrIncompatible, perr)
	}
}

func (u *Uploader) disconnected(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !u.offline {
		u.offline = true
		u.logf("hub unreachable, retrying with backoff: %v", err)
	}
	return fmt.Errorf("%w: %v", ErrDisconnected, err)
}

func (u *Uploader) logf(format string, args ...any) {
	fmt.Fprintf(u.log, "ohya: "+format+"\n", args...)
}
