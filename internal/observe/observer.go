package observe

import "time"

type Observer interface {
	Add(name string, delta uint64, labelValues ...string)
	Set(name string, value float64, labelValues ...string)
	Observe(name string, d time.Duration, labelValues ...string)
}

type disabledObserver struct{}

func (disabledObserver) Add(string, uint64, ...string)            {}
func (disabledObserver) Set(string, float64, ...string)           {}
func (disabledObserver) Observe(string, time.Duration, ...string) {}

var Disabled Observer = disabledObserver{}

var _ Observer = (*Recorder)(nil)
