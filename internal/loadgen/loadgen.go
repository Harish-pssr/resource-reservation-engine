// Package loadgen drives the HTTP API with simulated buyers, open loop (D10).
//
// An open-loop generator sends at a fixed rate whatever the system answers. A closed-loop one
// waits for each answer before sending the next, so when the system slows down the generator
// slows down with it and queueing disappears from the numbers ("coordinated omission"). To keep
// the numbers honest this generator:
//   - schedules every arrival at a fixed intended time (start + i/rate), computed from the start
//     so timer jitter cannot make the schedule drift;
//   - measures hold latency from that INTENDED time, not from when the request was actually sent;
//   - records how late it was in sending (schedule lag), so a reader can tell whether the
//     generator itself kept up. It must never be the bottleneck.
//
// With the waiting room on (Config.Room), a buyer first joins the queue and polls until admitted
// (D9). Queue wait is reported separately from the store-facing latency, so the room's effect on
// the store is visible without hiding the wait it imposes on buyers.
package loadgen

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"resourcereservation/internal/workload"
)

// RetryPolicy is the client-side retry rule, identical for every strategy (D7): only lock_busy is
// retried, with jittered exponential backoff. unavailable is final.
type RetryPolicy struct {
	MaxRetries int
	Base, Cap  time.Duration
}

// DefaultRetry matches the approved experiment parameters.
var DefaultRetry = RetryPolicy{MaxRetries: 3, Base: 20 * time.Millisecond, Cap: 200 * time.Millisecond}

// backoff returns the wait before retry number attempt (0-based): full jitter in [0, min(cap, base*2^attempt)].
func (r RetryPolicy) backoff(attempt int, rnd func() float64) time.Duration {
	limit := r.Base << attempt
	if limit > r.Cap || limit <= 0 {
		limit = r.Cap
	}
	return time.Duration(rnd() * float64(limit))
}

// Config describes one run.
type Config struct {
	BaseURL        string
	ItemIDs        []int64 // the event's items, in order; picker indexes point into this
	Picker         *workload.Picker
	Rate           float64 // buyers per second
	Duration       time.Duration
	MaxInflight    int // safety cap; arrivals beyond it are counted as skipped
	Retry          RetryPolicy
	RunID          string
	HoldTimeout    time.Duration
	ConfirmTimeout time.Duration

	// Waiting room (D9). Room makes every buyer join the queue and poll until admitted before holding.
	Room            bool
	RoomWaitTimeout time.Duration // a buyer gives up waiting after this long (default 2 min)
	RoomPollMin     time.Duration // never poll more often than this (default 100 ms)
}

// Ref is a hold reference as returned by the API.
type Ref struct {
	ItemID      int64 `json:"item_id"`
	HoldVersion int64 `json:"hold_version"`
}

// Hold outcomes as seen by the client.
const (
	HoldHeld        = "held"
	HoldUnavailable = "unavailable"
	HoldBusy        = "lock_busy"    // still busy after the retry budget
	HoldNotAdmitted = "not_admitted" // the API required a pass this buyer did not have
	HoldError       = "error"        // transport error or unexpected status: the outcome is unknown
)

// Waiting-room outcomes as seen by the client. Empty when the room is not used.
const (
	RoomAdmitted   = "admitted"
	RoomTurnedAway = "turned_away" // the room was full: retry
	RoomGaveUp     = "gave_up"     // waited longer than the buyer's patience
	RoomError      = "error"
)

