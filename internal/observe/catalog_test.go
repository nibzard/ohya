package observe

import (
	"fmt"
	"testing"
	"time"
)

func TestCatalogNamesAreUnique(t *testing.T) {
	seen := make(map[string]bool, len(Catalog))
	for _, metric := range Catalog {
		if seen[metric.Name] {
			t.Errorf("duplicate metric name %s", metric.Name)
		}
		seen[metric.Name] = true
		if metric.Name == "" || metric.Help == "" {
			t.Errorf("metric %q needs a name and help text", metric.Name)
		}
		if metric.Kind != Counter && metric.Kind != Gauge && metric.Kind != Duration {
			t.Errorf("metric %s has unknown kind %d", metric.Name, metric.Kind)
		}
	}
}

func TestCatalogLabelsAreDeclaredWithFallback(t *testing.T) {
	for _, metric := range Catalog {
		for _, labelName := range metric.Labels {
			set, ok := labelSets[labelName]
			if !ok {
				t.Errorf("metric %s uses undeclared label %s", metric.Name, labelName)
				continue
			}
			fallbackKnown := false
			for _, value := range set.Values {
				if value == set.Fallback {
					fallbackKnown = true
				}
			}
			if !fallbackKnown {
				t.Errorf("label %s has fallback %q outside its value set", labelName, set.Fallback)
			}
		}
	}
}

func TestCatalogLabelValuesStayBounded(t *testing.T) {
	for name, set := range labelSets {
		if len(set.Values) > 16 {
			t.Errorf("label %s declares %d values, above the bounded-cardinality ceiling", name, len(set.Values))
		}
	}
}

func TestDurationMetricsUseFixedBounds(t *testing.T) {
	recorder := testRecorder(t)
	for _, metric := range Catalog {
		if metric.Kind != Duration {
			continue
		}
		values := make([]string, len(metric.Labels))
		recorder.Observe(metric.Name, time.Millisecond, values...)
	}
	snapshot := recorder.Snapshot()
	for name, data := range snapshot.Durations {
		if len(data.Buckets) != len(DurationBoundsMS)+1 {
			t.Errorf("duration %s has %d buckets, want %d", name, len(data.Buckets), len(DurationBoundsMS)+1)
		}
		for i, bound := range data.BoundsMS {
			if bound != DurationBoundsMS[i] {
				t.Errorf("duration %s bound %d = %f, want %f", name, i, bound, DurationBoundsMS[i])
			}
		}
	}
}

func TestSeriesBoundMatchesCatalogue(t *testing.T) {
	want := 0
	for _, metric := range Catalog {
		series := 1
		for _, labelName := range metric.Labels {
			series *= len(labelSets[labelName].Values)
		}
		want += series
	}
	if SeriesBound() != want {
		t.Fatalf("SeriesBound = %d, want %d", SeriesBound(), want)
	}
	if want > 4096 {
		t.Fatalf("catalogue series bound %d exceeds the fixed catalogue ceiling", want)
	}
}

func TestMetricByNameMirrorsCatalogue(t *testing.T) {
	for _, metric := range Catalog {
		found, ok := MetricByName(metric.Name)
		if !ok || found.Name != metric.Name {
			t.Errorf("MetricByName(%s) missing", metric.Name)
		}
	}
	if _, ok := MetricByName(fmt.Sprintf("nope-%d", time.Now().UnixNano())); ok {
		t.Error("unknown metric must not resolve")
	}
}
