package observe

import (
	"sync"
	"time"
)

type Clock interface {
	Monotonic() time.Duration
	WallMS() int64
}

type SystemClock struct {
	start time.Time
}

func NewSystemClock() *SystemClock {
	return &SystemClock{start: time.Now()}
}

func (c *SystemClock) Monotonic() time.Duration { return time.Since(c.start) }

func (c *SystemClock) WallMS() int64 { return time.Now().UnixMilli() }

type FakeClock struct {
	mu   sync.Mutex
	mono time.Duration
	wall int64
}

func NewFakeClock(wallMS int64) *FakeClock {
	return &FakeClock{wall: wallMS}
}

func (c *FakeClock) Monotonic() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mono
}

func (c *FakeClock) WallMS() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wall
}

func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mono += d
	c.wall += d.Milliseconds()
}

func (c *FakeClock) SetWallMS(ms int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wall = ms
}
