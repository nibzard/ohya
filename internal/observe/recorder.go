package observe

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

type Reading struct {
	Value  *float64 `json:"value"`
	Reason string   `json:"reason,omitempty"`
}

func Supported(value float64) Reading {
	return Reading{Value: &value}
}

func Unsupported(reason string) Reading {
	return Reading{Reason: reason}
}

func (r Reading) Validate() error {
	if r.Value == nil {
		if r.Reason == "" {
			return errors.New("observe: unsupported reading needs a reason")
		}
		return nil
	}
	if r.Reason != "" {
		return errors.New("observe: supported reading must not carry a reason")
	}
	return nil
}

type Snapshot struct {
	FormatVersion int                      `json:"format_version"`
	Instance      Instance                 `json:"instance"`
	UptimeMS      int64                    `json:"uptime_ms"`
	SampledWallMS int64                    `json:"sampled_wall_ms"`
	Counters      map[string]uint64        `json:"counters"`
	Gauges        map[string]float64       `json:"gauges"`
	Durations     map[string]HistogramData `json:"durations"`
	Resources     map[string]Reading       `json:"resources"`
	Truncated     bool                     `json:"truncated,omitempty"`
	Stopped       bool                     `json:"stopped,omitempty"`
}

type Recorder struct {
	mu        sync.Mutex
	instance  Instance
	clock     Clock
	counters  map[string]uint64
	gauges    map[string]float64
	durations map[string]*histogram
	resources map[string]Reading
	dropped   uint64
}

func NewRecorder(instance Instance, clock Clock) (*Recorder, error) {
	if err := instance.Validate(); err != nil {
		return nil, err
	}
	if clock == nil {
		return nil, errors.New("observe: recorder needs a clock")
	}
	return &Recorder{
		instance:  instance,
		clock:     clock,
		counters:  make(map[string]uint64),
		gauges:    make(map[string]float64),
		durations: make(map[string]*histogram),
		resources: make(map[string]Reading),
	}, nil
}

func (r *Recorder) Instance() Instance {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.instance
}

func (r *Recorder) Add(name string, delta uint64, labelValues ...string) {
	key, ok := r.resolve(name, Counter, labelValues)
	if !ok {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counters[key] += delta
}

func (r *Recorder) Set(name string, value float64, labelValues ...string) {
	key, ok := r.resolve(name, Gauge, labelValues)
	if !ok {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gauges[key] = value
}

func (r *Recorder) Observe(name string, d time.Duration, labelValues ...string) {
	key, ok := r.resolve(name, Duration, labelValues)
	if !ok {
		return
	}
	if d < 0 {
		r.mu.Lock()
		r.dropped++
		r.mu.Unlock()
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	h := r.durations[key]
	if h == nil {
		h = newHistogram()
		r.durations[key] = h
	}
	h.observe(d)
}

func (r *Recorder) Resource(name string, reading Reading) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resources[name] = reading
}

func (r *Recorder) DroppedMetrics() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dropped
}

func (r *Recorder) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	counters := make(map[string]uint64, len(r.counters))
	for key, value := range r.counters {
		counters[key] = value
	}
	gauges := make(map[string]float64, len(r.gauges))
	for key, value := range r.gauges {
		gauges[key] = value
	}
	durations := make(map[string]HistogramData, len(r.durations))
	for key, h := range r.durations {
		durations[key] = h.data()
	}
	resources := make(map[string]Reading, len(r.resources))
	for name, reading := range r.resources {
		resources[name] = reading
	}
	return Snapshot{
		FormatVersion: SnapshotFormatVersion,
		Instance:      r.instance,
		UptimeMS:      r.clock.Monotonic().Milliseconds(),
		SampledWallMS: r.clock.WallMS(),
		Counters:      counters,
		Gauges:        gauges,
		Durations:     durations,
		Resources:     resources,
	}
}

func (r *Recorder) resolve(name string, kind MetricKind, labelValues []string) (string, bool) {
	metric, ok := catalogIndex[name]
	if !ok || metric.Kind != kind || len(labelValues) != len(metric.Labels) {
		r.mu.Lock()
		r.dropped++
		r.mu.Unlock()
		return "", false
	}
	if len(metric.Labels) == 0 {
		return name, true
	}
	rendered := make([]string, len(metric.Labels))
	for i, labelName := range metric.Labels {
		set, declared := labelSets[labelName]
		if !declared {
			r.mu.Lock()
			r.dropped++
			r.mu.Unlock()
			return "", false
		}
		value := labelValues[i]
		if !labelContains(set.Values, value) {
			value = set.Fallback
		}
		rendered[i] = labelName + "=" + value
	}
	return name + "{" + strings.Join(rendered, ",") + "}", true
}

func labelContains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

func (s Snapshot) Validate() error {
	if s.FormatVersion != SnapshotFormatVersion {
		return fmt.Errorf("observe: snapshot format version %d is unsupported, want %d", s.FormatVersion, SnapshotFormatVersion)
	}
	if err := s.Instance.Validate(); err != nil {
		return err
	}
	if s.UptimeMS < 0 {
		return errors.New("observe: snapshot uptime must not be negative")
	}
	if s.SampledWallMS <= 0 {
		return errors.New("observe: snapshot needs a sample timestamp")
	}
	for name, reading := range s.Resources {
		if err := reading.Validate(); err != nil {
			return fmt.Errorf("observe: resource %s: %w", name, err)
		}
	}
	return nil
}
