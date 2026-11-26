package hubsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/nibzard/ohya/internal/mcpserver"
	"github.com/nibzard/ohya/internal/observe"
	"github.com/nibzard/ohya/internal/store"
)

var ErrRemoteUnauthorized = errors.New("hubsync: the hub refused this device's credential")

type RemoteSessionOptions struct {
	DeviceID          string
	Usage             mcpserver.UsageSink
	Observer          observe.Observer
	KeepaliveInterval time.Duration
}

type RemoteSession struct {
	dial              func(ctx context.Context) (net.Conn, error)
	credential        string
	timeout           time.Duration
	keepaliveInterval time.Duration
	record            bool
	deviceID          string
	usage             mcpserver.UsageSink
	observer          observe.Observer

	loopCtx    context.Context
	loopCancel context.CancelFunc
	stopOnce   sync.Once
	doneCh     chan struct{}

	mu           sync.Mutex
	client       *RemoteClient
	lastUsed     time.Time
	keepaliveOff bool
	closed       bool
}

func NewRemoteSession(dial func(ctx context.Context) (net.Conn, error), credential string, opts RemoteSessionOptions) *RemoteSession {
	observer := opts.Observer
	if observer == nil {
		observer = observe.Disabled
	}
	s := &RemoteSession{
		dial:              dial,
		credential:        credential,
		timeout:           DefaultRemoteTimeout,
		keepaliveInterval: opts.KeepaliveInterval,
		record:            opts.Usage != nil || opts.Observer != nil,
		deviceID:          opts.DeviceID,
		usage:             opts.Usage,
		observer:          observer,
	}
	if opts.KeepaliveInterval > 0 {
		s.loopCtx, s.loopCancel = context.WithCancel(context.Background())
		s.doneCh = make(chan struct{})
		go s.keepaliveLoop()
	}
	return s
}

func (s *RemoteSession) Query(ctx context.Context, query string) (mcpserver.Result, error) {
	start := time.Now()
	var result mcpserver.Result
	err := s.call(ctx, func(c *RemoteClient) error {
		var cerr error
		result, cerr = c.Query(ctx, query)
		return cerr
	})
	if s.record {
		s.reportUsage(ctx, observe.UsageToolSQL, start, result, err)
	}
	return result, err
}

func (s *RemoteSession) AnnotateAs(ctx context.Context, _ string, raw json.RawMessage) (store.AnnotateResult, error) {
	start := time.Now()
	var result store.AnnotateResult
	err := s.call(ctx, func(c *RemoteClient) error {
		var cerr error
		result, cerr = c.Annotate(ctx, raw)
		return cerr
	})
	if s.record {
		s.reportUsage(ctx, observe.UsageToolAnnotate, start, mcpserver.Result{}, err)
	}
	return result, err
}

func (s *RemoteSession) Diagnostics(ctx context.Context) (RemoteStatus, error) {
	var result RemoteStatus
	err := s.call(ctx, func(c *RemoteClient) error {
		var cerr error
		result, cerr = c.Diagnostics(ctx)
		return cerr
	})
	return result, err
}

func (s *RemoteSession) Close() error {
	s.stopOnce.Do(func() {
		if s.loopCancel != nil {
			s.loopCancel()
		}
	})
	s.mu.Lock()
	s.closed = true
	err := s.dropLocked()
	s.mu.Unlock()
	if s.doneCh != nil {
		<-s.doneCh
	}
	return err
}

func (s *RemoteSession) keepaliveLoop() {
	defer close(s.doneCh)
	tick := max(min(s.keepaliveInterval/2, 30*time.Second), 5*time.Millisecond)
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	for {
		select {
		case <-s.loopCtx.Done():
			return
		case <-ticker.C:
			s.stepKeepalive()
		}
	}
}

func (s *RemoteSession) stepKeepalive() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.keepaliveOff || s.client == nil {
		return
	}
	if !s.client.Alive() {
		_ = s.dropLocked()
		return
	}
	if time.Since(s.lastUsed) < s.keepaliveInterval {
		return
	}
	kctx, cancel := context.WithTimeout(s.loopCtx, s.timeout)
	err := s.client.Keepalive(kctx)
	cancel()
	if err == nil {
		s.lastUsed = time.Now()
		return
	}
	if errors.Is(err, ErrKeepaliveUnsupported) {
		s.keepaliveOff = true
	}
	_ = s.dropLocked()
}

func (s *RemoteSession) call(ctx context.Context, exchange func(*RemoteClient) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("hubsync: remote session is closed")
	}
	if s.client != nil && !s.client.Alive() {
		_ = s.dropLocked()
	}
	if s.client == nil {
		if err := s.connectLocked(ctx); err != nil {
			return err
		}
	}
	err := exchange(s.client)
	if err == nil {
		s.lastUsed = time.Now()
		return nil
	}
	retry, mapped := s.finishAttemptLocked(ctx, err)
	if !retry {
		return mapped
	}
	_ = s.dropLocked()
	if err := s.connectLocked(ctx); err != nil {
		return err
	}
	err = exchange(s.client)
	if err == nil {
		s.lastUsed = time.Now()
		return nil
	}
	_, mapped = s.finishAttemptLocked(ctx, err)
	return mapped
}

