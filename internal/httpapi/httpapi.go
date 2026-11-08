// Package httpapi exposes the engine over HTTP. A booking is two user-facing steps:
//
//	POST /holds           the buyer selects items: a hold
//	POST /holds/confirm   the buyer pays: payment marker, simulated payment, then confirm
//	POST /holds/release   optional: the buyer backs out
//
// Payment failure or timeout releases the hold on the server side.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"resourcereservation/internal/db"
	"resourcereservation/internal/faults"
	"resourcereservation/internal/metrics"
	"resourcereservation/internal/payment"
	"resourcereservation/internal/reservation"
	"resourcereservation/internal/waitingroom"
	"resourcereservation/web"
)

// Deps are the pieces the handler needs.
type Deps struct {
	Service  reservation.Service
	Payment  *payment.Provider
	Pool     *pgxpool.Pool // only used by POST /events (demo and benchmark setup)
	HoldTTL  time.Duration
	MaxItems int // largest request, 6 by default
	// Room is the waiting room (D9); nil disables admission control.
	Room *waitingroom.Room
	// Faults is the fault registry (D13); nil disables injection. AdminToken guards its controls.
	Faults     *faults.Registry
	AdminToken string
	// Info is shown by the console (strategy, schema, profile...); RedisPing, if set, feeds /healthz.
	Info      map[string]string
	RedisPing func(context.Context) error
	// Metrics receives request counters and latency histograms; New creates one if nil.
	Metrics *metrics.Registry
}

// Stats counts outcomes the benchmarks report (D4): how often a late confirm won, and how
// often a buyer paid and still lost the item.
type Stats struct {
	ConfirmedInTime       atomic.Int64
	ConfirmedAfterExpiry  atomic.Int64
	ConfirmReplayed       atomic.Int64
	PaidButLost           atomic.Int64 // payment succeeded, then the hold was no longer the buyer's
	PaymentFailed         atomic.Int64
	PaymentTimedOut       atomic.Int64
	PaymentInProgress     atomic.Int64
	PaymentAttempts       atomic.Int64 // payments started (simulated)
	MaxConcurrentPayments atomic.Int64 // most payments in flight at once for one hold: above 1 means a double payment (D5 marker)
	RoomFailOpen          atomic.Int64 // requests let through because Redis was unreachable (D9)
}

// Handler serves the API.
type Handler struct {
	d     Deps
	Stats Stats
	mux   *http.ServeMux

	inflight    atomic.Int64
	maxInflight atomic.Int64

	payMu     sync.Mutex
	payActive map[string]int // payments in flight, per hold
}

// New builds the handler.
func New(d Deps) *Handler {
	if d.MaxItems == 0 {
		d.MaxItems = 6
	}
	if d.Metrics == nil {
		d.Metrics = metrics.NewRegistry()
	}
	h := &Handler{d: d, mux: http.NewServeMux(), payActive: map[string]int{}}
	h.mux.HandleFunc("POST /events", h.createEvent)
	h.mux.HandleFunc("GET /events/{id}/availability", h.availability)
	h.mux.HandleFunc("POST /holds", h.hold)
	h.mux.HandleFunc("POST /holds/confirm", h.confirm)
	h.mux.HandleFunc("POST /holds/release", h.release)
	h.mux.HandleFunc("POST /queue/join", h.queueJoin)
	h.mux.HandleFunc("GET /queue/{buyer}", h.queuePoll)
	h.mux.HandleFunc("GET /stats", h.stats)
	h.registerAdmin()
	h.registerUI()
	// The console. Any path no API route claims is looked up in the embedded web folder.
	h.mux.Handle("GET /", noCache(http.FileServerFS(web.FS)))
	h.mux.HandleFunc("GET /metrics", h.metrics)
	return h
}

// statusWriter remembers the status code so the middleware can count it.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) { w.status = code; w.ResponseWriter.WriteHeader(code) }

