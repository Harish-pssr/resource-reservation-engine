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
package loadgen

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
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
	HoldBusy        = "lock_busy" // still busy after the retry budget
	HoldError       = "error"     // transport error or unexpected status: the outcome is unknown
)

// BuyerResult is everything the client knows about one buyer. The per-buyer log is what the
// reconciliation against the database uses (requests of unknown outcome are the ones with errors).
type BuyerResult struct {
	Buyer          string        `json:"buyer"`
	RequestID      string        `json:"request_id"`
	Items          []int64       `json:"items"`
	Sent           bool          `json:"sent"`
	Lag            time.Duration `json:"lag_ns"`
	HoldOutcome    string        `json:"hold_outcome"`
	Attempts       int           `json:"attempts"`
	HoldLatency    time.Duration `json:"hold_latency_ns"` // intended send time -> final hold answer, retries included
	Refs           []Ref         `json:"refs,omitempty"`
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

func (c *client) post(ctx context.Context, path string, body any, timeout time.Duration) (int, map[string]any, error) {
	b, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, bytes.NewReader(b))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
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

func (c *client) hold(ctx context.Context, cfg Config, r *BuyerResult, intended time.Time) {
	body := map[string]any{"buyer_id": r.Buyer, "request_id": r.RequestID, "item_ids": r.Items}
	for attempt := 0; ; attempt++ {
		r.Attempts = attempt + 1
		status, out, err := c.post(ctx, "/holds", body, cfg.HoldTimeout)
		switch {
		case err != nil:
			r.HoldOutcome, r.Err = HoldError, err.Error()
		case status == http.StatusCreated:
			r.HoldOutcome = HoldHeld
			if raw, ok := out["items"].([]any); ok {
				for _, it := range raw {
					m, _ := it.(map[string]any)
					id, _ := m["item_id"].(float64)
					v, _ := m["hold_version"].(float64)
					r.Refs = append(r.Refs, Ref{ItemID: int64(id), HoldVersion: int64(v)})
				}
			}
		case status == http.StatusConflict:
			r.HoldOutcome = HoldUnavailable
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
		c.hold(ctx, cfg, r, intended)
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
