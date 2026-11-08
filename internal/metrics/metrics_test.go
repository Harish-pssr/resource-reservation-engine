package metrics

import (
	"testing"
	"time"
)

func TestHistogramBucketsAndUpperBoundPercentiles(t *testing.T) {
	var h Histogram
	for i := 0; i < 98; i++ {
		h.Observe(3 * time.Millisecond) // lands in the <=5 bucket
	}
	h.Observe(150 * time.Millisecond) // <=200
	h.Observe(20 * time.Second)       // beyond the last bound
	s := h.Snapshot()
	if s.Count != 100 || s.BucketsMs["<=5"] != 98 || s.BucketsMs["<=200"] != 1 || s.BucketsMs["<=inf"] != 1 {
		t.Fatalf("buckets: %+v", s)
	}
	if s.P50MsUB != 5 {
		t.Fatalf("p50 upper bound %v, want 5", s.P50MsUB)
	}
	if s.P99MsUB != 200 { // the 99th of 100 observations is the 150 ms one
		t.Fatalf("p99 upper bound %v, want 200", s.P99MsUB)
	}
}

func TestBoundaryValueGoesIntoItsOwnBucket(t *testing.T) {
	var h Histogram
	h.Observe(5 * time.Millisecond)
	if h.Snapshot().BucketsMs["<=5"] != 1 {
		t.Fatalf("exactly 5 ms belongs in the <=5 bucket: %+v", h.Snapshot().BucketsMs)
	}
}

func TestRegistryCountersAreSharedByName(t *testing.T) {
	r := NewRegistry()
	r.Counter("a").Add(2)
	r.Counter("a").Add(3)
	r.Histogram("h").Observe(time.Millisecond)
	s := r.Snapshot()
	if s.Counters["a"] != 5 || s.Histograms["h"].Count != 1 {
		t.Fatalf("snapshot %+v", s)
	}
}
