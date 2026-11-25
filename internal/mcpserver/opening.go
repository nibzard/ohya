package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/nibzard/ohya/internal/store"
)

var ErrOpening = errors.New("the hub database is still opening: another process holds its write lock, for example for a migration, which can take minutes on a large hub; try again later")

type Opening struct {
	deadline time.Duration
	done     chan struct{}
	cancel   context.CancelFunc
	backend  Backend
	close    func()
	err      error
}

func OpenInBackground(ctx context.Context, o EngineOptions, open func(context.Context) (Backend, func(), error)) *Opening {
	ctx, cancel := context.WithCancel(ctx)
	o = o.withDefaults()
	opening := &Opening{deadline: o.Deadline, done: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(opening.done)
		opening.backend, opening.close, opening.err = open(ctx)
	}()
	return opening
}

func (o *Opening) ready(ctx context.Context) (Backend, error) {
	select {
	case <-o.done:
		return o.backend, o.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(o.deadline):
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
	<-o.done
	o.cancel()
	if o.close != nil {
		o.close()
	}
	return o.err
}
