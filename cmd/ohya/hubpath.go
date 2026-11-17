package main

import (
	"context"
	"net"
	"sync"
	"time"

	"github.com/nibzard/ohya/internal/registry"
	"github.com/nibzard/ohya/internal/transport/address"
)

const deviceSyncRefresh = 30 * time.Second

type pathRecorder struct {
	inner *registry.Authenticator
	hub   *address.Hub
	db    *registry.DB
	logf  func(string, ...any)

	mu       sync.Mutex
	remote   map[string]net.Addr
	recorded map[string]time.Time
	reported bool
}

func newPathRecorder(inner *registry.Authenticator, hub *address.Hub, db *registry.DB, logf func(string, ...any)) *pathRecorder {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &pathRecorder{
		inner: inner, hub: hub, db: db, logf: logf,
		remote: map[string]net.Addr{}, recorded: map[string]time.Time{},
	}
}

func (p *pathRecorder) Authenticate(ctx context.Context, conn net.Conn) (string, error) {
	deviceID, err := p.inner.Authenticate(ctx, conn)
	if err != nil {
		return deviceID, err
	}
	p.mu.Lock()
	p.remote[deviceID] = conn.RemoteAddr()
	p.mu.Unlock()
	p.record(ctx, deviceID)
	return deviceID, nil
}

func (p *pathRecorder) Authorize(ctx context.Context, deviceID string) error {
	if err := p.inner.Authorize(ctx, deviceID); err != nil {
		return err
	}
	p.mu.Lock()
	due := time.Since(p.recorded[deviceID]) >= deviceSyncRefresh
	p.mu.Unlock()
	if due {
		p.record(ctx, deviceID)
	}
	return nil
}

func (p *pathRecorder) WatchRevocations(ctx context.Context, fn func(deviceID string)) error {
	return p.inner.WatchRevocations(ctx, fn)
}

func (p *pathRecorder) record(ctx context.Context, deviceID string) {
	p.mu.Lock()
	remote := p.remote[deviceID]
	p.mu.Unlock()
	if remote == nil {
		return
	}
	report := p.hub.PeerPath(remote)
	if err := p.db.RecordSync(ctx, deviceID, report.Path, time.Now().UnixMilli()); err != nil {
		p.mu.Lock()
		first := !p.reported
		p.reported = true
		p.mu.Unlock()
		if first {
			p.logf("record device sync state: %v", err)
		}
		return
	}
	p.mu.Lock()
	p.recorded[deviceID] = time.Now()
	p.mu.Unlock()
}
