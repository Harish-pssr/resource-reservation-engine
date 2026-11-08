package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"resourcereservation/internal/checker"
	"resourcereservation/internal/db"
	"resourcereservation/internal/faults"
	"resourcereservation/internal/httpapi"
	"resourcereservation/internal/payment"
	"resourcereservation/internal/reservation/locking"
	"resourcereservation/internal/testdb"
)

const adminToken = "test-admin-token"

type faultFixture struct {
	t     *testing.T
	srv   *httptest.Server
	h     *httpapi.Handler
	pool  *pgxpool.Pool
	reg   *faults.Registry
	items []int64
}

// newFaultFixture builds the real handler and strategy with fault injection switched on. The payment
// takes payDelay; its timeout is 30 ms, so a payment marker goes stale after about 2 s.
func newFaultFixture(t *testing.T, enabled bool, token string, payDelay time.Duration) *faultFixture {
	t.Helper()
	pool := testdb.New(t, true)
	reg := faults.New(pool, enabled, time.Hour, slog.New(slog.DiscardHandler))
	reg.SetExit(func(int) {}) // never kill the test process: a crash is observed as "returned"
	svc := locking.New(pool)
	svc.SetFaults(reg)
	pcfg := payment.Config{DelayMin: payDelay, DelayMax: payDelay, Timeout: 30 * time.Millisecond}
	h := httpapi.New(httpapi.Deps{Service: svc, Payment: payment.New(pcfg).WithRand(func() float64 { return 0.5 }),
		Pool: pool, HoldTTL: time.Minute, Faults: reg, AdminToken: token})
	_, items, err := db.CreateEvent(context.Background(), pool, "t", 6)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &faultFixture{t: t, srv: srv, h: h, pool: pool, reg: reg, items: items}
}

func (f *faultFixture) call(method, path string, body any, token string) (int, map[string]any) {
	f.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, f.srv.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// arm sets a one-shot Error fault through the real control endpoint.
func (f *faultFixture) arm(point string) {
	f.t.Helper()
	if code, out := f.call("PUT", "/admin/faults/"+point, map[string]any{"action": "error", "remaining": 1}, adminToken); code != http.StatusNoContent {
		f.t.Fatalf("arming %s: %d %v", point, code, out)
	}
}

func (f *faultFixture) holdItem(buyer, req string, item int64) (int, map[string]any, []map[string]any) {
	f.t.Helper()
	code, out := f.call("POST", "/holds", map[string]any{"buyer_id": buyer, "request_id": req, "item_ids": []int64{item}}, "")
	var refs []map[string]any
	if raw, ok := out["items"].([]any); ok {
		for _, it := range raw {
			m := it.(map[string]any)
			refs = append(refs, map[string]any{"item_id": m["item_id"], "hold_version": m["hold_version"]})
		}
	}
	return code, out, refs
}

func (f *faultFixture) confirm(buyer string, refs []map[string]any) (int, map[string]any) {
	f.t.Helper()
	return f.call("POST", "/holds/confirm", map[string]any{"buyer_id": buyer, "items": refs}, "")
}

func (f *faultFixture) itemState(id int64) (state, holder string) {
	var h *string
	f.pool.QueryRow(context.Background(), `SELECT state, holder FROM items WHERE id = $1`, id).Scan(&state, &h)
	if h != nil {
		holder = *h
	}
	return
}

func (f *faultFixture) bookings() (n int) {
	f.pool.QueryRow(context.Background(), `SELECT count(*) FROM bookings WHERE status = 'confirmed'`).Scan(&n)
	return n
}

// ---- the guard ----

func TestFaultControlsNeedBothSwitches(t *testing.T) {
	body := map[string]any{"action": "error", "remaining": 1}
	cases := []struct {
		name    string
		enabled bool
		server  string // the token the server was started with
		sent    string
		want    int
	}{
		{"faults disabled, right token", false, adminToken, adminToken, http.StatusForbidden},
		{"enabled but no admin token configured", true, "", adminToken, http.StatusForbidden},
		{"enabled, no token sent", true, adminToken, "", http.StatusUnauthorized},
		{"enabled, wrong token", true, adminToken, "nope", http.StatusUnauthorized},
		{"enabled, right token", true, adminToken, adminToken, http.StatusNoContent},
	}
	for _, c := range cases {
		f := newFaultFixture(t, c.enabled, c.server, time.Millisecond)
		if code, out := f.call("PUT", "/admin/faults/hold.before_commit", body, c.sent); code != c.want {
			t.Errorf("%s: status %d, want %d (%v)", c.name, code, c.want, out)
		}
	}
}

func TestControlsRejectUnknownPointsAndListWhatIsArmed(t *testing.T) {
	f := newFaultFixture(t, true, adminToken, time.Millisecond)
	if code, _ := f.call("PUT", "/admin/faults/no.such.point", map[string]any{"action": "crash"}, adminToken); code != http.StatusBadRequest {
		t.Fatalf("an unknown point must be rejected: %d", code)
	}
	f.arm("hold.before_reply")
	code, out := f.call("GET", "/admin/faults", nil, adminToken)
	list, _ := out["faults"].([]any)
	if code != http.StatusOK || len(list) != 1 || out["known"] == nil {
		t.Fatalf("%d %v", code, out)
	}
	if code, _ := f.call("DELETE", "/admin/faults", nil, adminToken); code != http.StatusNoContent {
		t.Fatalf("clear: %d", code)
	}
	if _, out := f.call("GET", "/admin/faults", nil, adminToken); len(out["faults"].([]any)) != 0 {
		t.Fatal("clear must remove every fault")
	}
}

func TestMetricsReportWhetherFaultsAreEnabled(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		f := newFaultFixture(t, enabled, adminToken, time.Millisecond)
		_, m := f.call("GET", "/metrics", nil, "")
		if m["faults_enabled"] != enabled {
			t.Fatalf("faults_enabled %v, want %v: the benchmark runners rely on this to refuse to run", m["faults_enabled"], enabled)
		}
	}
}

