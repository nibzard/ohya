package observe

import (
	"math"
	"testing"
	"time"
)

func TestHistogramBucketAssignment(t *testing.T) {
	cases := []struct {
		ms     float64
		bucket int
	}{
		{0.01, 0},
		{0.1, 0},
		{0.11, 1},
		{0.5, 1},
		{0.51, 2},
		{1, 2},
		{2, 3},
		{5, 4},
		{10, 5},
		{25, 6},
		{50, 7},
		{100, 8},
		{250, 9},
		{500, 10},
		{1000, 11},
		{2500, 12},
		{5000, 13},
		{10000, 14},
		{30000, 15},
		{60000, 16},
		{60000.1, 17},
		{1e9, 17},
	}
	for _, tc := range cases {
		if got := histogramBucket(tc.ms); got != tc.bucket {
			t.Errorf("bucket(%g) = %d, want %d", tc.ms, got, tc.bucket)
		}
	}
}

func TestHistogramSumAndCount(t *testing.T) {
	h := newHistogram()
	h.observe(100 * time.Microsecond)
	h.observe(1500 * time.Microsecond)
	h.observe(2 * time.Second)
	data := h.data()
	if data.Count != 3 {
		t.Fatalf("count = %d, want 3", data.Count)
	}
	wantSum := 0.1 + 1.5 + 2000
	if math.Abs(data.SumMS-wantSum) > 1e-9 {
		t.Errorf("sum = %f, want %f", data.SumMS, wantSum)
	}
	var total uint64
	for _, bucket := range data.Buckets {
		total += bucket
	}
	if total != 3 {
		t.Errorf("bucket total = %d, want 3", total)
	}
	if data.Buckets[0] != 1 || data.Buckets[3] != 1 || data.Buckets[12] != 1 {
		t.Errorf("bucket placement wrong: %v", data.Buckets)
	}
}

func TestHistogramIgnoresNegativeDurations(t *testing.T) {
	h := newHistogram()
	h.observe(-time.Second)
	data := h.data()
	if data.Count != 0 || data.SumMS != 0 {
		t.Errorf("negative duration must not be counted: %+v", data)
	}
}

func TestQuantileNeedsMinimumSamples(t *testing.T) {
	h := newHistogram()
	for i := 0; i < MinQuantileSamples-1; i++ {
		h.observe(time.Millisecond)
	}
	if _, ok := h.data().Quantile(0.5); ok {
		t.Fatalf("quantile below %d observations must be unavailable", MinQuantileSamples)
	}
	h.observe(time.Millisecond)
	estimate, ok := h.data().Quantile(0.5)
	if !ok {
		t.Fatal("quantile at the minimum sample count must be available")
	}
	if estimate < 0.5 || estimate > 1 {
		t.Errorf("quantile estimate %f outside its bucket (0.5, 1]", estimate)
	}
}

func TestQuantileIsBucketEstimate(t *testing.T) {
	h := newHistogram()
	for i := 0; i < 50; i++ {
		h.observe(3 * time.Millisecond)
	}
	data := h.data()
	for _, p := range []float64{0.1, 0.5, 0.9, 1.0} {
		estimate, ok := data.Quantile(p)
		if !ok {
			t.Fatalf("quantile(%g) unavailable", p)
		}
		want := 2 + (5-2)*p
		if math.Abs(estimate-want) > 1e-9 {
			t.Errorf("quantile(%g) = %f, want interpolated estimate %f", p, estimate, want)
		}
	}
	earlier, _ := data.Quantile(0.2)
	later, _ := data.Quantile(0.8)
	if later < earlier {
		t.Errorf("quantiles must be non-decreasing in p: %f then %f", earlier, later)
	}
}

func TestQuantileAboveLastBoundReportsLastBound(t *testing.T) {
	h := newHistogram()
	for i := 0; i < 25; i++ {
		h.observe(90 * time.Second)
	}
	estimate, ok := h.data().Quantile(0.5)
	if !ok {
		t.Fatal("quantile unavailable")
	}
	if estimate != DurationBoundsMS[len(DurationBoundsMS)-1] {
		t.Errorf("estimate above the last finite bound = %f, want the last bound %f", estimate, DurationBoundsMS[len(DurationBoundsMS)-1])
	}
}

func TestQuantileRejectsInvalidProbability(t *testing.T) {
	h := newHistogram()
	for i := 0; i < 25; i++ {
		h.observe(time.Millisecond)
	}
	data := h.data()
	for _, p := range []float64{-0.01, 1.01, math.NaN()} {
		if _, ok := data.Quantile(p); ok {
			t.Errorf("quantile(%g) must be unavailable", p)
		}
	}
}

func TestQuantileRejectsMalformedHistogram(t *testing.T) {
	data := HistogramData{BoundsMS: DurationBoundsMS, Buckets: make([]uint64, 3), Count: 100}
	if _, ok := data.Quantile(0.5); ok {
		t.Error("bucket count mismatch must make quantiles unavailable")
	}
}
