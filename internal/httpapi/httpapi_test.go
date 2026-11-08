package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"resourcereservation/internal/db"
	"resourcereservation/internal/httpapi"
	"resourcereservation/internal/payment"
	"resourcereservation/internal/reservation/locking"
	"resourcereservation/internal/testdb"
)

type fixture struct {
	t     *testing.T
	srv   *httptest.Server
	h     *httpapi.Handler
	event int64
	items []int64
}

// newFixture builds the real handler. roll fixes the payment outcome: 0.5 succeeds, 0.07 fails, 0.01 times out.
func newFixture(t *testing.T, roll float64, delay time.Duration) *fixture {
	pool := testdb.New(t, true)
	cfg := payment.Config{DelayMin: delay, DelayMax: delay, FailPct: 10, TimeoutPct: 5, Timeout: 30 * time.Millisecond}
	prov := payment.New(cfg).WithRand(func() float64 { return roll })
	h := httpapi.New(httpapi.Deps{Service: locking.New(pool), Payment: prov, Pool: pool, HoldTTL: time.Minute})
	ev, items, err := db.CreateEvent(context.Background(), pool, "t", 6)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &fixture{t: t, srv: srv, h: h, event: ev, items: items}
}

func (f *fixture) post(path string, body any) (int, map[string]any) {
	f.t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(f.srv.URL+path, "application/json", bytes.NewReader(b))
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (f *fixture) available() int {
	f.t.Helper()
	resp, err := http.Get(f.srv.URL + "/events/" + itoa(f.event) + "/availability")
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct{ Available int }
	json.NewDecoder(resp.Body).Decode(&out)
	return out.Available
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// hold takes the items and returns the refs for a later confirm or release.
func (f *fixture) hold(buyer, req string, items ...int64) (int, map[string]any, []map[string]any) {
	f.t.Helper()
	code, out := f.post("/holds", map[string]any{"buyer_id": buyer, "request_id": req, "item_ids": items})
	var refs []map[string]any
	if raw, ok := out["items"].([]any); ok {
		for _, it := range raw {
			m := it.(map[string]any)
			refs = append(refs, map[string]any{"item_id": m["item_id"], "hold_version": m["hold_version"]})
		}
	}
	return code, out, refs
}

func (f *fixture) confirm(buyer string, refs []map[string]any) (int, map[string]any) {
	f.t.Helper()
	return f.post("/holds/confirm", map[string]any{"buyer_id": buyer, "items": refs})
}

func wantStatus(t *testing.T, got, want int, out map[string]any) {
	t.Helper()
	if got != want {
		t.Fatalf("status %d, want %d (body %v)", got, want, out)
	}
}

func TestBookingFlow(t *testing.T) {
	f := newFixture(t, 0.5, time.Millisecond)
	code, out, refs := f.hold("alice", "r1", f.items[0], f.items[1])
	wantStatus(t, code, http.StatusCreated, out)
	if f.available() != 4 {
		t.Fatalf("available %d, want 4", f.available())
	}
	code, out = f.confirm("alice", refs)
	wantStatus(t, code, http.StatusOK, out)
	if out["outcome"] != "confirmed" || out["replayed"] != false {
		t.Fatalf("unexpected confirm body: %v", out)
	}
	if f.h.Stats.ConfirmedInTime.Load() != 1 {
		t.Fatalf("stats: %+v", &f.h.Stats)
	}
}

func TestSecondBuyerGetsUnavailableAndStatusDistinguishesFromBusy(t *testing.T) {
	f := newFixture(t, 0.5, time.Millisecond)
	f.hold("alice", "r1", f.items[0])
	code, out, _ := f.hold("bob", "r2", f.items[0])
	wantStatus(t, code, http.StatusConflict, out)
	if out["outcome"] != "unavailable" {
		t.Fatalf("body: %v", out)
	}
}

func TestPaymentFailureReleasesTheHold(t *testing.T) {
	f := newFixture(t, 0.07, time.Millisecond) // the failure band
	_, _, refs := f.hold("alice", "r1", f.items[0])
	code, out := f.confirm("alice", refs)
	wantStatus(t, code, http.StatusPaymentRequired, out)
	if out["outcome"] != "payment_failed" || f.available() != 6 {
		t.Fatalf("hold must be released after a failed payment: %v, available %d", out, f.available())
	}
	if code, out, _ := f.hold("bob", "r2", f.items[0]); code != http.StatusCreated {
		t.Fatalf("bob should now be able to hold: %d %v", code, out)
	}
}

func TestPaymentTimeoutReleasesTheHold(t *testing.T) {
	f := newFixture(t, 0.01, time.Millisecond) // the timeout band
	_, _, refs := f.hold("alice", "r1", f.items[0])
	code, out := f.confirm("alice", refs)
	wantStatus(t, code, http.StatusPaymentRequired, out)
	if out["outcome"] != "payment_timeout" || f.available() != 6 {
		t.Fatalf("hold must be released after a payment timeout: %v", out)
	}
}

func TestConfirmRetryAfterSuccessIsReplayed(t *testing.T) {
	f := newFixture(t, 0.5, time.Millisecond)
	_, _, refs := f.hold("alice", "r1", f.items[0])
	f.confirm("alice", refs)
	code, out := f.confirm("alice", refs)
	wantStatus(t, code, http.StatusOK, out)
	if out["replayed"] != true {
		t.Fatalf("a retried confirm must be reported as a replay: %v", out)
	}
}

func TestConfirmRetryDuringPaymentIsInProgress(t *testing.T) {
	f := newFixture(t, 0.5, 400*time.Millisecond) // a slow payment
	_, _, refs := f.hold("alice", "r1", f.items[0])
	done := make(chan int, 1)
	go func() { c, _ := f.confirm("alice", refs); done <- c }()
	time.Sleep(100 * time.Millisecond) // the first attempt is now inside the payment step
	code, out := f.confirm("alice", refs)
	wantStatus(t, code, http.StatusConflict, out)
	if out["outcome"] != "payment_in_progress" {
		t.Fatalf("body: %v", out)
	}
	if first := <-done; first != http.StatusOK {
		t.Fatalf("the first attempt must still succeed, got %d", first)
	}
}

func TestReleaseIsIdempotent(t *testing.T) {
	f := newFixture(t, 0.5, time.Millisecond)
	_, _, refs := f.hold("alice", "r1", f.items[0])
	code, out := f.post("/holds/release", map[string]any{"buyer_id": "alice", "items": refs})
	if code != http.StatusOK || out["outcome"] != "released" {
		t.Fatalf("first release: %d %v", code, out)
	}
	code, out = f.post("/holds/release", map[string]any{"buyer_id": "alice", "items": refs})
	if code != http.StatusOK || out["outcome"] != "not_held" {
		t.Fatalf("a retried release must be a harmless no-op: %d %v", code, out)
	}
}

func TestValidation(t *testing.T) {
	f := newFixture(t, 0.5, time.Millisecond)
	seven := []int64{1, 2, 3, 4, 5, 6, 7}
	if code, out := f.post("/holds", map[string]any{"buyer_id": "a", "request_id": "r", "item_ids": seven}); code != http.StatusBadRequest {
		t.Fatalf("7 items must be refused: %d %v", code, out)
	}
	if code, _ := f.post("/holds", map[string]any{"buyer_id": "", "request_id": "r", "item_ids": []int64{1}}); code != http.StatusBadRequest {
		t.Fatalf("an empty buyer must be refused: %d", code)
	}
}

func TestMetricsExplainWhatHappened(t *testing.T) {
	f := newFixture(t, 0.5, time.Millisecond)
	f.hold("alice", "r1", f.items[0])
	f.hold("bob", "r2", f.items[0]) // unavailable
	get := func() map[string]any {
		resp, err := http.Get(f.srv.URL + "/metrics")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return out
	}
	m := get()
	api := m["api"].(map[string]any)
	counters := api["counters"].(map[string]any)
	if counters["requests{POST /holds,201}"] != float64(1) || counters["requests{POST /holds,409}"] != float64(1) {
		t.Fatalf("request counters: %v", counters)
	}
	strat := m["strategy"].(map[string]any)
	if strat["db_pool_max_conns"] != float64(10) {
		t.Fatalf("the pool size in use must be reported (the test pool is 10): %v", strat["db_pool_max_conns"])
	}
	if _, ok := strat["db_pool_acquires_that_waited"]; !ok {
		t.Fatalf("pool waits must be reported: %v", strat)
	}
	if _, ok := m["strategy"].(map[string]any)["lock_busy"]; !ok {
		t.Fatalf("the locking strategy's counters must be exposed: %v", m["strategy"])
	}
	hist := api["histograms"].(map[string]any)["latency{POST /holds}"].(map[string]any)
	if hist["count"] != float64(2) {
		t.Fatalf("latency histogram: %v", hist)
	}
	if m["max_inflight"].(float64) < 1 {
		t.Fatalf("max_inflight: %v", m["max_inflight"])
	}
	again := get()["api"].(map[string]any)["counters"].(map[string]any)
	if len(again) != len(counters) {
		t.Fatal("reading /metrics must not change the metrics")
	}
}