// ---- each hook sits at exactly its row of docs/crash-analysis.md ----

// Row 8: dying inside the reserve transaction, before the commit, leaves nothing.
func TestHookHoldBeforeCommitLeavesNothing(t *testing.T) {
	f := newFaultFixture(t, true, adminToken, time.Millisecond)
	f.arm("hold.before_commit")
	if code, _, _ := f.holdItem("a", "r1", f.items[0]); code != http.StatusInternalServerError {
		t.Fatalf("the injected error must surface: %d", code)
	}
	if state, _ := f.itemState(f.items[0]); state != "free" {
		t.Fatalf("the transaction must have rolled back, item is %q", state)
	}
	if code, _, _ := f.holdItem("a", "r1", f.items[0]); code != http.StatusCreated {
		t.Fatalf("the retry is a first request and must succeed: %d", code)
	}
}

// Row 9: dying after the commit, before the reply: the hold exists and the retry returns the SAME hold.
func TestHookHoldBeforeReplyLeavesAHoldThatTheRetryReturns(t *testing.T) {
	f := newFaultFixture(t, true, adminToken, time.Millisecond)
	f.arm("hold.before_reply")
	if code, _, _ := f.holdItem("a", "r1", f.items[0]); code != http.StatusInternalServerError {
		t.Fatalf("the client never hears the answer: %d", code)
	}
	if state, holder := f.itemState(f.items[0]); state != "held" || holder != "a" {
		t.Fatalf("the hold was committed: %q %q", state, holder)
	}
	code, out, refs := f.holdItem("a", "r1", f.items[0]) // the client retries with the same request id
	if code != http.StatusCreated || refs[0]["hold_version"] != float64(1) {
		t.Fatalf("the retry must return the same hold (version 1): %d %v", code, out)
	}
}

// Row 12: dying after the marker commits, before the payment: retries are told "in progress".
func TestHookConfirmAfterMarkerBlocksRetriesUntilTheMarkerGoesStale(t *testing.T) {
	f := newFaultFixture(t, true, adminToken, time.Millisecond)
	_, _, refs := f.holdItem("a", "r1", f.items[0])
	f.arm("confirm.after_marker")
	if code, _ := f.confirm("a", refs); code != http.StatusInternalServerError {
		t.Fatalf("%d", code)
	}
	if code, out := f.confirm("a", refs); code != http.StatusConflict || out["outcome"] != "payment_in_progress" {
		t.Fatalf("a retry right after must be told in progress: %d %v", code, out)
	}
	time.Sleep(2300 * time.Millisecond) // the marker is stale after the payment timeout plus slack (about 2.03 s)
	if code, out := f.confirm("a", refs); code != http.StatusOK || out["outcome"] != "confirmed" {
		t.Fatalf("once the marker is stale the retry proceeds: %d %v", code, out)
	}
}

