// Package reconcile checks the client's story against the database's (E5 d): every buyer the
// client was told "confirmed" must have exactly those bookings in the ledger, and every booking in
// the ledger must be explained by a buyer the client could have been told about.
//
// A request whose outcome the client never learned (a timeout or a killed server: its confirm ended
// in "error") is legitimately either booked or not booked. That is neither a match nor a mismatch:
// it is counted separately, and only a PARTIAL outcome (some of its items booked, some not) is a
// violation, because confirm is all-or-nothing.
package reconcile

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"resourcereservation/internal/loadgen"
)

// Report is the result of the comparison. The violation fields must be zero.
type Report struct {
	// What the client believes.
	ClientConfirmedBuyers int `json:"client_confirmed_buyers"`
	ClientConfirmedItems  int `json:"client_confirmed_items"`
	// What the database holds for the event.
	DBBookings int `json:"db_bookings"`

	MatchedBuyers int `json:"matched_buyers"` // confirmed by the client and fully present in the ledger

	// Requests of unknown outcome, resolved by reading the ledger.
	UnknownOutcomeBooked    int `json:"unknown_outcome_booked"`
	UnknownOutcomeNotBooked int `json:"unknown_outcome_not_booked"`

	// Violations.
	ConfirmedButMissing   int `json:"violation_confirmed_but_missing"`   // client told "confirmed", ledger lacks (some of) it
	UnexplainedBookings   int `json:"violation_unexplained_bookings"`    // booked, but the client was told otherwise or never asked
	UnknownOutcomePartial int `json:"violation_unknown_outcome_partial"` // an unknown-outcome buyer with only some items booked
}

// OK is true when the client's and the database's accounts agree.
func (r Report) OK() bool {
	return r.ConfirmedButMissing == 0 && r.UnexplainedBookings == 0 && r.UnknownOutcomePartial == 0
}

func (r Report) String() string {
	return fmt.Sprintf("client_confirmed=%d (%d items) db_bookings=%d matched=%d unknown_booked=%d unknown_not_booked=%d | "+
		"violations: confirmed_but_missing=%d unexplained_bookings=%d unknown_partial=%d",
		r.ClientConfirmedBuyers, r.ClientConfirmedItems, r.DBBookings, r.MatchedBuyers, r.UnknownOutcomeBooked,
		r.UnknownOutcomeNotBooked, r.ConfirmedButMissing, r.UnexplainedBookings, r.UnknownOutcomePartial)
}

// Compare is the pure comparison: booked maps buyer id to the set of item ids booked for that buyer.
func Compare(booked map[string]map[int64]bool, buyers []loadgen.BuyerResult) Report {
	var r Report
	seen := map[string]bool{}
	for _, b := range buyers {
		seen[b.Buyer] = true
		mine := booked[b.Buyer]
		n := 0
		for _, it := range b.Items {
			if mine[it] {
				n++
			}
		}
		switch {
		case b.HoldOutcome == loadgen.HoldHeld && b.ConfirmOutcome == "confirmed":
			r.ClientConfirmedBuyers++
			r.ClientConfirmedItems += len(b.Items)
			if n == len(b.Items) {
				r.MatchedBuyers++
			} else {
				r.ConfirmedButMissing++
			}
		case b.HoldOutcome == loadgen.HoldHeld && b.ConfirmOutcome == "error":
			switch {
			case n == len(b.Items):
				r.UnknownOutcomeBooked++
			case n == 0:
				r.UnknownOutcomeNotBooked++
			default:
				r.UnknownOutcomePartial++
			}
		default:
			// The client was told no, or never asked to confirm: no booking may exist for this buyer.
			r.UnexplainedBookings += len(mine)
		}
	}
	for buyer, items := range booked {
		if !seen[buyer] {
			r.UnexplainedBookings += len(items) // a booking from a buyer this run never sent
		}
	}
	for _, items := range booked {
		r.DBBookings += len(items)
	}
	return r
}

// Check reads the event's confirmed bookings from the database and compares them with the client log.
func Check(ctx context.Context, pool *pgxpool.Pool, eventID int64, buyers []loadgen.BuyerResult) (Report, error) {
	rows, err := pool.Query(ctx, `SELECT buyer_id, item_id FROM bookings WHERE event_id = $1 AND status = 'confirmed'`, eventID)
	if err != nil {
		return Report{}, err
	}
	defer rows.Close()
	booked := map[string]map[int64]bool{}
	for rows.Next() {
		var buyer string
		var item int64
		if err := rows.Scan(&buyer, &item); err != nil {
			return Report{}, err
		}
		if booked[buyer] == nil {
			booked[buyer] = map[int64]bool{}
		}
		booked[buyer][item] = true
	}
	if err := rows.Err(); err != nil {
		return Report{}, err
	}
	return Compare(booked, buyers), nil
}
