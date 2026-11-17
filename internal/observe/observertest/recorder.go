package observertest

import (
	"sync"
	"time"

	"github.com/nibzard/ohya/internal/observe"
)

type Recorder struct {
	mu         sync.Mutex
	counters   map[string]uint64
	gauges     map[string]float64
	durations  map[string]int
	unknownAdd int
}

func New() *Recorder {
	return &Recorder{
		counters:  map[string]uint64{},
		gauges:    map[string]float64{},
		durations: map[string]int{},
	}
}

func (r *Recorder) Add(name string, delta uint64, labelValues ...string) {
	if _, ok := observe.MetricByName(name); !ok || !labelArityMatches(name, labelValues) {
		r.mu.Lock()
		r.unknownAdd++
		r.mu.Unlock()
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counters[observe.SeriesKey(name, labelValues...)] += delta
}

func (r *Recorder) Set(name string, value float64, labelValues ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gauges[observe.SeriesKey(name, labelValues...)] = value
}

func (r *Recorder) Observe(name string, d time.Duration, labelValues ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.durations[observe.SeriesKey(name, labelValues...)]++
}

func (r *Recorder) Counter(name string, labelValues ...string) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counters[observe.SeriesKey(name, labelValues...)]
}

func (r *Recorder) Gauge(name string, labelValues ...string) float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gauges[observe.SeriesKey(name, labelValues...)]
}

func (r *Recorder) DurationCount(name string, labelValues ...string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.durations[observe.SeriesKey(name, labelValues...)]
}

func (r *Recorder) AwaitDurationCount(name string, want int, timeout time.Duration, labelValues ...string) bool {
	deadline := time.Now().Add(timeout)
	for {
		if r.DurationCount(name, labelValues...) >= want {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
}

func (r *Recorder) UnknownAddCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.unknownAdd
}

func labelArityMatches(name string, labelValues []string) bool {
	metric, ok := observe.MetricByName(name)
	return ok && len(metric.Labels) == len(labelValues)
}
