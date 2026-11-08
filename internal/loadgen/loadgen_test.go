package loadgen

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"resourcereservation/internal/checker"
	"resourcereservation/internal/httpapi"
	"resourcereservation/internal/payment"
	"resourcereservation/internal/reservation/locking"
	"resourcereservation/internal/testdb"
	"resourcereservation/internal/workload"
)

func TestBackoffStaysWithinTheJitterWindow(t *testing.T) {
	r := DefaultRetry // base 20 ms, cap 200 ms
	for attempt, limit := range []time.Duration{20, 40, 80, 160, 200, 200} {
		limit *= time.Millisecond
		if got := r.backoff(attempt, func() float64 { return 1 }); got != limit {
			t.Errorf("attempt %d at full jitter: %v, want %v", attempt, got, limit)
		}
		if got := r.backoff(attempt, func() float64 { return 0 }); got != 0 {
			t.Errorf("attempt %d at zero jitter: %v, want 0", attempt, got)
		}
	}
}

func TestSummarizeCountsEverything(t *testing.T) {
	results := []BuyerResult{
		{Sent: true, Items: []int64{1, 2}, HoldOutcome: HoldHeld, Attempts: 2, HoldLatency: 10 * time.Millisecond, ConfirmOutcome: "confirmed", ConfirmLatency: time.Second},
		{Sent: true, Items: []int64{3}, HoldOutcome: HoldHeld, Attempts: 1, HoldLatency: 20 * time.Millisecond, ConfirmOutcome: "confirmed", AfterExpiry: true},
		{Sent: true, Items: []int64{4}, HoldOutcome: HoldHeld, Attempts: 1, ConfirmOutcome: "payment_failed"},
		{Sent: true, Items: []int64{5}, HoldOutcome: HoldHeld, Attempts: 1, ConfirmOutcome: "hold_lost"},
		{Sent: true, Items: []int64{6}, HoldOutcome: HoldHeld, Attempts: 1, ConfirmOutcome: "error"},
		{Sent: true, Items: []int64{7}, HoldOutcome: HoldUnavailable, Attempts: 1, HoldLatency: 5 * time.Millisecond},
		{Sent: true, Items: []int64{8}, HoldOutcome: HoldBusy, Attempts: 4, HoldLatency: 300 * time.Millisecond},
		{Sent: true, Items: []int64{9}, HoldOutcome: HoldError, Attempts: 1},
		{Sent: false}, // skipped, never sent
	}
	s := Summarize(Config{RunID: "x", Rate: 9}, results, 1, 5, 2*time.Second, 4*time.Second)
	checks := map[string][2]int{
		"sent": {s.Sent, 8}, "held": {s.HoldHeld, 5}, "unavailable": {s.HoldUnavailable, 1}, "busy": {s.HoldBusy, 1},
		"hold errors": {s.HoldErrors, 1}, "retries": {s.Retries, 4}, "confirmed": {s.Confirmed, 2},
		"confirmed items": {s.ConfirmedItems, 3}, "after expiry": {s.ConfirmedAfterExpiry, 1}, "payment failed": {s.PaymentFailed, 1},
		"hold lost": {s.HoldLost, 1}, "confirm errors": {s.ConfirmErrors, 1}, "skipped": {s.Skipped, 1},
	}
	for name, c := range checks {
		if c[0] != c[1] {
			t.Errorf("%s: got %d, want %d", name, c[0], c[1])
		}
	}
	if s.BookingsPerSecArrival != 1 || s.BookingsPerSecWall != 0.5 {
		t.Errorf("bookings/s: %v and %v, want 1 and 0.5", s.BookingsPerSecArrival, s.BookingsPerSecWall)
	}
	if s.GeneratorOK {
		t.Error("a run with skipped arrivals must not report that the generator kept up")
	}
}

// fakeServer answers every hold the same way after a delay.
func fakeServer(status int, body string, delay time.Duration, calls *atomic.Int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls != nil {
			calls.Add(1)
		}
		time.Sleep(delay)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
}

func baseConfig(url string, rate float64, d time.Duration) Config {
	ids := make([]int64, 100)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	return Config{BaseURL: url, ItemIDs: ids, Picker: workload.NewPicker(len(ids), 1.1, 1), Rate: rate, Duration: d, RunID: "t"}
}

