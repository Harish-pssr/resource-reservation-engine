// Package metrics holds the small in-process counters and histograms the API exposes at /metrics.
// They exist to EXPLAIN a benchmark (where requests went, how many conflicts, how busy the API
// was), not to produce its headline numbers: those come from the client's exact timings and the
// database, because bucketed live histograms are only approximate.
package metrics

import (
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// bucketsMs are the histogram upper bounds in milliseconds; the last bucket is open-ended.
var bucketsMs = []float64{1, 2, 5, 10, 20, 50, 100, 200, 500, 1000, 2000, 5000, 10000}

// Histogram counts observations into fixed latency buckets.
type Histogram struct {
	counts [14]atomic.Int64 // len(bucketsMs) + 1 open-ended bucket
	total  atomic.Int64
	sumNs  atomic.Int64
}

// Observe records one duration.
func (h *Histogram) Observe(d time.Duration) {
	ms := float64(d) / float64(time.Millisecond)
	i := sort.SearchFloat64s(bucketsMs, ms) // first bound >= ms
	h.counts[i].Add(1)
	h.total.Add(1)
	h.sumNs.Add(int64(d))
}

// HistogramSnapshot is the JSON form. Percentiles are upper bounds of the bucket they fall in.
type HistogramSnapshot struct {
	Count     int64            `json:"count"`
	MeanMs    float64          `json:"mean_ms"`
	P50MsUB   float64          `json:"p50_ms_upper_bound"`
	P99MsUB   float64          `json:"p99_ms_upper_bound"`
	BucketsMs map[string]int64 `json:"buckets_ms"`
}

// Snapshot returns the current state.
func (h *Histogram) Snapshot() HistogramSnapshot {
	s := HistogramSnapshot{Count: h.total.Load(), BucketsMs: map[string]int64{}}
	if s.Count > 0 {
		s.MeanMs = float64(h.sumNs.Load()) / float64(s.Count) / float64(time.Millisecond)
	}
	counts := make([]int64, len(h.counts))
	for i := range h.counts {
		counts[i] = h.counts[i].Load()
		label := "inf"
		if i < len(bucketsMs) {
			label = trim(bucketsMs[i])
		}
		if counts[i] > 0 {
			s.BucketsMs["<="+label] = counts[i]
		}
	}
	s.P50MsUB, s.P99MsUB = upperBound(counts, s.Count, 0.5), upperBound(counts, s.Count, 0.99)
	return s
}

func upperBound(counts []int64, total int64, p float64) float64 {
	if total == 0 {
		return 0
	}
	target := int64(float64(total)*p + 0.999999)
	var acc int64
	for i, c := range counts {
		acc += c
		if acc >= target {
			if i < len(bucketsMs) {
				return bucketsMs[i]
			}
			return -1 // beyond the last bound
		}
	}
	return -1
}

func trim(f float64) string { return strconv.FormatInt(int64(f), 10) } // every bound is a whole number

// Registry holds named counters and histograms.
type Registry struct {
	mu       sync.Mutex
	counters map[string]*atomic.Int64
	hists    map[string]*Histogram
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{counters: map[string]*atomic.Int64{}, hists: map[string]*Histogram{}}
}

// Counter returns the named counter, creating it on first use.
func (r *Registry) Counter(name string) *atomic.Int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.counters[name]
	if !ok {
		c = new(atomic.Int64)
		r.counters[name] = c
	}
	return c
}

// Histogram returns the named histogram, creating it on first use.
func (r *Registry) Histogram(name string) *Histogram {
	r.mu.Lock()
	defer r.mu.Unlock()
	h, ok := r.hists[name]
	if !ok {
		h = new(Histogram)
		r.hists[name] = h
	}
	return h
}

// Snapshot is everything in the registry.
type Snapshot struct {
	Counters   map[string]int64             `json:"counters"`
	Histograms map[string]HistogramSnapshot `json:"histograms"`
}

// Snapshot returns the current values.
func (r *Registry) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := Snapshot{Counters: map[string]int64{}, Histograms: map[string]HistogramSnapshot{}}
	for k, c := range r.counters {
		s.Counters[k] = c.Load()
	}
	for k, h := range r.hists {
		s.Histograms[k] = h.Snapshot()
	}
	return s
}