// BuyerResult is everything the client knows about one buyer. The per-buyer log is what the
// reconciliation against the database uses (requests of unknown outcome are the ones with errors).
type BuyerResult struct {
	Buyer     string        `json:"buyer"`
	RequestID string        `json:"request_id"`
	Items     []int64       `json:"items"`
	Sent      bool          `json:"sent"`
	Lag       time.Duration `json:"lag_ns"`

	// Waiting room (D9).
	RoomOutcome  string        `json:"room_outcome,omitempty"`
	Ticket       int64         `json:"ticket,omitempty"`      // arrival sequence at the room
	AdmitOrder   int64         `json:"admit_order,omitempty"` // order in which the room admitted buyers
	JoinPosition int           `json:"join_position,omitempty"`
	QueueWait    time.Duration `json:"queue_wait_ns,omitempty"` // join sent -> admission seen

	HoldOutcome string        `json:"hold_outcome"`
	Attempts    int           `json:"attempts"`
	HoldLatency time.Duration `json:"hold_latency_ns"`                 // intended send time -> final hold answer (queue wait and retries included)
	StoreHold   time.Duration `json:"store_hold_latency_ns,omitempty"` // first hold request sent -> final hold answer (retries included, queue wait excluded)
	Refs        []Ref         `json:"refs,omitempty"`

	ConfirmOutcome string        `json:"confirm_outcome,omitempty"` // confirmed, hold_lost, payment_failed, payment_timeout, payment_in_progress, error
	AfterExpiry    bool          `json:"after_expiry,omitempty"`
	Replayed       bool          `json:"replayed,omitempty"`
	ConfirmLatency time.Duration `json:"confirm_latency_ns,omitempty"`
	Err            string        `json:"err,omitempty"`
}

type client struct {
	http *http.Client
	base string
}

func (c *client) do(ctx context.Context, method, path string, body any, token string, timeout time.Duration) (int, map[string]any, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Admission-Token", token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out, nil
}

func (c *client) post(ctx context.Context, path string, body any, timeout time.Duration) (int, map[string]any, error) {
	return c.do(ctx, http.MethodPost, path, body, "", timeout)
}

func num(m map[string]any, key string) float64 { v, _ := m[key].(float64); return v }

// admit runs the waiting-room protocol (D9): join, then poll until admitted, turned away or out of
// patience. It returns the pass token ("" if the buyer was not admitted).
func (c *client) admit(ctx context.Context, cfg Config, r *BuyerResult) string {
	joined := time.Now()
	status, out, err := c.post(ctx, "/queue/join", map[string]any{"buyer_id": r.Buyer}, cfg.HoldTimeout)
	deadline := joined.Add(cfg.RoomWaitTimeout)
	rejoins := 0
	for {
		if err != nil {
			r.RoomOutcome, r.Err = RoomError, err.Error()
			return ""
		}
		state, _ := out["state"].(string)
		switch {
		case status == http.StatusTooManyRequests:
			r.RoomOutcome = RoomTurnedAway
			return ""
		case status == http.StatusOK && state == "admitted":
			token, _ := out["token"].(string)
			r.RoomOutcome, r.AdmitOrder, r.QueueWait = RoomAdmitted, int64(num(out, "admit_order")), time.Since(joined)
			return token
		case status == http.StatusOK && state == "queued":
			if r.Ticket == 0 {
				r.Ticket, r.JoinPosition = int64(num(out, "ticket")), int(num(out, "position"))
			}
		case status == http.StatusNotFound && rejoins < 3: // the queue was lost: join again at the back
			rejoins++
			status, out, err = c.post(ctx, "/queue/join", map[string]any{"buyer_id": r.Buyer}, cfg.HoldTimeout)
			continue
		default:
			r.RoomOutcome, r.Err = RoomError, fmt.Sprintf("unexpected room status %d", status)
			return ""
		}
		if time.Now().After(deadline) {
			r.RoomOutcome = RoomGaveUp
			r.QueueWait = time.Since(joined)
			return ""
		}
		wait := time.Duration(num(out, "retry_after_ms")) * time.Millisecond
		time.Sleep(min(max(wait, cfg.RoomPollMin), 2*time.Second))
		status, out, err = c.do(ctx, http.MethodGet, "/queue/"+r.Buyer, nil, "", cfg.HoldTimeout)
	}
}