// Row 15: dying after the payment succeeded, before the confirm: paid, not booked, the hold persists.
func TestHookConfirmAfterPaymentLeavesAHoldAndNoBooking(t *testing.T) {
	f := newFaultFixture(t, true, adminToken, time.Millisecond)
	_, _, refs := f.holdItem("a", "r1", f.items[0])
	f.arm("confirm.after_payment")
	if code, _ := f.confirm("a", refs); code != http.StatusInternalServerError {
		t.Fatalf("%d", code)
	}
	if state, _ := f.itemState(f.items[0]); state != "held" || f.bookings() != 0 {
		t.Fatalf("paid but not booked: state %q, bookings %d", state, f.bookings())
	}
}

// Row 16: the confirm transaction fails: nothing is booked and the hold is intact.
func TestHookConfirmBeforeCommitBooksNothing(t *testing.T) {
	f := newFaultFixture(t, true, adminToken, time.Millisecond)
	_, _, refs := f.holdItem("a", "r1", f.items[0])
	f.arm("confirm.before_commit")
	if code, _ := f.confirm("a", refs); code != http.StatusInternalServerError {
		t.Fatalf("%d", code)
	}
	if state, holder := f.itemState(f.items[0]); state != "held" || holder != "a" || f.bookings() != 0 {
		t.Fatalf("the failed transaction must leave the hold alone: %q %q bookings %d", state, holder, f.bookings())
	}
}

// Row 17: dying after the confirm commits, before the reply: booked once; the retry is a replay.
func TestHookConfirmBeforeReplyLeavesABookingThatTheRetryRecognises(t *testing.T) {
	f := newFaultFixture(t, true, adminToken, time.Millisecond)
	_, _, refs := f.holdItem("a", "r1", f.items[0])
	f.arm("confirm.before_reply")
	if code, _ := f.confirm("a", refs); code != http.StatusInternalServerError {
		t.Fatalf("%d", code)
	}
	if f.bookings() != 1 {
		t.Fatalf("the booking was committed: %d", f.bookings())
	}
	code, out := f.confirm("a", refs)
	if code != http.StatusOK || out["replayed"] != true || f.bookings() != 1 {
		t.Fatalf("the retry must be a replay with no second booking: %d %v bookings %d", code, out, f.bookings())
	}
	if r, _ := checker.Check(context.Background(), f.pool); !r.OK() {
		t.Fatalf("checker: %v", r)
	}
}

// Delay: the payment-delay fault slows the payment.
func TestHookPaymentDelaySlowsThePayment(t *testing.T) {
	f := newFaultFixture(t, true, adminToken, time.Millisecond)
	_, _, refs := f.holdItem("a", "r1", f.items[0])
	f.call("PUT", "/admin/faults/payment.delay", map[string]any{"action": "delay", "params": map[string]any{"delay_ms": 300}, "remaining": 1}, adminToken)
	start := time.Now()
	f.confirm("a", refs)
	if time.Since(start) < 280*time.Millisecond {
		t.Fatalf("the confirm took only %v", time.Since(start))
	}
}

// ---- the negative control for the payment marker (D5, D13) ----

// Two confirms for one hold at the same time. With the marker only one payment may be in flight; with
// the marker switched off by the state fault, two payments run at once: the control is non-zero, so a
// "1" with the marker on means something.
func TestPaymentMarkerNegativeControl(t *testing.T) {
	twoConfirms := func(f *faultFixture, refs []map[string]any) {
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); f.confirm("a", refs) }()
		}
		wg.Wait()
	}
	maxConcurrent := func(f *faultFixture) float64 {
		_, m := f.call("GET", "/metrics", nil, "")
		return m["outcomes"].(map[string]any)["max_concurrent_payments_per_hold"].(float64)
	}

	protected := newFaultFixture(t, true, adminToken, 400*time.Millisecond)
	_, _, refs := protected.holdItem("a", "r1", protected.items[0])
	twoConfirms(protected, refs)
	if got := maxConcurrent(protected); got != 1 {
		t.Fatalf("with the marker, at most one payment may be in flight per hold: %v", got)
	}

	control := newFaultFixture(t, true, adminToken, 400*time.Millisecond)
	control.call("PUT", "/admin/faults/confirm.skip_payment_marker", map[string]any{"action": "disable"}, adminToken)
	_, _, refs = control.holdItem("a", "r1", control.items[0])
	twoConfirms(control, refs)
	if got := maxConcurrent(control); got != 2 {
		t.Fatalf("with the marker switched off, two payments run at once (the control): %v", got)
	}
}

func TestInjectedErrorTextNamesThePoint(t *testing.T) {
	f := newFaultFixture(t, true, adminToken, time.Millisecond)
	f.arm("hold.before_reply")
	_, out, _ := f.holdItem("a", "r1", f.items[0])
	if msg, _ := out["error"].(string); !strings.Contains(msg, "hold.before_reply") {
		t.Fatalf("%v", out)
	}
}
