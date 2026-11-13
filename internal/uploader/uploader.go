// Package uploader is the device side of the sync contract in
// ARCHITECTURE.md: it reads the unacknowledged bytes of each watched source
// file, sends them to the hub as framed batches over one authenticated
// connection, and persists each hub acknowledgment in the local checkpoint
// state (internal/watch). It implements the device actions of tla/Sync.tla:
//
//   - SendBatch: a batch always starts at the durable checkpoint, and one
//     connection carries strictly sequential exchanges, so each source has
//     at most one batch in flight.
//   - DeliverAck: only an acknowledgment the hub sent after its durable
//     commit moves the checkpoint. Sending a batch never does: a timeout, a
//     reset or a lost acknowledgment leaves the checkpoint where it was, and
//     the batch is sent again after reconnecting.
//
// Offline catch-up needs no queue: the retained source files are the
// backlog. On every new connection each source is registered and its hub
// cursor probed before any batch. A hub cursor ahead of the checkpoint is a
// lost acknowledgment and is persisted; a hub cursor behind it is a cursor
// regression, for example after the hub restored a backup, and the retained
// file replays the lost bytes, or the gap is reported as unrecoverable.
//
// Limitation (MVP): there is no outgoing spool and no offline query
// replica. Bytes that a source gains and then loses — the file is replaced,
// truncated, rewritten or deleted — before they are acknowledged are lost.
// The uploader reports each such loss; it cannot recover it.
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
)

// DefaultRequestTimeout bounds one exchange: writing a request frame and
// reading the hub's reply. A batch of the 8 MiB bound fits comfortably.
const DefaultRequestTimeout = 60 * time.Second

// Reconnect backoff defaults: the wait after the first failed pass, doubled
// per consecutive failure up to the cap.
const (
	DefaultReconnectBase = time.Second
	DefaultReconnectCap  = time.Minute
)

// ErrDisconnected reports that the hub was unreachable or the connection
// failed during an exchange: a dial failure, a timeout, a reset, or a busy
// hub. Nothing unanswered was acknowledged; the next pass reconnects and
// sends the same bytes again.
var ErrDisconnected = errors.New("uploader: hub connection failed; unacknowledged bytes are sent again after reconnecting")

// ErrUnauthorized reports that the hub refused the device credential, for
// example after the device was revoked. Retrying cannot help: revocation is
// final for a device identity.
var ErrUnauthorized = errors.New("uploader: the hub refused this device's credential")

// ErrIncompatible reports a hub that refused a frame this uploader built: a
// protocol version mismatch or a malformed frame. Retrying cannot help.
var ErrIncompatible = errors.New("uploader: the hub refused the wire protocol")

// Dialer opens one connection to the hub, for example transport.Device.Dial
// bound to the hub address.
type Dialer func(ctx context.Context) (net.Conn, error)

// Options configures an Uploader.
type Options struct {
	// Agent names the agent whose parser owns the watched sources; it must
	// match the watcher's agent.
	Agent string
	// RequestTimeout bounds one exchange. Values <= 0 select
	// DefaultRequestTimeout.
	RequestTimeout time.Duration
	// Reconnect bounds the wait between failed passes in Run. Unset Base
	// and Cap select DefaultReconnectBase and DefaultReconnectCap; Attempts
	// is unused, because Run retries until its context ends.
	Reconnect hubsync.RetryPolicy
	// Interval is the pause between successful passes in Run.
	Interval time.Duration
	// Log receives diagnostics: offsets, source IDs, paths and reasons,
	// never transcript content. Nil discards them.
	Log io.Writer
}

// Stats summarizes one pass.
type Stats struct {
	Sources      int   // live sources observed
	Inserted     int64 // events the hub newly inserted
	Acknowledged int64 // bytes the local checkpoints advanced
}

// Uploader drives one agent's sources to the hub. It is not safe for
// concurrent use; one goroutine drives it, like the watcher it owns.
type Uploader struct {
	watcher    *watch.Watcher
	credential device.Credential
	dial       Dialer
	opts       Options
	log        io.Writer

	conn *conn
	// ready holds the sources registered and reconciled on the current
	// connection. A new connection starts empty, so every reconnect
	// re-probes every source: that is how a hub restored from a backup is
	// noticed.
	ready map[string]bool
	// blocked holds sources the hub or the checkpoint refused for a reason
	// retrying cannot fix. A changed file is a new source and starts clean.
	blocked map[string]bool
	// stops holds the parse stop of each stopped source and the file end
	// it was observed at; the bytes are not sent again until the file grows.
	stops map[string]stopMark
	// seen holds the last observed end of each live source, so the loss of
	// unsent bytes through replacement or deletion can be counted.
	seen map[string]int64
	// lossReported and gapReported keep each diagnostic to one line.
	lossReported map[string]bool
	gapReported  map[string]int64
	offline      bool
}

