package observe

import (
	"sync"
	"time"
)

type Observer interface {
	Add(name string, delta uint64, labelValues ...string)
	Set(name string, value float64, labelValues ...string)
	Observe(name string, d time.Duration, labelValues ...string)
}

type monotonicObserver interface {
	monotonicMS() int64
}

type disabledObserver struct{}

func (disabledObserver) Add(string, uint64, ...string)            {}
func (disabledObserver) Set(string, float64, ...string)           {}
func (disabledObserver) Observe(string, time.Duration, ...string) {}

var Disabled Observer = disabledObserver{}

var _ Observer = (*Recorder)(nil)

func (r *Recorder) monotonicMS() int64 {
	return r.clock.Monotonic().Milliseconds()
}

func RecordDiscoveryScan(o Observer, candidates int, elapsed time.Duration) {
	if o == nil {
		return
	}
	o.Add("discovery_scans", 1)
	o.Add("discovery_candidates", uint64(candidates))
	o.Observe("discovery_duration", elapsed)
	if m, ok := o.(monotonicObserver); ok {
		o.Set("discovery_last_scan_ms", float64(m.monotonicMS()))
	} else {
		o.Set("discovery_last_scan_ms", 0)
	}
}

type GaugeGroup struct {
	base  Observer
	mu    sync.Mutex
	slots map[string]map[string]float64
}

func NewGaugeGroup(base Observer) *GaugeGroup {
	if base == nil {
		base = Disabled
	}
	return &GaugeGroup{
		base:  base,
		slots: make(map[string]map[string]float64),
	}
}

func (g *GaugeGroup) Slot(slot string) Observer {
	if g == nil || g.base == nil || g.base == Disabled {
		return Disabled
	}
	return &slotObserver{group: g, slot: slot}
}

type slotObserver struct {
	group *GaugeGroup
	slot  string
}

func (s *slotObserver) Add(name string, delta uint64, labelValues ...string) {
	s.group.base.Add(name, delta, labelValues...)
}

func (s *slotObserver) Observe(name string, d time.Duration, labelValues ...string) {
	s.group.base.Observe(name, d, labelValues...)
}

func (s *slotObserver) Set(name string, value float64, labelValues ...string) {
	if !isSlotSummedGauge(name) {
		s.group.base.Set(name, value, labelValues...)
		return
	}
	key := SeriesKey(name, labelValues...)
	s.group.mu.Lock()
	perSlot := s.group.slots[key]
	if perSlot == nil {
		perSlot = make(map[string]float64)
		s.group.slots[key] = perSlot
	}
	perSlot[s.slot] = value
	sum := 0.0
	for _, v := range perSlot {
		sum += v
	}
	s.group.mu.Unlock()
	s.group.base.Set(name, sum, labelValues...)
}

func (s *slotObserver) monotonicMS() int64 {
	if m, ok := s.group.base.(monotonicObserver); ok {
		return m.monotonicMS()
	}
	return 0
}

func isSlotSummedGauge(name string) bool {
	switch name {
	case "watch_sources", "watch_partial_tail_bytes", "watch_pending_complete_bytes":
		return true
	default:
		return false
	}
}
