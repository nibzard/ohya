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
	DeviceID string
	Usage    mcpserver.UsageSink
	Observer observe.Observer
}

type RemoteSession struct {
	dial       func(ctx context.Context) (net.Conn, error)
	credential string
	timeout    time.Duration
	record     bool
	deviceID   string
	usage      mcpserver.UsageSink
	observer   observe.Observer

	mu     sync.Mutex
	client *RemoteClient
}

func NewRemoteSession(dial func(ctx context.Context) (net.Conn, error), credential string, opts RemoteSessionOptions) *RemoteSession {
	observer := opts.Observer
	if observer == nil {
		observer = observe.Disabled
	}
	return &RemoteSession{
		dial:       dial,
		credential: credential,
		timeout:    DefaultRemoteTimeout,
		record:     opts.Usage != nil || opts.Observer != nil,
		deviceID:   opts.DeviceID,
		usage:      opts.Usage,
		observer:   observer,
	}
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

func (s *RemoteSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropLocked()
}

func (s *RemoteSession) call(ctx context.Context, exchange func(*RemoteClient) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client == nil {
		if err := s.connectLocked(ctx); err != nil {
			return err
		}
	}
	err := exchange(s.client)
	if err == nil {
		return nil
	}
	var perr *Error
	if errors.As(err, &perr) {
		if perr.Code == CodeAuth {
			return fmt.Errorf("%w: %v", ErrRemoteUnauthorized, perr)
		}
		return err
	}
	_ = s.dropLocked()
	if err := s.connectLocked(ctx); err != nil {
		return err
	}
	return exchange(s.client)
}

func (s *RemoteSession) connectLocked(ctx context.Context) error {
	dctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	conn, err := s.dial(dctx)
	if err != nil {
		return fmt.Errorf("hubsync: remote session dial: %w", err)
	}
	if err := WriteHello(conn, s.credential); err != nil {
		conn.Close()
		return fmt.Errorf("hubsync: remote session hello: %w", err)
	}
	s.client = NewRemoteClient(conn)
	s.client.Timeout = s.timeout
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
