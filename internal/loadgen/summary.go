package loadgen

import (
	"fmt"
	"sort"
	"time"
)

// Summary is what one run reports. Latencies are in milliseconds. Every figure is client-side;
// the exact figures about what happened to the inventory come from the checker reading the database.
type Summary struct {
	Run         string  `json:"run"`
	TargetRate  float64 `json:"target_rate"`
	DurationS   float64 `json:"arrival_window_s"` // how long arrivals were sent
	WallS       float64 `json:"wall_s"`           // until the last buyer finished (payments included)
	Planned     int     `json:"planned"`
	Sent        int     `json:"sent"`
	Skipped     int     `json:"skipped"` // not sent because the in-flight cap was reached
	MaxInflight int64   `json:"max_inflight"`

	LagP50Ms    float64 `json:"schedule_lag_p50_ms"`
	LagP99Ms    float64 `json:"schedule_lag_p99_ms"`
	LagMaxMs    float64 `json:"schedule_lag_max_ms"`
	GeneratorOK bool    `json:"generator_kept_up"` // nothing skipped and p99 schedule lag under 50 ms

	// Waiting room (D9). All zero when the room is not used.
	RoomUsed        bool    `json:"room_used"`
	RoomAdmitted    int     `json:"room_admitted"`
	RoomTurnedAway  int     `json:"room_turned_away"` // the room was full
	RoomGaveUp      int     `json:"room_gave_up"`     // out of patience while queued
	RoomErrors      int     `json:"room_errors"`
	QueueWaitP50Ms  float64 `json:"queue_wait_p50_ms"`
	QueueWaitP99Ms  float64 `json:"queue_wait_p99_ms"`
	QueueWaitMaxMs  float64 `json:"queue_wait_max_ms"`
	FIFOInversions  int     `json:"fifo_inversions"` // admitted buyers served out of arrival order: must be 0
	AdmittedWithPos int     `json:"admitted_via_queue"`

	// Hold step. HoldBusy counts buyers still lock_busy after the retry budget.
	HoldHeld        int `json:"hold_held"`
	HoldUnavailable int `json:"hold_unavailable"`
	HoldBusy        int `json:"hold_lock_busy_after_retries"`
	HoldNotAdmitted int `json:"hold_not_admitted"`
	HoldErrors      int `json:"hold_errors"` // unknown outcome (transport error or unexpected status)
	Retries         int `json:"retries"`     // extra hold attempts beyond the first

	// Confirm step (only buyers who held).
	Confirmed            int `json:"confirmed"`
	ConfirmedItems       int `json:"confirmed_items"`
	ConfirmedAfterExpiry int `json:"confirmed_after_expiry"`
	ConfirmReplayed      int `json:"confirm_replayed"`
	HoldLost             int `json:"hold_lost"`
	PaymentFailed        int `json:"payment_failed"`
	PaymentTimedOut      int `json:"payment_timeout"`
	PaymentInProgress    int `json:"payment_in_progress"`
	ConfirmErrors        int `json:"confirm_errors"` // unknown outcome

	// Hold latency from the intended send time (queue wait and retries included): what a buyer feels.
	HoldP50Ms float64 `json:"hold_p50_ms"`
	HoldP95Ms float64 `json:"hold_p95_ms"`
	HoldP99Ms float64 `json:"hold_p99_ms"`
	HoldMaxMs float64 `json:"hold_max_ms"`
	// Hold latency from the first hold request sent (queue wait excluded): what the store experienced.
	StoreHoldP50Ms float64 `json:"store_hold_p50_ms"`
	StoreHoldP99Ms float64 `json:"store_hold_p99_ms"`
	// Confirm latency includes the simulated payment wait.
	ConfirmP50Ms float64 `json:"confirm_p50_ms"`
	ConfirmP99Ms float64 `json:"confirm_p99_ms"`

	// Bookings per second: confirmed buyers over the arrival window, and over the wall time.
	BookingsPerSecArrival float64 `json:"bookings_per_s_arrival_window"`
	BookingsPerSecWall    float64 `json:"bookings_per_s_wall"`
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// percentile returns the p-th percentile (0..1) of sorted values by linear interpolation, 0 if empty.
func percentile(sorted []float64, p float64) float64 {
	n := len(sorted)
	switch n {
	case 0:
		return 0
	case 1:
		return sorted[0]
	}
	pos := p * float64(n-1)
	lo := int(pos)
	if lo >= n-1 {
		return sorted[n-1]
	}
	return sorted[lo] + (pos-float64(lo))*(sorted[lo+1]-sorted[lo])
}

// Summarize turns the per-buyer results into a Summary. It is a pure function, unit-tested.
func Summarize(cfg Config, results []BuyerResult, skipped int, maxInflight int64, window, wall time.Duration) Summary {
	s := Summary{Run: cfg.RunID, TargetRate: cfg.Rate, DurationS: window.Seconds(), WallS: wall.Seconds(),
		Planned: len(results), Skipped: skipped, MaxInflight: maxInflight, RoomUsed: cfg.Room}
	var lag, hold, store, wait, conf []float64
	for _, r := range results {
		if !r.Sent {
			continue
		}
		s.Sent++
		lag = append(lag, ms(r.Lag))

		if cfg.Room {
			switch r.RoomOutcome {
			case RoomAdmitted:
				s.RoomAdmitted++
				wait = append(wait, ms(r.QueueWait))
			case RoomTurnedAway:
				s.RoomTurnedAway++
			case RoomGaveUp:
				s.RoomGaveUp++
			default:
				s.RoomErrors++
			}
			if r.RoomOutcome != RoomAdmitted {
				continue // never reached the store: no hold outcome to count
			}
		}

		s.Retries += max(r.Attempts-1, 0)
		switch r.HoldOutcome {
		case HoldHeld:
			s.HoldHeld++
		case HoldUnavailable:
			s.HoldUnavailable++
		case HoldBusy:
			s.HoldBusy++
		case HoldNotAdmitted:
			s.HoldNotAdmitted++
		default:
			s.HoldErrors++
		}
		if r.HoldOutcome != HoldError {
			hold = append(hold, ms(r.HoldLatency))
			store = append(store, ms(r.StoreHold))
		}
		if r.HoldOutcome != HoldHeld {
			continue
		}
		conf = append(conf, ms(r.ConfirmLatency))
		switch r.ConfirmOutcome {
		case "confirmed":
			s.Confirmed++
			s.ConfirmedItems += len(r.Items)
			if r.AfterExpiry {
				s.ConfirmedAfterExpiry++
			}
			if r.Replayed {
				s.ConfirmReplayed++
			}
		case "hold_lost":
			s.HoldLost++
		case "payment_failed":
			s.PaymentFailed++
		case "payment_timeout":
			s.PaymentTimedOut++
		case "payment_in_progress":
			s.PaymentInProgress++
		default:
			s.ConfirmErrors++
		}
	}
	sort.Float64s(lag)
	sort.Float64s(hold)
	sort.Float64s(store)
	sort.Float64s(wait)
	sort.Float64s(conf)
	s.LagP50Ms, s.LagP99Ms = percentile(lag, 0.5), percentile(lag, 0.99)
	if n := len(lag); n > 0 {
		s.LagMaxMs = lag[n-1]
	}
	s.GeneratorOK = s.Skipped == 0 && s.LagP99Ms < 50
	s.HoldP50Ms, s.HoldP95Ms, s.HoldP99Ms = percentile(hold, 0.5), percentile(hold, 0.95), percentile(hold, 0.99)
	if n := len(hold); n > 0 {
		s.HoldMaxMs = hold[n-1]
	}
	s.StoreHoldP50Ms, s.StoreHoldP99Ms = percentile(store, 0.5), percentile(store, 0.99)
	s.ConfirmP50Ms, s.ConfirmP99Ms = percentile(conf, 0.5), percentile(conf, 0.99)
	if cfg.Room {
		s.QueueWaitP50Ms, s.QueueWaitP99Ms = percentile(wait, 0.5), percentile(wait, 0.99)
		if n := len(wait); n > 0 {
			s.QueueWaitMaxMs = wait[n-1]
		}
		s.FIFOInversions = FIFOInversions(results)
		for _, r := range results {
			if r.Ticket > 0 && r.AdmitOrder > 0 {
				s.AdmittedWithPos++
			}
		}
	}
	if window > 0 {
		s.BookingsPerSecArrival = float64(s.Confirmed) / window.Seconds()
	}
	if wall > 0 {
		s.BookingsPerSecWall = float64(s.Confirmed) / wall.Seconds()
	}
	return s
}

// Format renders a Summary for a person.
func Format(s Summary) string {
	out := fmt.Sprintf("run %s: target %.0f buyers/s for %.0fs (wall %.1fs)\n"+
		"  sent %d of %d planned (skipped %d); generator kept up: %v (schedule lag p99 %.1f ms)\n",
		s.Run, s.TargetRate, s.DurationS, s.WallS, s.Sent, s.Planned, s.Skipped, s.GeneratorOK, s.LagP99Ms)
	if s.RoomUsed {
		out += fmt.Sprintf("  waiting room: admitted %d, turned away %d, gave up %d, errors %d; queue wait p50 %.0f ms, p99 %.0f ms; FIFO inversions %d (of %d queued-and-admitted)\n",
			s.RoomAdmitted, s.RoomTurnedAway, s.RoomGaveUp, s.RoomErrors, s.QueueWaitP50Ms, s.QueueWaitP99Ms, s.FIFOInversions, s.AdmittedWithPos)
	}
	out += fmt.Sprintf("  hold: held %d, unavailable %d, lock_busy-after-retries %d, not_admitted %d, errors %d, retries %d\n"+
		"  hold latency from intended send time: p50 %.1f ms, p95 %.1f ms, p99 %.1f ms, max %.1f ms\n"+
		"  hold latency at the store (queue wait excluded): p50 %.1f ms, p99 %.1f ms\n"+
		"  confirm: confirmed %d (%d items; %d after expiry), hold_lost %d, payment_failed %d, payment_timeout %d, in_progress %d, errors %d\n"+
		"  bookings/s: %.1f over the arrival window, %.1f over wall time\n",
		s.HoldHeld, s.HoldUnavailable, s.HoldBusy, s.HoldNotAdmitted, s.HoldErrors, s.Retries,
		s.HoldP50Ms, s.HoldP95Ms, s.HoldP99Ms, s.HoldMaxMs, s.StoreHoldP50Ms, s.StoreHoldP99Ms,
		s.Confirmed, s.ConfirmedItems, s.ConfirmedAfterExpiry, s.HoldLost, s.PaymentFailed, s.PaymentTimedOut, s.PaymentInProgress, s.ConfirmErrors,
		s.BookingsPerSecArrival, s.BookingsPerSecWall)
	return out
}