func (s *RemoteSession) finishAttemptLocked(ctx context.Context, err error) (retry bool, mapped error) {
	if ctx.Err() != nil {
		_ = s.dropLocked()
		return false, err
	}
	if errors.Is(err, ErrTooLarge) || errors.Is(err, ErrRemoteStatusUnsupported) {
		return false, err
	}
	var perr *Error
	if errors.As(err, &perr) {
		switch perr.Code {
		case CodeAuth:
			_ = s.dropLocked()
			return false, fmt.Errorf("%w: %v", ErrRemoteUnauthorized, perr)
		case CodeVersion, CodeMalformed, CodeTooLarge, CodeBusy:
			_ = s.dropLocked()
		}
		return false, err
	}
	return true, err
}

func (s *RemoteSession) connectLocked(ctx context.Context) error {
	dctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	conn, err := s.dial(dctx)
	if err != nil {
		return fmt.Errorf("hubsync: remote session dial: %w", err)
	}
	deadline := time.Now().Add(s.timeout)
	if d, ok := dctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetWriteDeadline(deadline)
	if err := WriteHello(conn, s.credential); err != nil {
		conn.Close()
		return fmt.Errorf("hubsync: remote session hello: %w", err)
	}
	_ = conn.SetWriteDeadline(time.Time{})
	s.client = NewRemoteClient(conn)
	s.client.Timeout = s.timeout
	s.lastUsed = time.Now()
	return nil
}

func (s *RemoteSession) dropLocked() error {
	if s.client == nil {
		return nil
	}
	err := s.client.Close()
	s.client = nil
	return err
}

func (s *RemoteSession) reportUsage(ctx context.Context, tool string, start time.Time, result mcpserver.Result, err error) {
	outcome := remoteUsageOutcome(err, result)
	elapsed := time.Since(start)
	if s.usage != nil {
		origin := mcpserver.OriginFromContext(ctx)
		timeMS := time.Now().UnixMilli()
		record := observe.UsageRecord{
			SchemaVersion: observe.UsageFormatVersion,
			TimeMS:        timeMS,
			TimeBucket:    observe.HourBucket(timeMS),
			Tool:          tool,
			Client:        observe.MapClient(origin.Client),
			ClientVersion: observe.BucketVersion(origin.ClientVersion),
			Path:          observe.UsagePathRemote,
			DeviceID:      s.deviceID,
			SessionHint:   origin.SessionHint,
			Outcome:       outcome,
			PhaseMS:       observe.UsagePhases{TotalMS: float64(elapsed.Nanoseconds()) / 1e6},
		}
		if tool == observe.UsageToolSQL && err == nil {
			record.Rows = int64(result.RowCount)
		}
		if tool == observe.UsageToolAnnotate {
			record.AnnotateOps = &observe.AnnotateOps{}
		}
		s.usage.Record(record)
	}
	s.observer.Add("sql_requests", 1, tool, outcome)
	s.observer.Observe("sql_total_duration", elapsed)
	if err != nil {
		var perr *Error
		if errors.As(err, &perr) {
			if perr.Code == CodeBusy {
				s.observer.Add("sql_busy_rejections", 1)
			}
			if tool == observe.UsageToolAnnotate && perr.Code == CodeRejected {
				s.observer.Add("annotate_validation_failures", 1)
			}
		}
		return
	}
	if tool == observe.UsageToolSQL {
		s.observer.Add("sql_rows", uint64(result.RowCount))
		if result.Truncated {
			s.observer.Add("sql_truncations", 1, result.TruncationReason)
		}
	}
}

func remoteUsageOutcome(err error, result mcpserver.Result) string {
	switch {
	case err == nil:
		switch {
		case result.Truncated && result.TruncationReason == mcpserver.TruncationReasonBytes:
			return observe.UsageOutcomeTruncatedBytes
		case result.Truncated:
			return observe.UsageOutcomeTruncatedRows
		default:
			return observe.UsageOutcomeOK
		}
	case errors.Is(err, context.Canceled):
		return observe.UsageOutcomeCancelled
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return observe.UsageOutcomeTimeout
	case errors.Is(err, ErrRemoteUnauthorized), errors.Is(err, ErrTooLarge):
		return observe.UsageOutcomeRejected
	}
	var perr *Error
	if errors.As(err, &perr) {
		switch perr.Code {
		case CodeBusy:
			return observe.UsageOutcomeBusy
		case CodeRejected, CodeTooLarge:
			return observe.UsageOutcomeRejected
		}
	}
	return observe.UsageOutcomeSQLError
}
