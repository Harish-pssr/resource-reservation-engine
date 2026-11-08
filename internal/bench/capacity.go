package bench

import (
	"fmt"
	"math"

	"resourcereservation/internal/loadgen"
)

// HoldP99BoundMs is the latency bound of D12: a step is only sustained if the hold p99 stays under it.
const HoldP99BoundMs = 1000.0

// Verdict says whether a step at a given arrival rate was sustained, and if not, why (D12).
type Verdict struct {
	Sustained bool   `json:"sustained"`
	Reason    string `json:"reason,omitempty"` // empty when sustained; otherwise the first rule that failed
}

// HoldP99Ms is the p99 the bound applies to: from the intended send time, retries included. With the
// waiting room on it is the store-facing latency, so the wait the room imposes is reported
// separately instead of being counted as the store's slowness.
func HoldP99Ms(s loadgen.Summary) float64 {
	if s.RoomUsed {
		return s.StoreHoldP99Ms
	}
	return s.HoldP99Ms
}

// HoldP50Ms is the median from the same latency view as HoldP99Ms, so the two are never mixed.
func HoldP50Ms(s loadgen.Summary) float64 {
	if s.RoomUsed {
		return s.StoreHoldP50Ms
	}
	return s.HoldP50Ms
}

// Judge applies the D12 rules, in order; the first that fails is reported:
//  1. the generator kept up (it must never be the bottleneck);
//  2. no unknown outcomes (a transport error, an unexpected status);
//  3. the hold p99 is under the bound.
func Judge(s loadgen.Summary) Verdict {
	fail := func(format string, args ...any) Verdict {
		return Verdict{Sustained: false, Reason: fmt.Sprintf(format, args...)}
	}
	switch unknown := s.HoldErrors + s.ConfirmErrors + s.RoomErrors; {
	case s.Sent == 0:
		return fail("nothing was sent")
	case !s.GeneratorOK:
		return fail("generator did not keep up (skipped %d, schedule lag p99 %.1f ms)", s.Skipped, s.LagP99Ms)
	case unknown > 0:
		return fail("%d requests of unknown outcome", unknown)
	case HoldP99Ms(s) >= HoldP99BoundMs:
		return fail("hold p99 %.0f ms is not under %.0f ms", HoldP99Ms(s), HoldP99BoundMs)
	}
	return Verdict{Sustained: true}
}

// Ladder returns the geometric ladder of arrival rates (buyers per second): start, start*factor, ...
// rounded to whole numbers and strictly increasing, up to max.
func Ladder(start, factor, max float64) []float64 {
	var rates []float64
	r := math.Round(start)
	for r <= max {
		rates = append(rates, r)
		next := math.Round(r * factor)
		if next <= r {
			next = r + 1
		}
		r = next
	}
	return rates
}

// ConfirmCapacity finds the highest rate in an ascending ladder that is reliably sustained, starting
// from the ramp's best guess. passes(i) runs the repeated test at ladder index i and reports whether
// it was sustained. If the start passes, it climbs until a rate fails; if not, it walks down until one
// passes. It returns the index of the confirmed rate, or -1 if none passed. Each rate is tested at
// most once, which bounds the total running time.
func ConfirmCapacity(n, start int, passes func(i int) bool) int {
	if n == 0 {
		return -1
	}
	i := min(max(start, 0), n-1)
	if passes(i) {
		best := i
		for j := i + 1; j < n; j++ {
			if !passes(j) {
				break
			}
			best = j
		}
		return best
	}
	for j := i - 1; j >= 0; j-- {
		if passes(j) {
			return j
		}
	}
	return -1
}

// Majority repeats a test up to three times and stops as soon as the outcome is decided (two passes
// or two failures), so a clear result costs two runs, not three. It returns the individual outcomes.
func Majority(run func(rep int) (bool, error)) (passed bool, outcomes []bool, err error) {
	passes, fails := 0, 0
	for rep := 1; rep <= 3 && passes < 2 && fails < 2; rep++ {
		ok, err := run(rep)
		if err != nil {
			return false, outcomes, err
		}
		outcomes = append(outcomes, ok)
		if ok {
			passes++
		} else {
			fails++
		}
	}
	return passes >= 2, outcomes, nil
}