type stopMark struct {
	offset, end int64
}

// New builds an uploader over a watcher and the device's pairing
// credential. The watcher's checkpoint state must be dedicated to this hub:
// its cursors are the hub's acknowledgments, so a state directory that local
// ingestion also acknowledges into would skip bytes the hub never received.
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
	return &Uploader{
		watcher: w, credential: cred, dial: dial, opts: opts, log: log,
		ready: map[string]bool{}, blocked: map[string]bool{},
		stops: map[string]stopMark{}, seen: map[string]int64{},
		lossReported: map[string]bool{}, gapReported: map[string]int64{},
	}, nil
}

// Close closes the hub connection, if one is open.
func (u *Uploader) Close() error {
	if u.conn == nil {
		return nil
	}
	err := u.conn.c.Close()
	u.conn = nil
	return err
}

// Run syncs until ctx ends: each pass discovers the candidate paths and calls
// Sync. A failed connection is retried with capped exponential backoff; the
// retained files are the offline backlog, so a reconnect catches up. Run
// returns nil when ctx ends, and an error only when retrying cannot help:
// ErrUnauthorized, ErrIncompatible, a discovery failure, or lost local state.
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

// Sync runs one pass: it polls the watcher, reports bytes lost before
// upload, connects if needed, and uploads every source's pending bytes,
// persisting each acknowledgment. A connection failure ends the pass with an
// error wrapping ErrDisconnected; acknowledged progress is kept. A source the
// hub refuses is reported and skipped; the other sources continue.
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
	return stats, nil
}

// noteLoss surfaces the MVP limitation: bytes a source gained but never had
// acknowledged are lost when the file is replaced or deleted before upload.
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
			// This process never observed the old incarnation's end.
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

// syncSource reconciles one source with the hub on this connection, then
// sends its pending bytes one batch at a time. It returns an error only for
// connection-level failures; source-level refusals are reported.
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
			return nil // stopped here, and the file has not grown since
		}
		b, stop, err := u.buildBatch(o)
		if err != nil {
			u.logf("%s: %v", o.Path, err) // the file changed since the poll
			return nil
		}
		if len(b.Records) == 0 {
			if stop != nil {
				u.stopAt(o, stop.offset, stop.reason)
			}
			return nil // nothing complete to send: an incomplete tail
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
			// The hub lost committed bytes while this connection was up.
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

// prepare registers the source and reconciles the local checkpoint with the
// hub's committed cursor. It reports ok=false for a refused source.
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

// probe asks the hub for the source's committed cursor with an empty batch.
func (u *Uploader) probe(ctx context.Context, sourceID string, at int64) (int64, *hubsync.Error, error) {
	ack, perr, err := u.exchange(ctx, func(w io.Writer) error {
		return hubsync.WriteBatch(w, hubsync.Batch{
			DeviceID: u.credential.DeviceID, SourceID: sourceID, Start: at, End: at})
	})
	return ack.Cursor, perr, err
}

// reconcile brings the local checkpoint in line with the hub cursor and
// marks the source ready on this connection. A hub ahead of the checkpoint
// committed a batch whose acknowledgment was lost: persist it. A hub behind
// the checkpoint lost acknowledged history: rewind and replay the retained
// file, or report the gap. ok=false means the source is blocked.
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

// checkGap probes a source whose file is gone. If the hub lost acknowledged
// bytes of it, nothing can replay them: report the unrecoverable gap.
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
		hub = 0 // the restored hub never saw the source
	}
	u.ready[o.SourceID] = true
	if hub < o.Cursor && u.gapReported[o.SourceID] != hub+1 {
		u.gapReported[o.SourceID] = hub + 1
		u.logf("%s: unrecoverable gap: the hub lost bytes [%d, %d) of source %s (hub restored from a backup?), and the file is no longer retained to replay them",
			o.Path, hub, o.Cursor, o.SourceID)
	}
	return nil
}

