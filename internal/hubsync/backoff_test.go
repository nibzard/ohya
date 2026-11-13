package hubsync

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRetryPolicyWait(t *testing.T) {
	p := RetryPolicy{Base: 10 * time.Millisecond, Cap: 80 * time.Millisecond}
	for _, tc := range []struct {
		attempt int
		want    time.Duration
	}{
		{1, 10 * time.Millisecond},
		{2, 20 * time.Millisecond},
		{3, 40 * time.Millisecond},
		{4, 80 * time.Millisecond},
		{9, 80 * time.Millisecond}, // capped
	} {
		if got := p.Wait(tc.attempt); got != tc.want {
			t.Fatalf("Wait(%d) = %v; want %v", tc.attempt, got, tc.want)
		}
	}
	// Unset fields select the defaults.
	if got := (RetryPolicy{}).Wait(1); got != DefaultRetryBase {
		t.Fatalf("default Wait(1) = %v; want %v", got, DefaultRetryBase)
	}
	if got := (RetryPolicy{}).Wait(99); got != DefaultRetryCap {
		t.Fatalf("default Wait(99) = %v; want %v", got, DefaultRetryCap)
	}
}

func TestRetrySucceedsAfterTransientFailures(t *testing.T) {
	p := RetryPolicy{Base: time.Millisecond, Cap: 2 * time.Millisecond, Attempts: 4}
	calls := 0
	start := time.Now()
	err := Retry(context.Background(), p, nil, func() error {
		calls++
		if calls < 3 {
			return errors.New("transient")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("retry = %v", err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d; want 3", calls)
	}
	if elapsed := time.Since(start); elapsed < 3*time.Millisecond {
		t.Fatalf("retry skipped its backoff: %v", elapsed)
	}
}

var errPermanentTest = errors.New("permanent")

func TestRetryStopsOnPermanentError(t *testing.T) {
	p := RetryPolicy{Base: time.Millisecond, Cap: time.Millisecond, Attempts: 5}
	calls := 0
	permanent := func(err error) bool { return errors.Is(err, errPermanentTest) }
	err := Retry(context.Background(), p, permanent, func() error {
		calls++
		return errPermanentTest
	})
	if !errors.Is(err, errPermanentTest) {
		t.Fatalf("retry = %v; want the permanent error", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d; want 1 (a permanent error never retries)", calls)
	}
}

func TestRetryExhaustsAndStopsOnCanceledContext(t *testing.T) {
	p := RetryPolicy{Base: time.Millisecond, Cap: time.Millisecond, Attempts: 2}
	calls := 0
	err := Retry(context.Background(), p, nil, func() error {
		calls++
		return errors.New("busy")
	})
	if err == nil || calls != 2 {
		t.Fatalf("exhausted retry = %v after %d calls", err, calls)
	}

	// A canceled context stops the loop before the first sleep, however long
	// the configured backoff is.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls = 0
	err = Retry(ctx, RetryPolicy{Base: time.Hour, Cap: time.Hour, Attempts: 9}, nil, func() error {
		calls++
		return errors.New("busy")
	})
	if err == nil || calls != 1 {
		t.Fatalf("canceled retry = %v after %d calls", err, calls)
	}

	// Cancellation mid-backoff also stops the loop instead of sleeping out
	// the full wait.
	ctx, cancel = context.WithCancel(context.Background())
	calls = 0
	go func() {
		time.Sleep(2 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	err = Retry(ctx, RetryPolicy{Base: 50 * time.Millisecond, Cap: 50 * time.Millisecond, Attempts: 9}, nil, func() error {
		calls++
		return errors.New("busy")
	})
	if err == nil || calls != 1 {
		t.Fatalf("mid-backoff cancel = %v after %d calls", err, calls)
	}
	if elapsed := time.Since(start); elapsed > 40*time.Millisecond {
		t.Fatalf("mid-backoff cancel slept %v", elapsed)
	}
}
