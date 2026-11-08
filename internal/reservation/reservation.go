// Package reservation defines what every contention strategy must do. The strategies
// (locking, optimistic, fast path, and the naive negative control) differ only in how they
// reach the answer; the behaviour below is the same for all of them and is checked by the
// shared suite in reservationtest.
package reservation

import (
	"context"
	"time"
)

// Outcome is the result of an operation. Retrying only helps for LockBusy (D6, D7).
type Outcome string

const (
	Held              Outcome = "held"
	Unavailable       Outcome = "unavailable"         // item is held or confirmed: final
	LockBusy          Outcome = "lock_busy"           // transient conflict: retry shortly
	PaymentStarted    Outcome = "payment_started"     // caller may now call the payment provider
	PaymentInProgress Outcome = "payment_in_progress" // another attempt is mid-payment (D5)
	HoldLost          Outcome = "hold_lost"           // the hold is no longer the caller's (D4)
	Confirmed         Outcome = "confirmed"
	Released          Outcome = "released"
	NotHeld           Outcome = "not_held" // release of something the caller does not hold: a no-op
)

// ItemRef names an item and the hold_version the caller was given when it held it.
type ItemRef struct {
	ItemID      int64
	HoldVersion int64
}

// HoldRequest asks to hold 1..M specific items, all or nothing (D6).
type HoldRequest struct {
	BuyerID   string
	RequestID string // same id on a retry returns the same hold (D5)
	ItemIDs   []int64
	TTL       time.Duration
}

// HeldItem describes one item of a successful hold.
type HeldItem struct {
	ItemID      int64
	HoldVersion int64
	ExpiresAt   time.Time
}

// HoldResult carries Items only when Outcome is Held.
type HoldResult struct {
	Outcome Outcome
	Items   []HeldItem
}

// Refs converts a successful hold into the references later calls need.
func (r HoldResult) Refs() []ItemRef {
	out := make([]ItemRef, len(r.Items))
	for i, it := range r.Items {
		out[i] = ItemRef{ItemID: it.ItemID, HoldVersion: it.HoldVersion}
	}
	return out
}

// HoldRef identifies a hold for payment, confirm and release.
type HoldRef struct {
	BuyerID string
	Items   []ItemRef
}

// ConfirmResult reports how a confirm ended.
type ConfirmResult struct {
	Outcome     Outcome
	AfterExpiry bool // the hold had expired but nobody took the item, so the late confirm won (D4)
	Replayed    bool // a retry of a confirm that had already succeeded (D5)
}

// Service is implemented by every strategy. All multi-item calls are all-or-nothing.
type Service interface {
	// Hold reserves the items for the buyer, or returns Unavailable / LockBusy and changes nothing.
	Hold(ctx context.Context, req HoldRequest) (HoldResult, error)
	// StartPayment sets the in-progress marker before the payment step. A marker older than
	// timeout counts as abandoned (the attempt crashed) and may be replaced (D5).
	StartPayment(ctx context.Context, ref HoldRef, timeout time.Duration) (Outcome, error)
	// Confirm books the items if the hold version still matches, even after expiry (D4).
	Confirm(ctx context.Context, ref HoldRef) (ConfirmResult, error)
	// Release frees the items if the hold is still the caller's; otherwise a no-op (D4, D5).
	Release(ctx context.Context, ref HoldRef) (Outcome, error)
	// Availability counts items of an event that are free or whose hold has expired (D3).
	Availability(ctx context.Context, eventID int64) (int, error)
}
