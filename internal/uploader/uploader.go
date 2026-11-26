package uploader

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/nibzard/ohya/internal/device"
	"github.com/nibzard/ohya/internal/hubsync"
	"github.com/nibzard/ohya/internal/observe"
	"github.com/nibzard/ohya/internal/watch"
	"github.com/nibzard/ohya/internal/workspace"
)

const DefaultRequestTimeout = 60 * time.Second

const DefaultKeepaliveInterval = 3 * time.Minute

const (
	DefaultReconnectBase = time.Second
	DefaultReconnectCap  = time.Minute
)

const (
	backlogRateWindow  = 10 * time.Second
	backlogRateMinSpan = time.Second
	backlogLogFloor    = 1 << 20
	backlogLogInterval = 10 * time.Second
)

var ErrDisconnected = errors.New("uploader: hub connection failed; unacknowledged bytes are sent again after reconnecting")

var ErrUnauthorized = errors.New("uploader: the hub refused this device's credential")

var ErrIncompatible = errors.New("uploader: the hub refused the wire protocol")

func compressionDisabled() bool {
	return strings.EqualFold(os.Getenv("OHYA_UPLOAD_COMPRESSION"), "off")
}

type Dialer func(ctx context.Context) (net.Conn, error)

type Options struct {
	Agent             string
	RequestTimeout    time.Duration
	KeepaliveInterval time.Duration
	Reconnect         hubsync.RetryPolicy
	Interval          time.Duration
	Log               io.Writer
	Resolve           workspace.Resolver
	SourceWorkspace   func(sourcePath string) (string, bool)
	Observer          observe.Observer
	Events            observe.EventSink
	Excluded          func(path string) bool
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
	observer   observe.Observer
	events     observe.EventSink

	conn         *conn
	ready        map[string]bool
	unregistered map[string]bool
	blocked      map[string]bool
	stops        map[string]stopMark
	idle         map[string]stopMark
	seen         map[string]int64
	lossReported map[string]bool
	gapReported  map[string]int64
	sourceTold   map[string]bool
	offline      bool
	advances     uint32

	hubCaps            hubsync.Capabilities
	capsOff            bool
	sourceWorkspaceOff bool

	workspacesDue bool
	workspacesOff bool
	drain         *backlogDrain
}

type backlogDrain struct {
	start   time.Time
	left    int64
	sent    int64
	total   int64
	samples []drainSample
	lastLog time.Time
	logged  bool
}

type drainSample struct {
	at   time.Time
	sent int64
}