func (c *client) hold(ctx context.Context, cfg Config, r *BuyerResult, intended time.Time, token string) {
	body := map[string]any{"buyer_id": r.Buyer, "request_id": r.RequestID, "item_ids": r.Items}
	first := time.Now()
	for attempt := 0; ; attempt++ {
		r.Attempts = attempt + 1
		status, out, err := c.do(ctx, http.MethodPost, "/holds", body, token, cfg.HoldTimeout)
		switch {
		case err != nil:
			r.HoldOutcome, r.Err = HoldError, err.Error()
		case status == http.StatusCreated:
			r.HoldOutcome = HoldHeld
			if raw, ok := out["items"].([]any); ok {
				for _, it := range raw {
					m, _ := it.(map[string]any)
					r.Refs = append(r.Refs, Ref{ItemID: int64(num(m, "item_id")), HoldVersion: int64(num(m, "hold_version"))})
				}
			}
		case status == http.StatusConflict:
			r.HoldOutcome = HoldUnavailable
		case status == http.StatusForbidden:
			r.HoldOutcome = HoldNotAdmitted
		case status == http.StatusServiceUnavailable:
			r.HoldOutcome = HoldBusy
			if attempt < cfg.Retry.MaxRetries {
				time.Sleep(cfg.Retry.backoff(attempt, rand.Float64))
				continue
			}
		default:
			r.HoldOutcome, r.Err = HoldError, fmt.Sprintf("unexpected status %d", status)
		}
		r.HoldLatency = time.Since(intended)
		r.StoreHold = time.Since(first)
		return
	}
}

func (c *client) confirm(ctx context.Context, cfg Config, r *BuyerResult) {
	start := time.Now()
	status, out, err := c.post(ctx, "/holds/confirm", map[string]any{"buyer_id": r.Buyer, "items": r.Refs}, cfg.ConfirmTimeout)
	r.ConfirmLatency = time.Since(start)
	if err != nil {
		r.ConfirmOutcome, r.Err = "error", err.Error()
		return
	}
	outcome, _ := out["outcome"].(string)
	switch status {
	case http.StatusOK:
		r.ConfirmOutcome = "confirmed"
		r.AfterExpiry, _ = out["after_expiry"].(bool)
		r.Replayed, _ = out["replayed"].(bool)
	case http.StatusConflict, http.StatusPaymentRequired:
		r.ConfirmOutcome = outcome
	default:
		r.ConfirmOutcome, r.Err = "error", fmt.Sprintf("unexpected status %d", status)
	}
}

// Result is a finished run.
type Result struct {
	Summary Summary
	Buyers  []BuyerResult
}

// intendedAt is when arrival i should be sent, computed from the start (no accumulated drift).
func intendedAt(start time.Time, i int, rate float64) time.Time {
	return start.Add(time.Duration(float64(i) / rate * float64(time.Second)))
}