// The defining property of an open-loop generator: a slow server does not slow the arrivals down,
// and the slowness shows up as latency measured from the intended send time.
func TestOpenLoopKeepsItsScheduleWhenTheServerIsSlow(t *testing.T) {
	srv := fakeServer(http.StatusConflict, `{"outcome":"unavailable"}`, 300*time.Millisecond, nil)
	defer srv.Close()
	res, err := Run(context.Background(), baseConfig(srv.URL, 100, time.Second))
	if err != nil {
		t.Fatal(err)
	}
	s := res.Summary
	if s.Sent != 100 || s.Skipped != 0 {
		t.Fatalf("sent %d, skipped %d, want all 100 sent", s.Sent, s.Skipped)
	}
	if s.LagP99Ms > 100 {
		t.Fatalf("p99 schedule lag %.1f ms: a slow server must not delay the schedule", s.LagP99Ms)
	}
	if s.HoldP50Ms < 300 {
		t.Fatalf("hold p50 %.1f ms, want at least the server's 300 ms delay", s.HoldP50Ms)
	}
	if s.MaxInflight < 20 {
		t.Fatalf("max in flight %d: with 100/s and 300 ms answers about 30 should overlap", s.MaxInflight)
	}
}

func TestLockBusyIsRetriedAndThenGivenUp(t *testing.T) {
	var calls atomic.Int64
	srv := fakeServer(http.StatusServiceUnavailable, `{"outcome":"lock_busy"}`, 0, &calls)
	defer srv.Close()
	cfg := baseConfig(srv.URL, 10, time.Second)
	cfg.Retry = RetryPolicy{MaxRetries: 3, Base: time.Millisecond, Cap: 2 * time.Millisecond}
	res, _ := Run(context.Background(), cfg)
	if res.Summary.HoldBusy != 10 || res.Summary.Retries != 30 || calls.Load() != 40 {
		t.Fatalf("busy %d retries %d calls %d: want 10 buyers x (1 try + 3 retries)", res.Summary.HoldBusy, res.Summary.Retries, calls.Load())
	}
}

func TestUnavailableIsNeverRetried(t *testing.T) {
	var calls atomic.Int64
	srv := fakeServer(http.StatusConflict, `{"outcome":"unavailable"}`, 0, &calls)
	defer srv.Close()
	res, _ := Run(context.Background(), baseConfig(srv.URL, 10, time.Second))
	if res.Summary.Retries != 0 || calls.Load() != 10 {
		t.Fatalf("retries %d calls %d: unavailable is final", res.Summary.Retries, calls.Load())
	}
}

func TestTransportErrorsAreRecordedAsUnknownOutcome(t *testing.T) {
	srv := fakeServer(http.StatusOK, `{}`, 0, nil)
	url := srv.URL
	srv.Close() // nothing is listening any more
	res, _ := Run(context.Background(), baseConfig(url, 10, time.Second))
	if res.Summary.HoldErrors != 10 || res.Summary.Sent != 10 {
		t.Fatalf("errors %d sent %d", res.Summary.HoldErrors, res.Summary.Sent)
	}
}

// End to end against the real handler, Postgres and the checker: what the client counted as
// booked must equal what the database holds, and the checker must find nothing wrong.
func TestRunAgainstTheRealStackReconcilesWithTheDatabase(t *testing.T) {
	pool := testdb.New(t, true)
	cfg := payment.Config{DelayMin: time.Millisecond, DelayMax: 3 * time.Millisecond, FailPct: 10, TimeoutPct: 0, Timeout: 50 * time.Millisecond}
	h := httpapi.New(httpapi.Deps{Service: locking.New(pool), Payment: payment.New(cfg), Pool: pool, HoldTTL: time.Minute})
	srv := httptest.NewServer(h)
	defer srv.Close()

	// Seed through the API, as the benchmarks will.
	resp, err := http.Post(srv.URL+"/events", "application/json", strings.NewReader(`{"name":"e","items":300}`))
	if err != nil {
		t.Fatal(err)
	}
	var ev struct {
		ItemIDs []int64 `json:"item_ids"`
	}
	json.NewDecoder(resp.Body).Decode(&ev)
	resp.Body.Close()

	run := Config{BaseURL: srv.URL, ItemIDs: ev.ItemIDs, Picker: workload.NewPicker(len(ev.ItemIDs), 1.1, 7), Rate: 150, Duration: 2 * time.Second, RunID: "e2e"}
	res, err := Run(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	s := res.Summary
	t.Logf("\n%s", Format(s))
	if s.HoldErrors != 0 || s.ConfirmErrors != 0 {
		t.Fatalf("no request may have an unknown outcome here: %+v", s)
	}
	if s.HoldHeld+s.HoldUnavailable+s.HoldBusy != s.Sent {
		t.Fatalf("every sent buyer has exactly one hold outcome: %+v", s)
	}
	rep, err := checker.Check(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK() {
		t.Fatalf("checker: %v", rep)
	}
	if rep.ConfirmedBookings != s.ConfirmedItems {
		t.Fatalf("the client counted %d booked items, the database holds %d", s.ConfirmedItems, rep.ConfirmedBookings)
	}
	if s.Confirmed == 0 {
		t.Fatal("expected some bookings")
	}
}
