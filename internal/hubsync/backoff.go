package hubsync

import (
	"context"
	"fmt"
	"time"
)

const (
	DefaultRetryBase     = 100 * time.Millisecond
	DefaultRetryCap      = 2 * time.Second
	DefaultRetryAttempts = 3
)

type RetryPolicy struct {
	Base     time.Duration
	Cap      time.Duration
	Attempts int
}

func (p RetryPolicy) normalized() RetryPolicy {
	if p.Base <= 0 {
		p.Base = DefaultRetryBase
	}
	if p.Cap <= 0 {
		p.Cap = DefaultRetryCap
	}
	if p.Attempts <= 0 {
		p.Attempts = DefaultRetryAttempts
	}
	return p
}

func (p RetryPolicy) Wait(attempt int) time.Duration {
	p = p.normalized()
	wait := p.Base
	for i := 1; i < attempt && wait < p.Cap; i++ {
		wait *= 2
	}
	if wait > p.Cap {
		wait = p.Cap
	}
	return wait
}

func Retry(ctx context.Context, policy RetryPolicy, permanent func(error) bool, fn func() error) error {
	policy = policy.normalized()
	var err error
	for attempt := 1; ; attempt++ {
		if err = fn(); err == nil {
			return nil
		}
		if permanent != nil && permanent(err) {
			return err
		}
		if ctx.Err() != nil {
			return fmt.Errorf("hubsync: retry stopped: %w (last error: %v)", ctx.Err(), err)
		}
		if attempt >= policy.Attempts {
			return fmt.Errorf("hubsync: retry exhausted after %d attempts: %w", attempt, err)
		}
		timer := time.NewTimer(policy.Wait(attempt))
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("hubsync: retry stopped: %w (last error: %v)", ctx.Err(), err)
		case <-timer.C:
		}
	}
}