// batchRefused handles a protocol rejection of a batch. retry=true resumes
// the source from the returned cursor.
func (u *Uploader) batchRefused(ctx context.Context, o watch.Outcome, perr *hubsync.Error) (int64, bool, error) {
	switch perr.Code {
	case hubsync.CodeRejected:
		// A gap ahead of the hub cursor means the hub lost history while
		// this connection was up; re-probe before declaring a conflict.
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
		// The hub forgot the source; the next pass registers it again.
		delete(u.ready, o.SourceID)
		u.logf("%s: hub no longer knows source %s; registering it again next pass", o.Path, o.SourceID)
		return 0, false, nil
	default:
		return 0, false, u.sourceRefused(o, perr)
	}
}

// sourceRefused maps a rejection to its consequence: transient ones skip the
// source this pass, permanent ones block it, and connection-level ones
// return the matching error.
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

// block stops uploading one source for the rest of this process.
func (u *Uploader) block(o watch.Outcome, reason string) {
	u.blocked[o.SourceID] = true
	u.logf("%s: source %s stops uploading: %s", o.Path, o.SourceID, reason)
}

// stopAt records a parse stop and reports it once per offset.
func (u *Uploader) stopAt(o watch.Outcome, offset int64, reason string) {
	prev, seen := u.stops[o.SourceID]
	u.stops[o.SourceID] = stopMark{offset: offset, end: o.End}
	if !seen || prev.offset != offset {
		u.logf("%s: source %s stops at offset %d: %s", o.Path, o.SourceID, offset, reason)
	}
}

// localStop is a record the hub would refuse before parsing: one above the
// record size limit. Sending it would close the connection.
type localStop struct {
	offset int64
	reason string
}

// buildBatch frames the complete records from the checkpoint on, within the
// protocol's batch byte and record bounds. A record above the size limit
// ends the batch and is reported as a stop at its offset.
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
			// An append-only source only adds bytes, so a tail already
			// above the limit can never become a valid record.
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

// conn is one authenticated hub connection.
type conn struct {
	c  net.Conn
	br *bufio.Reader
}

// alive reports whether the connection is still open, without blocking: the
// hub never sends unsolicited, so between exchanges a read can only time
// out (open), fail (closed), or find stray bytes (out of sync).
func (c *conn) alive() bool {
	_ = c.c.SetReadDeadline(time.Now().Add(time.Millisecond))
	_, err := c.br.Peek(1)
	_ = c.c.SetReadDeadline(time.Time{})
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// connect dials the hub and presents the device credential, unless a live
// connection is already open. A new connection re-reconciles every source.
// An open connection the hub has closed is replaced at once: a pass with
// nothing pending makes no exchange, so this check is how an idle device
// learns that the hub restarted, perhaps from a restored backup.
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
	if u.offline {
		u.offline = false
		u.logf("reconnected to the hub; catching up from the retained files")
	}
	return nil
}

// exchange writes one request frame and reads the hub's reply under the
// request timeout. A transport failure closes the connection and returns an
// error wrapping ErrDisconnected: the request may or may not have committed,
// and only a later acknowledgment or probe can tell.
func (u *Uploader) exchange(ctx context.Context, write func(io.Writer) error) (hubsync.Ack, *hubsync.Error, error) {
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
		// The hub may have refused the connection before closing it, for
		// example at its connection bound: prefer its reason.
		_ = c.c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		if _, perr, rerr := hubsync.ReadReply(c.br); rerr == nil && perr != nil {
			u.Close()
			return hubsync.Ack{}, nil, connError(perr)
		}
		u.Close()
		return hubsync.Ack{}, nil, u.disconnected(ctx, err)
	}
	ack, perr, err := hubsync.ReadReply(c.br)
	if err != nil {
		u.Close()
		return hubsync.Ack{}, nil, u.disconnected(ctx, err)
	}
	if perr != nil {
		switch perr.Code {
		case hubsync.CodeAuth, hubsync.CodeVersion, hubsync.CodeMalformed, hubsync.CodeTooLarge, hubsync.CodeBusy:
			// The hub closes the connection after these.
			u.Close()
			return hubsync.Ack{}, nil, connError(perr)
		}
	}
	return ack, perr, nil
}

// connError maps a connection-closing rejection to the uploader's error.
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

// disconnected wraps a transport failure. A canceled context is reported as
// such, not as an outage.
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
