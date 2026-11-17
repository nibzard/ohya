package observe

import "sort"

type Window struct {
	InstanceID          string            `json:"instance_id"`
	SameInstance        bool              `json:"same_instance"`
	ElapsedMS           *int64            `json:"elapsed_ms"`
	Counters            map[string]uint64 `json:"counters"`
	UnavailableCounters []string          `json:"unavailable_counters"`
}

func Delta(after, before *Snapshot) Window {
	if after.Instance.InstanceID != before.Instance.InstanceID {
		return Window{InstanceID: after.Instance.InstanceID, SameInstance: false}
	}
	window := Window{InstanceID: after.Instance.InstanceID, SameInstance: true}
	elapsed := after.UptimeMS - before.UptimeMS
	if elapsed >= 0 {
		window.ElapsedMS = &elapsed
	}
	window.Counters = make(map[string]uint64, len(after.Counters))
	for key, value := range after.Counters {
		previous, ok := before.Counters[key]
		if !ok || value < previous {
			window.UnavailableCounters = append(window.UnavailableCounters, key)
			continue
		}
		window.Counters[key] = value - previous
	}
	sort.Strings(window.UnavailableCounters)
	return window
}
