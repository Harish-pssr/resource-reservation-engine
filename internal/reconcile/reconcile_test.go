package reconcile

import (
	"context"
	"testing"

	"resourcereservation/internal/db"
	"resourcereservation/internal/loadgen"
	"resourcereservation/internal/testdb"
)

func confirmed(buyer string, items ...int64) loadgen.BuyerResult {
	return loadgen.BuyerResult{Buyer: buyer, Items: items, Sent: true, HoldOutcome: loadgen.HoldHeld, ConfirmOutcome: "confirmed"}
}

func unknown(buyer string, items ...int64) loadgen.BuyerResult {
	return loadgen.BuyerResult{Buyer: buyer, Items: items, Sent: true, HoldOutcome: loadgen.HoldHeld, ConfirmOutcome: "error"}
}

func declined(buyer, hold, confirm string, items ...int64) loadgen.BuyerResult {
	return loadgen.BuyerResult{Buyer: buyer, Items: items, Sent: true, HoldOutcome: hold, ConfirmOutcome: confirm}
}

func ledger(m map[string][]int64) map[string]map[int64]bool {
	out := map[string]map[int64]bool{}
	for b, items := range m {
		out[b] = map[int64]bool{}
		for _, it := range items {
			out[b][it] = true
		}
	}
	return out
}

func TestAgreementIsOK(t *testing.T) {
	r := Compare(ledger(map[string][]int64{"a": {1, 2}, "b": {3}}), []loadgen.BuyerResult{
		confirmed("a", 1, 2), confirmed("b", 3),
		declined("c", loadgen.HoldUnavailable, "", 1),
		declined("d", loadgen.HoldHeld, "payment_failed", 5),
	})
	if !r.OK() || r.MatchedBuyers != 2 || r.ClientConfirmedItems != 3 || r.DBBookings != 3 {
		t.Fatalf("%v", r)
	}
}

func TestConfirmedButMissingFromTheLedgerIsAViolation(t *testing.T) {
	r := Compare(ledger(map[string][]int64{"a": {1}}), []loadgen.BuyerResult{confirmed("a", 1, 2)}) // item 2 never booked
	if r.OK() || r.ConfirmedButMissing != 1 {
		t.Fatalf("%v", r)
	}
}

func TestBookingForABuyerTheClientWasToldNoIsAViolation(t *testing.T) {
	r := Compare(ledger(map[string][]int64{"c": {1}}), []loadgen.BuyerResult{declined("c", loadgen.HoldHeld, "payment_failed", 1)})
	if r.OK() || r.UnexplainedBookings != 1 {
		t.Fatalf("%v", r)
	}
}

func TestBookingFromAnUnknownBuyerIsAViolation(t *testing.T) {
	r := Compare(ledger(map[string][]int64{"ghost": {9}}), nil)
	if r.OK() || r.UnexplainedBookings != 1 {
		t.Fatalf("%v", r)
	}
}

// A killed server leaves requests whose answer never arrived. Either result is legitimate.
func TestUnknownOutcomeMayBeBookedOrNotButNeverPartial(t *testing.T) {
	r := Compare(ledger(map[string][]int64{"x": {1, 2}}), []loadgen.BuyerResult{
		unknown("x", 1, 2), // the confirm went through before the kill
		unknown("y", 3, 4), // it did not
	})
	if !r.OK() || r.UnknownOutcomeBooked != 1 || r.UnknownOutcomeNotBooked != 1 {
		t.Fatalf("%v", r)
	}
	partial := Compare(ledger(map[string][]int64{"x": {1}}), []loadgen.BuyerResult{unknown("x", 1, 2)})
	if partial.OK() || partial.UnknownOutcomePartial != 1 {
		t.Fatalf("a half-booked request breaks all-or-nothing: %v", partial)
	}
}

func TestCheckReadsTheRealLedger(t *testing.T) {
	pool := testdb.New(t, true)
	ctx := context.Background()
	ev, items, err := db.CreateEvent(ctx, pool, "e", 3)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items[:2] {
		if _, err := pool.Exec(ctx, `INSERT INTO bookings (item_id, event_id, buyer_id, hold_version) VALUES ($1, $2, 'a', 1)`, it, ev); err != nil {
			t.Fatal(err)
		}
	}
	r, err := Check(ctx, pool, ev, []loadgen.BuyerResult{confirmed("a", items[0], items[1])})
	if err != nil || !r.OK() || r.DBBookings != 2 {
		t.Fatalf("%v %v", r, err)
	}
	// The same ledger against a client that claims a third item: now they disagree.
	r, _ = Check(ctx, pool, ev, []loadgen.BuyerResult{confirmed("a", items[0], items[1], items[2])})
	if r.OK() {
		t.Fatalf("a client claiming an unbooked item must be flagged: %v", r)
	}
}
