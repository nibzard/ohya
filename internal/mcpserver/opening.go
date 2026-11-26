package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/nibzard/ohya/internal/store"
)

var (
	ErrOpening    = errors.New("the hub database is still opening: another process holds its write lock, for example for a migration, which can take minutes on a large hub; try again later")
	ErrClosed     = errors.New("mcpserver: backend is closed")
	errNilBackend = errors.New("mcpserver: backend is unavailable")
)

type openingPhase uint8

const (
	openingPending openingPhase = iota
	openingReady
	openingFailed
	openingClosed
)

type Opening struct {
	deadline  time.Duration
	done      chan struct{}
	cancel    context.CancelFunc
	closeOnce sync.Once

	mu      sync.Mutex
	phase   openingPhase
	backend Backend
	closeFn func()
	err     error
}

func OpenInBackground(ctx context.Context, o EngineOptions, open func(context.Context) (Backend, func(), error)) *Opening {
	ctx, cancel := context.WithCancel(ctx)
	o = o.withDefaults()
	opening := &Opening{
		deadline: o.Deadline,
		done:     make(chan struct{}),
		cancel:   cancel,
		phase:    openingPending,
	}
	go func() {
		defer close(opening.done)
		backend, closeFn, err := open(ctx)
		if err == nil && backend == nil {
			err = errNilBackend
		}
		opening.mu.Lock()
		defer opening.mu.Unlock()
		opening.err = err
		opening.closeFn = closeFn
		if opening.phase == openingClosed {
			return
		}
		if err != nil {
			opening.phase = openingFailed
			return
		}
		opening.backend = backend
		opening.phase = openingReady
	}()
	return opening
}

func (o *Opening) state() (Backend, bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	switch o.phase {
	case openingPending:
		return nil, false, nil
	case openingReady:
		return o.backend, true, nil
	case openingFailed:
		return nil, true, o.err
	case openingClosed:
		return nil, true, ErrClosed
	default:
		return nil, true, ErrClosed
	}
}

func (o *Opening) ready(ctx context.Context) (Backend, error) {
	if backend, done, err := o.state(); done {
		return backend, err
	}
	timer := time.NewTimer(o.deadline)
	defer timer.Stop()
	select {
	case <-o.done:
		backend, _, err := o.state()
		return backend, err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		if backend, done, err := o.state(); done {
			return backend, err
		}
		return nil, ErrOpening
	}
}

func (o *Opening) Query(ctx context.Context, query string) (Result, error) {
	backend, err := o.ready(ctx)
	if err != nil {
		return Result{}, err
	}
	return backend.Query(ctx, query)
}

func (o *Opening) AnnotateAs(ctx context.Context, writerID string, raw json.RawMessage) (store.AnnotateResult, error) {
	backend, err := o.ready(ctx)
	if err != nil {
		return store.AnnotateResult{}, err
	}
	return backend.AnnotateAs(ctx, writerID, raw)
}

func (o *Opening) Wait() error {
	<-o.done
	return o.err
}

func (o *Opening) Close() error {
	o.closeOnce.Do(func() {
		o.mu.Lock()
		o.phase = openingClosed
		o.mu.Unlock()
		o.cancel()
		<-o.done
		if o.closeFn != nil {
			o.closeFn()
		}
	})
	return o.err
}
