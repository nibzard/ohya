// Capped exponential backoff for retrying retryable failures. The hub uses
// it for transient write-path failures (an external process holding the
// database lock past the busy timeout); the device uploader reuses the same
// policy for reconnects and resends. Backoff is deterministic — no jitter —
// so tests and traces stay reproducible.

package hubsync

import (
	"context"
	"fmt"
	"time"
)

// Retry policy defaults: three attempts waiting 100 ms, then 200 ms between
// the rest, capped at 2 s. Transient contention clears well inside that; a
// longer outage surfaces to the device, which retries with its own policy.
const (
	DefaultRetryBase     = 100 * time.Millisecond
	DefaultRetryCap      = 2 * time.Second
	DefaultRetryAttempts = 3
)

// RetryPolicy bounds one retry loop.
type RetryPolicy struct {
	// Base is the wait before the second attempt. It doubles per attempt up
	// to Cap. Values <= 0 select DefaultRetryBase.
	Base time.Duration
	// Cap bounds one wait. Values <= 0 select DefaultRetryCap.
	Cap time.Duration
	// Attempts is the total number of runs of the operation, including the
	// first. Values <= 0 select DefaultRetryAttempts.
	Attempts int
}

// normalized replaces unset fields with the defaults.
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

// Wait returns the pause before the retry with the given 1-based attempt
// number: Base doubled attempt-1 times, capped.
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

// Retry runs fn until it succeeds, reports a permanent error, or exhausts
// the attempts. permanent classifies errors that retrying cannot fix; when
// it is nil every error retries. A canceled context stops the loop and
// returns the context error wrapped over the last failure.
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