type stopMark struct {
	offset, end int64
	modTime     time.Time
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
	if opts.KeepaliveInterval == 0 {
		opts.KeepaliveInterval = DefaultKeepaliveInterval
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
	observer := opts.Observer
	if observer == nil {
		observer = observe.Disabled
	}
	return &Uploader{
		watcher: w, credential: cred, dial: dial, opts: opts, log: log, observer: observer,
		events: opts.Events,
		ready:  map[string]bool{}, unregistered: map[string]bool{}, blocked: map[string]bool{},
		stops: map[string]stopMark{}, idle: map[string]stopMark{}, seen: map[string]int64{},
		lossReported: map[string]bool{}, gapReported: map[string]int64{},
		sourceTold: map[string]bool{},
	}, nil
}

func (u *Uploader) event(level observe.Level, code observe.EventCode, outcome string, counts map[string]int64) {
	if u.events == nil {
		return
	}
	u.events.Event("uploader", level, code, outcome, counts)
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
			u.observer.Observe("upload_backoff_duration", wait)
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

func (u *Uploader) Sync(ctx context.Context, candidates []string) (stats Stats, err error) {
	outcomes, err := u.watcher.Poll(candidates)
	if err != nil {
		return stats, err
	}
	u.observer.Set("watch_sources", float64(len(u.stops)), "stopped")
	u.observer.Set("watch_sources", float64(len(u.blocked)), "blocked")
	defer func() {
		u.observer.Set("watch_sources", float64(len(u.stops)), "stopped")
		u.observer.Set("watch_sources", float64(len(u.blocked)), "blocked")
		if ferr := u.watcher.Flush(); ferr != nil {
			u.logf("persist acknowledgments: %v", ferr)
			u.observer.Add("upload_failures", 1, "checkpoint")
			u.event(observe.LevelWarn, observe.EventCheckpointFailed, "upload", nil)
			if err == nil {
				err = ferr
			}
			return
		}
		u.observer.Add("upload_checkpoint_advances", uint64(u.advances))
		u.advances = 0
	}()
	var missingAtStartup []string
	for _, o := range outcomes {
		if u.noteLoss(o) {
			missingAtStartup = append(missingAtStartup, o.Path)
		}
	}
	u.reportMissingAtStartup(missingAtStartup)
	u.updateBacklog(outcomes)
	if err = u.connect(ctx); err != nil {
		return stats, err
	}
	if err = u.probeSources(ctx, outcomes); err != nil {
		return stats, err
	}
	for _, o := range outcomes {
		if err = u.syncSource(ctx, o, &stats); err != nil {
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

func (u *Uploader) noteLoss(o watch.Outcome) (missingAtStartup bool) {
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
		delete(u.stops, old)
		delete(u.blocked, old)
		delete(u.idle, old)
		delete(u.ready, old)
		delete(u.sourceTold, old)
		delete(u.gapReported, old)
	case o.Missing:
		if u.lossReported[o.SourceID] {
			return false
		}
		u.lossReported[o.SourceID] = true
		if u.opts.Excluded != nil && u.opts.Excluded(o.Path) {
			if end, ok := u.seen[o.SourceID]; ok && end > o.Cursor {
				u.logf("%s: source %s now matches --exclude and stops uploading going forward; bytes [%d, %d) were never uploaded and never will be; stored sessions stay on the hub",
					o.Path, o.SourceID, o.Cursor, end)
				return false
			}
			u.logf("%s: source %s now matches --exclude and stops uploading going forward; the checkpoint is kept and upload resumes only if the exclusion is removed",
				o.Path, o.SourceID)
			return false
		}
		end, ok := u.seen[o.SourceID]
		if !ok {
			return true
		}
		if end > o.Cursor {
			u.logf("%s: source %s disappeared before upload: bytes [%d, %d) were never uploaded; they upload if the file returns, otherwise they are lost (no outgoing spool)",
				o.Path, o.SourceID, o.Cursor, end)
			return false
		}
		u.logf("%s: source %s disappeared; the checkpoint is kept and upload resumes if the file returns",
			o.Path, o.SourceID)
		return false
	}
	if u.lossReported[o.SourceID] {
		u.observer.Add("watch_source_events", 1, "returned")
		u.event(observe.LevelInfo, observe.EventSourceReturned, "", nil)
	}
	u.seen[o.SourceID] = o.End
	delete(u.lossReported, o.SourceID)
	return false
}

func (u *Uploader) reportMissingAtStartup(paths []string) {
	switch len(paths) {
	case 0:
	case 1:
		u.logf("%s: source with a saved checkpoint was already missing at startup; the checkpoint is kept and upload resumes if the file returns", paths[0])
	default:
		u.logf("%d sources with saved checkpoints were already missing at startup (first: %s); the checkpoints are kept and upload resumes if a file returns",
			len(paths), slices.Min(paths))
	}
}

func (u *Uploader) syncSource(ctx context.Context, o watch.Outcome, stats *Stats) error {
	if u.blocked[o.SourceID] {
		if !o.Missing {
			stats.Sources++
		}
		return nil
	}
	if o.Missing {
		return u.checkGap(ctx, o)
	}
	stats.Sources++
	if !u.ready[o.SourceID] {
		cursor, ok, err := u.prepare(ctx, o)
		if err != nil || !ok {
			return err
		}
		o.Cursor = cursor
	}
	for o.Cursor < o.End {
		if s, ok := u.stops[o.SourceID]; ok && s.offset == o.Cursor && s.end == o.End && s.modTime.Equal(o.ModTime) {
			return nil
		}
		if s, ok := u.idle[o.SourceID]; ok && s.offset == o.Cursor && s.end == o.End && s.modTime.Equal(o.ModTime) {
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
			} else {
				u.idle[o.SourceID] = stopMark{offset: o.Cursor, end: o.End, modTime: o.ModTime}
			}
			return nil
		}
		delete(u.idle, o.SourceID)
		u.observer.Add("upload_batches", 1, "attempted")
		u.observer.Add("upload_payload_bytes", uint64(b.End-b.Start))
		ack, perr, err := u.exchange(ctx, func(w io.Writer) error { return u.sendBatch(w, b) })
		if err != nil {
			return err
		}
		if perr != nil {
			u.observer.Add("upload_batches", 1, "rejected")
			cursor, retry, err := u.batchRefused(ctx, o, perr)
			if err != nil || !retry {
				return err
			}
			o.Cursor = cursor
			continue
		}
		u.observer.Add("upload_batches", 1, "successful")
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
				u.observer.Add("upload_failures", 1, "checkpoint")
				u.event(observe.LevelWarn, observe.EventCheckpointFailed, "upload", nil)
				return nil
			}
			delete(u.stops, o.SourceID)
			u.advances++
			u.observer.Add("upload_ack_bytes", uint64(ack.Cursor-o.Cursor))
			u.advanceBacklog(ack.Cursor - o.Cursor)
			u.observer.Add("upload_events_inserted", uint64(ack.Inserted))
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
	if o.Cursor > 0 && o.RenamedFrom == "" && !u.unregistered[o.SourceID] {
		hub, perr, err := u.probe(ctx, o.SourceID, o.Cursor)
		if err != nil {
			return 0, false, err
		}
		if perr == nil {
			return u.resume(ctx, o, hub)
		}
		if perr.Code != hubsync.CodeUnknownSource {
			return 0, false, u.sourceRefused(o, perr)
		}
	}
	delete(u.unregistered, o.SourceID)
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
	if err := u.sendSourceWorkspace(ctx, o); err != nil {
		return 0, false, err
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

func (u *Uploader) resume(ctx context.Context, o watch.Outcome, hub int64) (int64, bool, error) {
	if err := u.sendSourceWorkspace(ctx, o); err != nil {
		return 0, false, err
	}
	cursor, ok := u.reconcile(o, hub)
	return cursor, ok, nil
}

func (u *Uploader) sendSourceWorkspace(ctx context.Context, o watch.Outcome) error {
	if u.opts.SourceWorkspace == nil || u.sourceTold[o.SourceID] {
		return nil
	}
	path, ok := u.opts.SourceWorkspace(o.Path)
	if !ok {
		return nil
	}
	if u.hubCaps&hubsync.CapSourceWorkspace == 0 {
		if !u.sourceWorkspaceOff {
			u.sourceWorkspaceOff = true
			u.logf("the hub does not accept source workspaces, so sessions without a working directory stay without one until the hub is upgraded (first: %s)", o.Path)
		}
		return nil
	}
	var frame bytes.Buffer
	err := hubsync.WriteSourceWorkspace(&frame, hubsync.SourceWorkspace{
		DeviceID: u.credential.DeviceID, SourceID: o.SourceID, Path: path})
	if err != nil {
		u.logf("%s: source %s has a working directory the wire does not carry: %v", o.Path, o.SourceID, err)
		return nil
	}
	_, perr, err := u.exchange(ctx, func(w io.Writer) error {
		_, err := w.Write(frame.Bytes())
		return err
	})
	if err != nil {
		return err
	}
	if perr != nil {
		u.logf("%s: the hub did not store the working directory of source %s, and it is sent again on the next connection: %v",
			o.Path, o.SourceID, perr)
		return nil
	}
	u.sourceTold[o.SourceID] = true
	return nil
}

func (u *Uploader) probe(ctx context.Context, sourceID string, at int64) (int64, *hubsync.Error, error) {
	ack, perr, err := u.exchange(ctx, func(w io.Writer) error {
		return hubsync.WriteBatch(w, hubsync.Batch{
			DeviceID: u.credential.DeviceID, SourceID: sourceID, Start: at, End: at})
	})
	return ack.Cursor, perr, err
}

func (u *Uploader) probeSources(ctx context.Context, outcomes []watch.Outcome) error {
	if u.hubCaps&hubsync.CapBatchProbes == 0 {
		return nil
	}
	var pending []int
	for i := range outcomes {
		o := outcomes[i]
		if u.blocked[o.SourceID] || u.ready[o.SourceID] || o.Cursor == 0 {
			continue
		}
		if !o.Missing && o.RenamedFrom != "" {
			continue
		}
		pending = append(pending, i)
	}
	if len(pending) == 0 {
		return nil
	}
	for start := 0; start < len(pending); start += hubsync.MaxProbes {
		chunk := pending[start:min(start+hubsync.MaxProbes, len(pending))]
		ids := make([]string, len(chunk))
		for i, idx := range chunk {
			ids[i] = outcomes[idx].SourceID
		}
		var reply hubsync.ProbesReply
		perr, err := u.roundTrip(ctx,
			func(w io.Writer) error {
				return hubsync.WriteProbes(w, hubsync.Probes{
					DeviceID: u.credential.DeviceID, SourceIDs: ids})
			},
			func(r io.Reader) (*hubsync.Error, error) {
				var perr *hubsync.Error
				var err error
				reply, perr, err = hubsync.ReadProbesReply(r)
				return perr, err
			})
		if err != nil {
			return err
		}
		if perr != nil {
			switch perr.Code {
			case hubsync.CodeInternal:
				u.logf("hub failed to probe %d sources, retrying next pass: %s", len(ids), perr.Message)
				return nil
			default:
				return connError(perr)
			}
		}
		if len(reply.Entries) != len(chunk) {
			u.Close()
			u.observer.Add("upload_failures", 1, "read")
			return u.disconnected(ctx, fmt.Errorf("hubsync: probes reply holds %d entries for %d sources", len(reply.Entries), len(chunk)))
		}
		for i, idx := range chunk {
			o := outcomes[idx]
			entry := reply.Entries[i]
			switch entry.Status {
			case hubsync.ProbeKnown:
				if o.Missing {
					u.recordMissingGap(o, entry.Cursor)
					continue
				}
				cursor, ok, err := u.resume(ctx, o, entry.Cursor)
				if err != nil {
					return err
				}
				if ok {
					outcomes[idx].Cursor = cursor
				}
			case hubsync.ProbeUnknown:
				if o.Missing {
					u.recordMissingGap(o, 0)
					continue
				}
				u.unregistered[o.SourceID] = true
			case hubsync.ProbeOwnership:
				if err := u.sourceRefused(o, &hubsync.Error{Code: hubsync.CodeOwnership, Message: entry.Message}); err != nil {
					return err
				}
			case hubsync.ProbePurged:
				if err := u.sourceRefused(o, &hubsync.Error{Code: hubsync.CodeRejected, Message: entry.Message}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (u *Uploader) recordMissingGap(o watch.Outcome, hub int64) {
	u.ready[o.SourceID] = true
	if hub < o.Cursor && u.gapReported[o.SourceID] != hub+1 {
		u.gapReported[o.SourceID] = hub + 1
		u.logf("%s: unrecoverable gap: the hub lost bytes [%d, %d) of source %s (hub restored from a backup?), and the file is no longer retained to replay them",
			o.Path, hub, o.Cursor, o.SourceID)
	}
}

func (u *Uploader) reconcile(o watch.Outcome, hub int64) (int64, bool) {
	switch {
	case hub > o.Cursor:
		if err := u.watcher.Acknowledge(o.Path, o.SourceID, hub); err != nil {
			u.observer.Add("upload_failures", 1, "checkpoint")
			u.event(observe.LevelWarn, observe.EventCheckpointFailed, "upload", nil)
			u.block(o, fmt.Sprintf("hub cursor %d is ahead of the checkpoint %d but cannot be acknowledged: %v",
				hub, o.Cursor, err))
			return 0, false
		}
		u.advances++
		u.observer.Add("upload_ack_bytes", uint64(hub-o.Cursor))
		u.advanceBacklog(hub - o.Cursor)
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
		if perr2 == nil && hub != o.Cursor {
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
	u.stops[o.SourceID] = stopMark{offset: offset, end: o.End, modTime: o.ModTime}
	if !seen || prev.offset != offset {
		u.logf("%s: source %s stops at offset %d: %s", o.Path, o.SourceID, offset, reason)
		u.event(observe.LevelWarn, observe.EventSourceStopped, "", map[string]int64{"stopped_sources": int64(len(u.stops))})
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
	c        net.Conn
	br       *bufio.Reader
	bw       *bufio.Writer
	lastUsed time.Time
}

func (c *conn) alive() bool {
	_ = c.c.SetReadDeadline(time.Now().Add(time.Millisecond))
	_, err := c.br.Peek(1)
	_ = c.c.SetReadDeadline(time.Time{})
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func (u *Uploader) needsKeepalive() bool {
	return u.conn != nil && !u.capsOff && u.opts.KeepaliveInterval > 0 && time.Since(u.conn.lastUsed) >= u.opts.KeepaliveInterval
}

func (u *Uploader) keepalive(ctx context.Context) error {
	_, perr, err := u.negotiate(ctx, u.offer())
	if err != nil {
		return err
	}
	if perr != nil {
		u.Close()
		if perr.Code != hubsync.CodeMalformed && perr.Code != hubsync.CodeVersion {
			u.protocolFailure(perr)
			return connError(perr)
		}
		u.capsOff = true
		return ErrDisconnected
	}
	u.noteOnline()
	return nil
}

func (u *Uploader) connect(ctx context.Context) error {
	if u.conn != nil {
		if !u.conn.alive() {
			u.Close()
		} else if !u.needsKeepalive() {
			return nil
		} else if err := u.keepalive(ctx); err == nil {
			return nil
		} else if !errors.Is(err, ErrDisconnected) {
			return err
		}
	}
	for {
		if err := u.dialHello(ctx); err != nil {
			return err
		}
		if u.capsOff {
			return nil
		}
		if compressionDisabled() && u.opts.SourceWorkspace == nil {
			return nil
		}
		caps, perr, err := u.negotiate(ctx, u.offer())
		if err != nil {
			return err
		}
		if perr == nil {
			u.hubCaps = caps
			u.noteOnline()
			return nil
		}
		u.Close()
		if perr.Code != hubsync.CodeMalformed && perr.Code != hubsync.CodeVersion {
			u.protocolFailure(perr)
			return connError(perr)
		}
		u.capsOff = true
		u.logf("the hub does not accept capability negotiation (%v); uploading plain frames until this process restarts", perr)
	}
}

func (u *Uploader) offer() hubsync.Capabilities {
	caps := hubsync.CapSourceWorkspace | hubsync.CapBatchProbes
	if !compressionDisabled() {
		caps |= hubsync.CapZstdBatches
	}
	return caps
}

func (u *Uploader) dialHello(ctx context.Context) error {
	dctx, cancel := context.WithTimeout(ctx, u.opts.RequestTimeout)
	defer cancel()
	c, err := u.dial(dctx)
	if err != nil {
		u.observer.Add("upload_failures", 1, "connect")
		return u.disconnected(ctx, fmt.Errorf("dial hub: %w", err))
	}
	_ = c.SetWriteDeadline(time.Now().Add(u.opts.RequestTimeout))
	if err := hubsync.WriteHello(c, u.credential.Secret); err != nil {
		c.Close()
		u.observer.Add("upload_failures", 1, "connect")
		return u.disconnected(ctx, err)
	}
	u.conn = &conn{
		c:        c,
		br:       bufio.NewReaderSize(c, 64<<10),
		bw:       bufio.NewWriterSize(c, 64<<10),
		lastUsed: time.Now(),
	}
	u.hubCaps = 0
	u.ready = map[string]bool{}
	u.unregistered = map[string]bool{}
	u.workspacesDue = true
	return nil
}

func (u *Uploader) negotiate(ctx context.Context, offer hubsync.Capabilities) (hubsync.Capabilities, *hubsync.Error, error) {
	c := u.conn
	deadline := time.Now().Add(u.opts.RequestTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = c.c.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = c.c.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	if err := hubsync.WriteCapabilities(c.c, offer); err != nil {
		_ = c.c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		if _, perr, rerr := hubsync.ReadReply(c.br); rerr == nil && perr != nil {
			u.Close()
			u.protocolFailure(perr)
			return 0, nil, connError(perr)
		}
		u.Close()
		u.observer.Add("upload_failures", 1, "write")
		return 0, nil, u.disconnected(ctx, err)
	}
	caps, perr, err := hubsync.ReadWelcome(c.br)
	if err != nil {
		u.Close()
		u.observer.Add("upload_failures", 1, "read")
		return 0, nil, u.disconnected(ctx, err)
	}
	c.lastUsed = time.Now()
	return caps, perr, nil
}

type countWriter struct {
	w io.Writer
	n int64
}

func (cw *countWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	cw.n += int64(n)
	return n, err
}

func (u *Uploader) sendBatch(w io.Writer, b hubsync.Batch) error {
	cw := &countWriter{w: w}
	var err error
	if u.hubCaps&hubsync.CapZstdBatches != 0 {
		err = hubsync.WriteBatchCompressed(cw, b)
	} else {
		err = hubsync.WriteBatch(cw, b)
	}
	u.observer.Add("upload_wire_bytes", uint64(cw.n))
	return err
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
	start := time.Now()
	defer func() { u.observer.Observe("upload_round_trip_duration", time.Since(start)) }()
	deadline := time.Now().Add(u.opts.RequestTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = c.c.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = c.c.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	c.bw.Reset(c.c)
	err := write(c.bw)
	if err == nil {
		err = c.bw.Flush()
	}
	if err != nil {
		_ = c.c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		if _, perr, rerr := hubsync.ReadReply(c.br); rerr == nil && perr != nil {
			u.Close()
			u.protocolFailure(perr)
			return nil, connError(perr)
		}
		u.Close()
		u.observer.Add("upload_failures", 1, "write")
		return nil, u.disconnected(ctx, err)
	}
	perr, err := read(c.br)
	if err != nil {
		u.Close()
		u.observer.Add("upload_failures", 1, "read")
		return nil, u.disconnected(ctx, err)
	}
	c.lastUsed = time.Now()
	if perr != nil {
		switch perr.Code {
		case hubsync.CodeAuth, hubsync.CodeVersion, hubsync.CodeMalformed, hubsync.CodeTooLarge, hubsync.CodeBusy:
			u.protocolFailure(perr)
			u.Close()
			return nil, connError(perr)
		}
	}
	u.noteOnline()
	return perr, nil
}

func (u *Uploader) noteOnline() {
	if !u.offline {
		return
	}
	u.offline = false
	u.logf("reconnected to the hub; catching up from the retained files")
	u.event(observe.LevelInfo, observe.EventTransportOnline, "", nil)
}

func (u *Uploader) protocolFailure(perr *hubsync.Error) {
	switch perr.Code {
	case hubsync.CodeAuth:
		u.observer.Add("upload_failures", 1, "auth")
		u.event(observe.LevelWarn, observe.EventAuthFailure, "auth", nil)
	case hubsync.CodeBusy:
		u.observer.Add("upload_failures", 1, "retry")
	default:
		u.observer.Add("upload_failures", 1, "other")
	}
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
		u.event(observe.LevelWarn, observe.EventTransportOffline, "connect", nil)
	}
	return fmt.Errorf("%w: %v", ErrDisconnected, err)
}

func (u *Uploader) logf(format string, args ...any) {
	fmt.Fprintf(u.log, "ohya: "+format+"\n", args...)
}

func (u *Uploader) updateBacklog(outcomes []watch.Outcome) {
	var pending int64
	for _, o := range outcomes {
		if o.Missing || u.blocked[o.SourceID] {
			continue
		}
		if s, ok := u.stops[o.SourceID]; ok && s.offset == o.Cursor && s.end == o.End && s.modTime.Equal(o.ModTime) {
			continue
		}
		pending += o.End - o.Cursor
	}
	now := time.Now()
	switch {
	case u.drain == nil && pending > 0:
		u.drain = &backlogDrain{
			start:   now,
			left:    pending,
			total:   pending,
			samples: []drainSample{{at: now}},
			lastLog: now,
		}
		u.reportBacklog(now, false)
	case u.drain != nil && pending > 0:
		u.drain.left = pending
		u.reportBacklog(now, false)
	case u.drain != nil && pending == 0:
		u.reportBacklog(now, true)
		u.drain = nil
	}
}

func (u *Uploader) advanceBacklog(delta int64) {
	if u.drain == nil || delta <= 0 {
		return
	}
	now := time.Now()
	u.drain.sent += delta
	u.drain.left -= delta
	u.drain.samples = append(u.drain.samples, drainSample{at: now, sent: u.drain.sent})
	u.reportBacklog(now, false)
}

func (u *Uploader) reportBacklog(now time.Time, done bool) {
	d := u.drain
	if d == nil {
		return
	}
	cutoff := now.Add(-backlogRateWindow)
	keep := 0
	for i, s := range d.samples {
		if s.at.After(cutoff) {
			keep = i
			break
		}
	}
	d.samples = d.samples[keep:]

	var rate float64
	rateKnown := false
	if first, last := d.samples[0], d.samples[len(d.samples)-1]; len(d.samples) >= 2 && last.at.Sub(first.at) >= backlogRateMinSpan {
		rate = float64(last.sent-first.sent) / last.at.Sub(first.at).Seconds()
		rateKnown = true
	}
	left, sent := d.left, d.sent
	if done {
		left = 0
	}
	u.observer.Set("upload_backlog_bytes", float64(left), u.opts.Agent)
	u.observer.Set("upload_backlog_sent_bytes", float64(sent), u.opts.Agent)
	if done || !rateKnown || rate <= 0 || left <= 0 {
		u.observer.Set("upload_backlog_rate_bytes", 0, u.opts.Agent)
		u.observer.Set("upload_backlog_eta_ms", 0, u.opts.Agent)
	} else {
		eta := time.Duration(float64(left) / rate * float64(time.Second))
		u.observer.Set("upload_backlog_rate_bytes", rate, u.opts.Agent)
		u.observer.Set("upload_backlog_eta_ms", eta.Seconds()*1000, u.opts.Agent)
	}
	if d.total < backlogLogFloor {
		return
	}
	if !done && (sent == 0 || left <= 0) {
		return
	}
	if d.logged && now.Sub(d.lastLog) < backlogLogInterval && !done {
		return
	}
	d.lastLog = now
	d.logged = true
	agent := u.opts.Agent
	switch {
	case done:
		u.logf("upload backlog for %s: complete, %d bytes acknowledged in %s", agent, sent, now.Sub(d.start).Round(time.Second))
	case !rateKnown:
		u.logf("upload backlog for %s: %d of %d bytes acknowledged", agent, sent, d.total)
	case left > 0:
		eta := time.Duration(float64(left) / rate * float64(time.Second))
		u.logf("upload backlog for %s: %d of %d bytes acknowledged, %.0f bytes/s, about %s left",
			agent, sent, d.total, rate, eta.Round(time.Second))
	default:
		u.logf("upload backlog for %s: %d of %d bytes acknowledged, %.0f bytes/s", agent, sent, d.total, rate)
	}
}