// Run sends cfg.Rate buyers per second for cfg.Duration, open loop, and waits for all of them to finish.
func Run(ctx context.Context, cfg Config) (Result, error) {
	if cfg.Rate <= 0 || cfg.Duration <= 0 || cfg.Picker == nil || len(cfg.ItemIDs) == 0 {
		return Result{}, fmt.Errorf("rate, duration, picker and items are required")
	}
	if cfg.MaxInflight <= 0 {
		cfg.MaxInflight = 20000
	}
	if cfg.Retry.Base == 0 {
		cfg.Retry = DefaultRetry
	}
	if cfg.HoldTimeout <= 0 {
		cfg.HoldTimeout = 30 * time.Second
	}
	if cfg.ConfirmTimeout <= 0 {
		cfg.ConfirmTimeout = 60 * time.Second
	}
	if cfg.RoomWaitTimeout <= 0 {
		cfg.RoomWaitTimeout = 2 * time.Minute
	}
	if cfg.RoomPollMin <= 0 {
		cfg.RoomPollMin = 100 * time.Millisecond
	}
	if cfg.RunID == "" {
		cfg.RunID = fmt.Sprintf("run-%d", time.Now().Unix())
	}
	c := &client{base: cfg.BaseURL, http: &http.Client{Transport: &http.Transport{
		MaxIdleConns: cfg.MaxInflight, MaxIdleConnsPerHost: cfg.MaxInflight, IdleConnTimeout: 90 * time.Second}}}

	total := int(cfg.Rate * cfg.Duration.Seconds())
	results := make([]BuyerResult, total)
	// All picks are drawn before the run starts: the picker is not concurrency-safe, and drawing
	// them up front keeps the schedule loop free of work that could delay a send.
	for i := range results {
		idx := cfg.Picker.Pick()
		items := make([]int64, len(idx))
		for j, k := range idx {
			items[j] = cfg.ItemIDs[k]
		}
		results[i] = BuyerResult{Buyer: fmt.Sprintf("%s-b%d", cfg.RunID, i), RequestID: fmt.Sprintf("%s-b%d-h", cfg.RunID, i), Items: items}
	}

	var (
		wg       sync.WaitGroup
		inflight atomic.Int64
		peak     atomic.Int64
		skipped  int
	)
	buyer := func(i int, intended time.Time) {
		defer wg.Done()
		defer inflight.Add(-1)
		r := &results[i]
		r.Sent = true
		r.Lag = time.Since(intended)
		token := ""
		if cfg.Room {
			if token = c.admit(ctx, cfg, r); r.RoomOutcome != RoomAdmitted {
				return // turned away, out of patience, or an error: this buyer never reaches the store
			}
		}
		c.hold(ctx, cfg, r, intended, token)
		if r.HoldOutcome == HoldHeld {
			c.confirm(ctx, cfg, r)
		}
	}

	start := time.Now()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for next := 0; next < total; {
		select {
		case <-ctx.Done():
			wg.Wait()
			return Result{Summary: Summarize(cfg, results, skipped, peak.Load(), time.Since(start), time.Since(start)), Buyers: results}, ctx.Err()
		case <-ticker.C:
		}
		now := time.Now()
		// Send everything whose intended time has arrived. If the loop fell behind it catches up,
		// and the lateness is recorded against each request's intended time.
		for next < total && !intendedAt(start, next, cfg.Rate).After(now) {
			if inflight.Load() >= int64(cfg.MaxInflight) {
				skipped++
				next++
				continue
			}
			cur := inflight.Add(1)
			for {
				old := peak.Load()
				if cur <= old || peak.CompareAndSwap(old, cur) {
					break
				}
			}
			wg.Add(1)
			go buyer(next, intendedAt(start, next, cfg.Rate))
			next++
		}
	}
	wg.Wait()
	return Result{Summary: Summarize(cfg, results, skipped, peak.Load(), cfg.Duration, time.Since(start)), Buyers: results}, nil
}

// CreateEvent seeds an event with n items through the API and returns its id and item ids in order.
func CreateEvent(ctx context.Context, baseURL string, n int) (int64, []int64, error) {
	c := &client{base: baseURL, http: &http.Client{}}
	status, out, err := c.post(ctx, "/events", map[string]any{"name": "loadgen", "items": n}, 2*time.Minute) // seeding a large event takes a moment
	if err != nil {
		return 0, nil, err
	}
	if status != http.StatusCreated {
		return 0, nil, fmt.Errorf("create event: status %d: %v", status, out)
	}
	var ids []int64
	if raw, ok := out["item_ids"].([]any); ok {
		for _, v := range raw {
			f, _ := v.(float64)
			ids = append(ids, int64(f))
		}
	}
	return int64(num(out, "event_id")), ids, nil
}

// FIFOInversions counts pairs of admitted buyers whose admission order differs from their arrival
// order at the room. The room guarantees 0 for buyers who keep polling (D9): this is the measured
// check behind the word "fair". Buyers without both numbers are ignored.
func FIFOInversions(buyers []BuyerResult) int {
	type pair struct{ ticket, order int64 }
	var ps []pair
	for _, b := range buyers {
		if b.Ticket > 0 && b.AdmitOrder > 0 {
			ps = append(ps, pair{b.Ticket, b.AdmitOrder})
		}
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].ticket < ps[j].ticket })
	// Count inversions in the sequence of admission orders with a Fenwick tree: O(n log n).
	orders := make([]int64, len(ps))
	for i, p := range ps {
		orders[i] = p.order
	}
	sorted := append([]int64(nil), orders...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	rank := func(v int64) int { return sort.Search(len(sorted), func(i int) bool { return sorted[i] >= v }) + 1 }
	tree := make([]int, len(sorted)+2)
	add := func(i int) {
		for ; i < len(tree); i += i & -i {
			tree[i]++
		}
	}
	sum := func(i int) (s int) {
		for ; i > 0; i -= i & -i {
			s += tree[i]
		}
		return
	}
	inv := 0
	for i, v := range orders {
		r := rank(v)
		inv += i - sum(r) // earlier arrivals that were admitted after this one
		add(r)
	}
	return inv
}