// route is a low-cardinality name for a request, used as the metric key.
func route(r *http.Request) string {
	if strings.HasPrefix(r.URL.Path, "/events/") && strings.HasSuffix(r.URL.Path, "/availability") {
		return r.Method + " /events/:id/availability"
	}
	if strings.HasPrefix(r.URL.Path, "/events/") && (strings.HasSuffix(r.URL.Path, "/items") || strings.HasSuffix(r.URL.Path, "/bookings")) {
		return r.Method + " /events/:id/" + r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	}
	if strings.HasPrefix(r.URL.Path, "/admin/faults/") {
		return r.Method + " /admin/faults/:point"
	}
	if strings.HasPrefix(r.URL.Path, "/queue/") && r.URL.Path != "/queue/join" {
		return r.Method + " /queue/:buyer"
	}
	return r.Method + " " + r.URL.Path
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/metrics" { // reading the metrics must not change them
		h.mux.ServeHTTP(w, r)
		return
	}
	cur := h.inflight.Add(1)
	for {
		old := h.maxInflight.Load()
		if cur <= old || h.maxInflight.CompareAndSwap(old, cur) {
			break
		}
	}
	start := time.Now()
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	h.mux.ServeHTTP(sw, r)
	h.inflight.Add(-1)
	name := route(r)
	h.d.Metrics.Counter("requests{" + name + "," + strconv.Itoa(sw.status) + "}").Add(1)
	h.d.Metrics.Histogram("latency{" + name + "}").Observe(time.Since(start))
}

// ---- wire types ----

type itemRef struct {
	ItemID      int64 `json:"item_id"`
	HoldVersion int64 `json:"hold_version"`
}

type holdBody struct {
	BuyerID   string  `json:"buyer_id"`
	RequestID string  `json:"request_id"`
	ItemIDs   []int64 `json:"item_ids"`
}

type refBody struct {
	BuyerID string    `json:"buyer_id"`
	Items   []itemRef `json:"items"`
}

func (b refBody) ref() reservation.HoldRef {
	r := reservation.HoldRef{BuyerID: b.BuyerID}
	for _, it := range b.Items {
		r.Items = append(r.Items, reservation.ItemRef{ItemID: it.ItemID, HoldVersion: it.HoldVersion})
	}
	return r
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func outcome(w http.ResponseWriter, status int, o string, extra map[string]any) {
	body := map[string]any{"outcome": o}
	for k, v := range extra {
		body[k] = v
	}
	writeJSON(w, status, body)
}

func fail(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": msg})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		fail(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

// ---- handlers ----

func (h *Handler) createEvent(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Name  string `json:"name"`
		Items int    `json:"items"`
	}
	if !decode(w, r, &b) {
		return
	}
	if h.d.Pool == nil || b.Items <= 0 || b.Items > 500000 {
		fail(w, http.StatusBadRequest, "items must be between 1 and 500000")
		return
	}
	id, items, err := db.CreateEvent(r.Context(), h.d.Pool, b.Name, b.Items)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"event_id": id, "item_ids": items})
}

func (h *Handler) availability(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, http.StatusBadRequest, "bad event id")
		return
	}
	n, err := h.d.Service.Availability(r.Context(), id)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"event_id": id, "available": n, "approximate": true})
}

