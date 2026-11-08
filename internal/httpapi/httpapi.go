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
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"resourcereservation/internal/db"
	"resourcereservation/internal/payment"
	"resourcereservation/internal/reservation"
)

// Deps are the pieces the handler needs.
type Deps struct {
	Service  reservation.Service
	Payment  *payment.Provider
	Pool     *pgxpool.Pool // only used by POST /events (demo and benchmark setup)
	HoldTTL  time.Duration
	MaxItems int // largest request, 6 by default
}

// Stats counts outcomes the benchmarks report (D4): how often a late confirm won, and how
// often a buyer paid and still lost the item.
type Stats struct {
	ConfirmedInTime      atomic.Int64
	ConfirmedAfterExpiry atomic.Int64
	ConfirmReplayed      atomic.Int64
	PaidButLost          atomic.Int64 // payment succeeded, then the hold was no longer the buyer's
	PaymentFailed        atomic.Int64
	PaymentTimedOut      atomic.Int64
	PaymentInProgress    atomic.Int64
}

// Handler serves the API.
type Handler struct {
	d     Deps
	Stats Stats
	mux   *http.ServeMux
}

// New builds the handler.
func New(d Deps) *Handler {
	if d.MaxItems == 0 {
		d.MaxItems = 6
	}
	h := &Handler{d: d, mux: http.NewServeMux()}
	h.mux.HandleFunc("POST /events", h.createEvent)
	h.mux.HandleFunc("GET /events/{id}/availability", h.availability)
	h.mux.HandleFunc("POST /holds", h.hold)
	h.mux.HandleFunc("POST /holds/confirm", h.confirm)
	h.mux.HandleFunc("POST /holds/release", h.release)
	h.mux.HandleFunc("GET /stats", h.stats)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

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
	if h.d.Pool == nil || b.Items <= 0 || b.Items > 100000 {
		fail(w, http.StatusBadRequest, "items must be between 1 and 100000")
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
	res, err := h.d.Service.Hold(r.Context(), reservation.HoldRequest{
		BuyerID: b.BuyerID, RequestID: b.RequestID, ItemIDs: b.ItemIDs, TTL: h.d.HoldTTL})
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
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

	started, err := h.d.Service.StartPayment(ctx, ref, h.markerTimeout())
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

	// If the client disconnects here the marker stays and goes stale after the timeout: the
	// same state a crashed server leaves behind (D5).
	paid, err := h.d.Payment.Pay(ctx)
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

	res, err := h.d.Service.Confirm(ctx, ref)
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	switch res.Outcome {
	case reservation.Confirmed:
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
