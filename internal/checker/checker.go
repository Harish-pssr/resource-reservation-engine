// Package checker verifies the final state of a run from the database alone (E5). It never
// uses the strategy under test: it only reads items, events and the bookings ledger, so a bug
// in a strategy cannot hide from it.
package checker

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Report is the checker's verdict. All counts are zero for a correct run.
type Report struct {
	// (a) Items with more than one confirmed booking, and how many bookings beyond the first.
	DoubleSoldItems int `json:"double_sold_items"`
	ExtraBookings   int `json:"extra_bookings"`
	// (b) Confirmed bookings beyond an event's capacity, summed over events.
	Oversold int `json:"oversold"`
	// (c) Expired holds that cannot be taken over. In the lazy-expiry design (D3) this can only
	// be non-zero through a bug in the availability predicate or a constraint.
	StuckHolds int `json:"stuck_holds"`
	// Ledger and item rows that disagree: confirmed items with no confirmed booking, and
	// confirmed bookings whose item is not in state confirmed.
	ConfirmedItemsWithoutBooking int `json:"confirmed_items_without_booking"`
	BookingsOnUnconfirmedItems   int `json:"bookings_on_unconfirmed_items"`

	// Context for reading the numbers.
	ConfirmedBookings int `json:"confirmed_bookings"`
	TotalCapacity     int `json:"total_capacity"`
}

// OK is true when every invariant held.
func (r Report) OK() bool {
	return r.DoubleSoldItems == 0 && r.ExtraBookings == 0 && r.Oversold == 0 && r.StuckHolds == 0 &&
		r.ConfirmedItemsWithoutBooking == 0 && r.BookingsOnUnconfirmedItems == 0
}

func (r Report) String() string {
	return fmt.Sprintf("double_sold_items=%d extra_bookings=%d oversold=%d stuck_holds=%d "+
		"confirmed_items_without_booking=%d bookings_on_unconfirmed_items=%d (confirmed_bookings=%d capacity=%d)",
		r.DoubleSoldItems, r.ExtraBookings, r.Oversold, r.StuckHolds,
		r.ConfirmedItemsWithoutBooking, r.BookingsOnUnconfirmedItems, r.ConfirmedBookings, r.TotalCapacity)
}

// Check reads the database and returns the report. Run it after the load has stopped.
func Check(ctx context.Context, pool *pgxpool.Pool) (Report, error) {
	var r Report

	// (a) no item has more than one confirmed booking.
	if err := pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(sum(c - 1), 0) FROM (
			SELECT count(*) AS c FROM bookings WHERE status = 'confirmed'
			GROUP BY item_id HAVING count(*) > 1) t`).Scan(&r.DoubleSoldItems, &r.ExtraBookings); err != nil {
		return r, fmt.Errorf("double-sold check: %w", err)
	}

	// (b) confirmed bookings never exceed capacity.
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(sum(GREATEST(n - capacity, 0)), 0), COALESCE(sum(n), 0), COALESCE(sum(capacity), 0) FROM (
			SELECT e.capacity, count(b.id) AS n
			FROM events e LEFT JOIN bookings b ON b.event_id = e.id AND b.status = 'confirmed'
			GROUP BY e.id, e.capacity) t`).Scan(&r.Oversold, &r.ConfirmedBookings, &r.TotalCapacity); err != nil {
		return r, fmt.Errorf("capacity check: %w", err)
	}

	// Ledger and item rows must agree.
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM items i WHERE i.state = 'confirmed'
		  AND NOT EXISTS (SELECT 1 FROM bookings b WHERE b.item_id = i.id AND b.status = 'confirmed')`).
		Scan(&r.ConfirmedItemsWithoutBooking); err != nil {
		return r, fmt.Errorf("confirmed-without-booking check: %w", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(DISTINCT b.item_id) FROM bookings b JOIN items i ON i.id = b.item_id
		WHERE b.status = 'confirmed' AND i.state <> 'confirmed'`).Scan(&r.BookingsOnUnconfirmedItems); err != nil {
		return r, fmt.Errorf("booking-on-unconfirmed check: %w", err)
	}

	// (c) every expired hold is takeable: free them in a transaction that is always rolled back,
	// using the same predicate the strategies use, and count any that refuse.
	stuck, err := stuckHolds(ctx, pool)
	if err != nil {
		return r, err
	}
	r.StuckHolds = stuck
	return r, nil
}

func stuckHolds(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx) // the probe never keeps its changes

	var expired []int64
	rows, err := tx.Query(ctx, `SELECT id FROM items WHERE state = 'held' AND hold_expires_at < clock_timestamp()`)
	if err != nil {
		return 0, fmt.Errorf("list expired holds: %w", err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		expired = append(expired, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(expired) == 0 {
		return 0, nil
	}
	tag, err := tx.Exec(ctx, `
		UPDATE items SET state = 'free', holder = NULL, hold_request_id = NULL,
		       hold_expires_at = NULL, payment_started_at = NULL
		WHERE id = ANY($1) AND (state = 'free' OR (state = 'held' AND hold_expires_at < clock_timestamp()))`, expired)
	if err != nil {
		return 0, fmt.Errorf("probe takeover: %w", err)
	}
	return len(expired) - int(tag.RowsAffected()), nil
}
