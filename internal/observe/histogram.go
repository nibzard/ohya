package observe

import (
	"math"
	"sort"
	"time"
)

var DurationBoundsMS = []float64{0.1, 0.5, 1, 2, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000, 10000, 30000, 60000}

const MinQuantileSamples = 20

type HistogramData struct {
	BoundsMS []float64 `json:"bounds_ms"`
	Buckets  []uint64  `json:"buckets"`
	Count    uint64    `json:"count"`
	SumMS    float64   `json:"sum_ms"`
}

func (d HistogramData) Quantile(p float64) (float64, bool) {
	if d.Count < MinQuantileSamples || p < 0 || p > 1 || math.IsNaN(p) || len(d.Buckets) != len(d.BoundsMS)+1 {
		return 0, false
	}
	rank := p * float64(d.Count)
	if rank < 1 {
		rank = 1
	}
	cumulative := uint64(0)
	for i, bucket := range d.Buckets {
		cumulative += bucket
		if bucket == 0 || float64(cumulative) < rank {
			continue
		}
		if i == len(d.BoundsMS) {
			return d.BoundsMS[i-1], true
		}
		lower := 0.0
		if i > 0 {
			lower = d.BoundsMS[i-1]
		}
		fraction := (rank - float64(cumulative-bucket)) / float64(bucket)
		return lower + (d.BoundsMS[i]-lower)*fraction, true
	}
	return d.BoundsMS[len(d.BoundsMS)-1], true
}

type histogram struct {
	buckets []uint64
	count   uint64
	sum     float64
}

func newHistogram() *histogram {
	return &histogram{buckets: make([]uint64, len(DurationBoundsMS)+1)}
}

func (h *histogram) observe(d time.Duration) {
	ms := float64(d) / float64(time.Millisecond)
	if ms < 0 || math.IsNaN(ms) || math.IsInf(ms, 0) {
		return
	}
	h.buckets[histogramBucket(ms)]++
	h.count++
	h.sum += ms
}

func histogramBucket(ms float64) int {
	return sort.SearchFloat64s(DurationBoundsMS, ms)
}

func (h *histogram) data() HistogramData {
	buckets := make([]uint64, len(h.buckets))
	copy(buckets, h.buckets)
	bounds := make([]float64, len(DurationBoundsMS))
	copy(bounds, DurationBoundsMS)
	return HistogramData{BoundsMS: bounds, Buckets: buckets, Count: h.count, SumMS: h.sum}
}