func (h *Handler) hold(w http.ResponseWriter, r *http.Request) {
	var b holdBody
	if !decode(w, r, &b) {
		return
	}
	if b.BuyerID == "" || b.RequestID == "" || len(b.ItemIDs) == 0 || len(b.ItemIDs) > h.d.MaxItems {
		fail(w, http.StatusBadRequest, "buyer_id, request_id and 1.."+strconv.Itoa(h.d.MaxItems)+" item_ids are required")
		return
	}
	if !h.admitted(w, r, b.BuyerID) {
		return
	}
	res, err := h.d.Service.Hold(r.Context(), reservation.HoldRequest{
		BuyerID: b.BuyerID, RequestID: b.RequestID, ItemIDs: b.ItemIDs, TTL: h.d.HoldTTL})
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if res.Outcome == reservation.Held { // row 9: dying here leaves a committed hold the client never hears about
		if err := h.d.Faults.Hit(r.Context(), "hold.before_reply"); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	switch res.Outcome {
	case reservation.Held:
		items := make([]map[string]any, len(res.Items))
		for i, it := range res.Items {
			items[i] = map[string]any{"item_id": it.ItemID, "hold_version": it.HoldVersion, "expires_at": it.ExpiresAt}
		}
		outcome(w, http.StatusCreated, "held", map[string]any{"items": items})
	case reservation.Unavailable:
		outcome(w, http.StatusConflict, "unavailable", nil) // final: do not retry
	case reservation.LockBusy:
		outcome(w, http.StatusServiceUnavailable, "lock_busy", nil) // transient: retry shortly
	default:
		fail(w, http.StatusInternalServerError, "unexpected outcome "+string(res.Outcome))
	}
}

func (h *Handler) validRef(w http.ResponseWriter, b refBody) bool {
	if b.BuyerID == "" || len(b.Items) == 0 || len(b.Items) > h.d.MaxItems {
		fail(w, http.StatusBadRequest, "buyer_id and 1.."+strconv.Itoa(h.d.MaxItems)+" items are required")
		return false
	}
	return true
}

// markerTimeout is how old a payment marker must be before it counts as abandoned: the longest a
// payment attempt can take, plus slack (D5).
func (h *Handler) markerTimeout() time.Duration { return h.d.Payment.Config().Timeout + 2*time.Second }

func (h *Handler) confirm(w http.ResponseWriter, r *http.Request) {
	var b refBody
	if !decode(w, r, &b) || !h.validRef(w, b) {
		return
	}
	ref := b.ref()
	ctx := r.Context()

	var (
		started reservation.Outcome
		err     error
	)
	if _, skip := h.d.Faults.Active("confirm.skip_payment_marker"); skip {
		started = reservation.PaymentStarted // negative control (D13): the marker is neither checked nor set
	} else {
		started, err = h.d.Service.StartPayment(ctx, ref, h.markerTimeout())
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	switch started {
	case reservation.HoldLost:
		outcome(w, http.StatusConflict, "hold_lost", nil)
		return
	case reservation.PaymentInProgress:
		h.Stats.PaymentInProgress.Add(1)
		outcome(w, http.StatusConflict, "payment_in_progress", nil)
		return
	case reservation.Confirmed: // a retry of a confirm that already succeeded
		h.Stats.ConfirmReplayed.Add(1)
		outcome(w, http.StatusOK, "confirmed", map[string]any{"replayed": true})
		return
	case reservation.PaymentStarted:
	default:
		fail(w, http.StatusInternalServerError, "unexpected outcome "+string(started))
		return
	}

	if err := h.d.Faults.Hit(ctx, "confirm.after_marker"); err != nil { // row 12: the marker is committed, no payment yet
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := h.d.Faults.Hit(ctx, "payment.delay"); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	// If the client disconnects here the marker stays and goes stale after the timeout: the
	// same state a crashed server leaves behind (D5).
	done := h.trackPayment(ref)
	paid, err := h.d.Payment.PayWith(ctx, func(c context.Context) error { return h.d.Faults.Hit(c, "confirm.during_payment") }) // row 13
	done()
	if err != nil {
		fail(w, http.StatusServiceUnavailable, "payment interrupted: "+err.Error())
		return
	}
	switch paid {
	case payment.Failed, payment.TimedOut:
		// Release on a context that outlives the request, so a disconnect cannot leave the hold.
		if err := h.releaseDetached(ctx, ref); err != nil {
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		if paid == payment.Failed {
			h.Stats.PaymentFailed.Add(1)
			outcome(w, http.StatusPaymentRequired, "payment_failed", nil)
		} else {
			h.Stats.PaymentTimedOut.Add(1)
			outcome(w, http.StatusPaymentRequired, "payment_timeout", nil)
		}
		return
	}

	if err := h.d.Faults.Hit(ctx, "confirm.after_payment"); err != nil { // row 15: paid, not yet booked
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	res, err := h.d.Service.Confirm(ctx, ref)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	switch res.Outcome {
	case reservation.Confirmed:
		if err := h.d.Faults.Hit(ctx, "confirm.before_reply"); err != nil { // row 17: booked, the client never hears
			fail(w, http.StatusInternalServerError, err.Error())
			return
		}
		switch {
		case res.Replayed:
			h.Stats.ConfirmReplayed.Add(1)
		case res.AfterExpiry:
			h.Stats.ConfirmedAfterExpiry.Add(1)
		default:
			h.Stats.ConfirmedInTime.Add(1)
		}
		outcome(w, http.StatusOK, "confirmed", map[string]any{"after_expiry": res.AfterExpiry, "replayed": res.Replayed})
	case reservation.HoldLost:
		h.Stats.PaidButLost.Add(1) // simulated payment: counted, nothing is refunded (D4)
		outcome(w, http.StatusConflict, "hold_lost", map[string]any{"paid": true})
	default:
		fail(w, http.StatusInternalServerError, "unexpected outcome "+string(res.Outcome))
	}
}

func (h *Handler) release(w http.ResponseWriter, r *http.Request) {
	var b refBody
	if !decode(w, r, &b) || !h.validRef(w, b) {
		return
	}
	o, err := h.d.Service.Release(r.Context(), b.ref())
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	outcome(w, http.StatusOK, string(o), nil) // not_held is a successful no-op, so a retry is safe
}

func (h *Handler) stats(w http.ResponseWriter, r *http.Request) {
	s := &h.Stats
	writeJSON(w, http.StatusOK, map[string]int64{
		"confirmed_in_time": s.ConfirmedInTime.Load(), "confirmed_after_expiry": s.ConfirmedAfterExpiry.Load(),
		"confirm_replayed": s.ConfirmReplayed.Load(), "paid_but_lost": s.PaidButLost.Load(),
		"payment_failed": s.PaymentFailed.Load(), "payment_timed_out": s.PaymentTimedOut.Load(),
		"payment_in_progress_replies": s.PaymentInProgress.Load(),
	})
}

// releaseDetached releases the hold on a context that keeps the request's values but not its
// cancellation, with its own deadline, so a client disconnect cannot stop the release that frees a
// failed buyer's hold.
func (h *Handler) releaseDetached(ctx context.Context, ref reservation.HoldRef) error {
	c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_, err := h.d.Service.Release(c, ref)
	return err
}

// metrics reports what the API and the strategy counted, to explain a benchmark.
func (h *Handler) metrics(w http.ResponseWriter, r *http.Request) {
	strategy := map[string]int64{}
	if cp, ok := h.d.Service.(reservation.CounterProvider); ok {
		strategy = cp.Counters()
	}
	if h.d.Pool != nil { // the connection pool: how big it is and how often a request had to wait for a connection
		st := h.d.Pool.Stat()
		strategy["db_pool_max_conns"] = int64(st.MaxConns())
		strategy["db_pool_acquired_conns"] = int64(st.AcquiredConns())
		strategy["db_pool_idle_conns"] = int64(st.IdleConns())
		strategy["db_pool_acquires_total"] = st.AcquireCount()
		strategy["db_pool_acquires_that_waited"] = st.EmptyAcquireCount()
		strategy["db_pool_acquire_wait_ms_total"] = st.AcquireDuration().Milliseconds()
	}
	if h.d.Room != nil {
		for k, v := range h.d.Room.Counters() {
			strategy[k] = v
		}
		strategy["room_fail_open"] = h.Stats.RoomFailOpen.Load()
		if n, err := h.d.Room.QueueLength(r.Context()); err == nil {
			strategy["room_queue_length"] = n // queue depth now
		}
	}
	s := &h.Stats
	writeJSON(w, http.StatusOK, map[string]any{
		"api": h.d.Metrics.Snapshot(), "strategy": strategy,
		"inflight": h.inflight.Load(), "max_inflight": h.maxInflight.Load(),
		"faults_enabled": h.d.Faults.Enabled(),
		"outcomes": map[string]int64{
			"confirmed_in_time": s.ConfirmedInTime.Load(), "confirmed_after_expiry": s.ConfirmedAfterExpiry.Load(),
			"confirm_replayed": s.ConfirmReplayed.Load(), "paid_but_lost": s.PaidButLost.Load(),
			"payment_failed": s.PaymentFailed.Load(), "payment_timed_out": s.PaymentTimedOut.Load(),
			"payment_in_progress_replies": s.PaymentInProgress.Load(),
			"payment_attempts":            s.PaymentAttempts.Load(), "max_concurrent_payments_per_hold": s.MaxConcurrentPayments.Load(),
		},
	})
}

// ---- waiting room (D9) ----

// admitted enforces the admission pass on POST /holds. With no room configured everything passes.
// If Redis is unreachable the request is let through (fail open): the database stays correct, only
// the surge protection is missing, and the count of such requests is reported.
func (h *Handler) admitted(w http.ResponseWriter, r *http.Request, buyer string) bool {
	if h.d.Room == nil {
		return true
	}
	owner, err := h.d.Room.Validate(r.Context(), r.Header.Get("X-Admission-Token"))
	switch {
	case err == nil && owner == buyer:
		return true
	case err == nil, errors.Is(err, waitingroom.ErrNoPass):
		outcome(w, http.StatusForbidden, "not_admitted", nil) // join the queue first
		return false
	default:
		h.Stats.RoomFailOpen.Add(1)
		return true
	}
}

func roomBody(st waitingroom.Status) map[string]any {
	body := map[string]any{"state": st.State}
	switch st.State {
	case waitingroom.Queued:
		body["position"], body["ticket"] = st.Position, st.Ticket
	case waitingroom.Admitted:
		body["token"], body["admit_order"] = st.Token, st.AdmitOrder
	}
	if st.RetryAfter > 0 {
		body["retry_after_ms"] = st.RetryAfter.Milliseconds()
	}
	return body
}

func (h *Handler) roomReply(w http.ResponseWriter, st waitingroom.Status, err error) {
	switch {
	case err != nil: // fail open: tell the buyer to go ahead with an "open" pass, which holds accept only while Redis is down
		h.Stats.RoomFailOpen.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{"state": waitingroom.Admitted, "token": "open", "degraded": true})
	case st.State == waitingroom.Retry:
		writeJSON(w, http.StatusTooManyRequests, roomBody(st))
	case st.State == waitingroom.NotQueued:
		writeJSON(w, http.StatusNotFound, roomBody(st))
	default:
		writeJSON(w, http.StatusOK, roomBody(st))
	}
}

func (h *Handler) queueJoin(w http.ResponseWriter, r *http.Request) {
	if h.d.Room == nil {
		fail(w, http.StatusNotFound, "no waiting room is configured")
		return
	}
	var b struct {
		BuyerID string `json:"buyer_id"`
	}
	if !decode(w, r, &b) {
		return
	}
	if b.BuyerID == "" {
		fail(w, http.StatusBadRequest, "buyer_id is required")
		return
	}
	st, err := h.d.Room.Join(r.Context(), b.BuyerID)
	h.roomReply(w, st, err)
}

func (h *Handler) queuePoll(w http.ResponseWriter, r *http.Request) {
	if h.d.Room == nil {
		fail(w, http.StatusNotFound, "no waiting room is configured")
		return
	}
	st, err := h.d.Room.Poll(r.Context(), r.PathValue("buyer"))
	h.roomReply(w, st, err)
}

// trackPayment records that a payment for this hold is in flight and returns the function that records
// its end. If the payment marker (D5) ever lets a second payment start while one is running, the
// per-hold maximum rises above 1: the measurement behind the marker's negative control (D13).
func (h *Handler) trackPayment(ref reservation.HoldRef) func() {
	ids := make([]int64, len(ref.Items))
	for i, it := range ref.Items {
		ids[i] = it.ItemID
	}
	slices.Sort(ids)
	key := ref.BuyerID + fmt.Sprint(ids)
	h.Stats.PaymentAttempts.Add(1)
	h.payMu.Lock()
	h.payActive[key]++
	n := int64(h.payActive[key])
	h.payMu.Unlock()
	for {
		old := h.Stats.MaxConcurrentPayments.Load()
		if n <= old || h.Stats.MaxConcurrentPayments.CompareAndSwap(old, n) {
			break
		}
	}
	return func() {
		h.payMu.Lock()
		if h.payActive[key]--; h.payActive[key] <= 0 {
			delete(h.payActive, key)
		}
		h.payMu.Unlock()
	}
}

// noCache makes the browser re-check the console files on every load, so a change is never hidden by a stale copy.
func noCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		next.ServeHTTP(w, r)
	})
}
