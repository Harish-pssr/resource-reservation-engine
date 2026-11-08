// Package naive is the negative control (D2): check-then-act from the application with no row
// locks, no transaction, no state guard in its UPDATE and no ownership check on confirm.
// It exists so that the checker can be shown to detect a double sale. DO NOT use it for anything else.
//
// What exactly is missing, so claims about its overselling stay honest:
//   - Hold reads the rows, and if they look available writes with an UPDATE that does not
//     re-check the state, so two buyers can both read "free" and both be told Held.
//   - Confirm reads whether the item is already confirmed and, if not, inserts the booking and
//     marks the item, without a transaction, a lock or a check that the caller still holds it.
//   - There is no idempotency, no payment marker and no fencing check.
package naive

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"resourcereservation/internal/reservation"
)

// Store is the naive strategy.
type Store struct {
	pool *pgxpool.Pool
	// UniqueViolations counts confirms the database refused because the protected schema's
	// unique index caught a second booking (D2): the backstop working.
	UniqueViolations atomic.Int64
}

// New returns a Store on the given pool.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

var _ reservation.Service = (*Store)(nil)

// Hold: check (SELECT), then act (UPDATE), with nothing between them.
func (s *Store) Hold(ctx context.Context, req reservation.HoldRequest) (reservation.HoldResult, error) {
	ids := slices.Clone(req.ItemIDs)
	slices.Sort(ids)
	ids = slices.Compact(ids)

	var available int
	if err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM items
		WHERE id = ANY($1) AND (state = 'free' OR (state = 'held' AND hold_expires_at < clock_timestamp()))`,
		ids).Scan(&available); err != nil {
		return reservation.HoldResult{}, err
	}
	if available != len(ids) {
		return reservation.HoldResult{Outcome: reservation.Unavailable}, nil
	}
	// The gap between the SELECT above and this UPDATE is the race. The UPDATE has no
	// "WHERE state = free" guard, so it overwrites whatever another buyer wrote meanwhile.
	rows, err := s.pool.Query(ctx, `
		UPDATE items SET state = 'held', holder = $2, hold_request_id = $3,
		       hold_version = hold_version + 1,
		       hold_expires_at = clock_timestamp() + make_interval(secs => $4),
		       payment_started_at = NULL
		WHERE id = ANY($1)
		RETURNING id, hold_version, hold_expires_at`,
		ids, req.BuyerID, req.RequestID, req.TTL.Seconds())
	if err != nil {
		return reservation.HoldResult{}, err
	}
	defer rows.Close()
	res := reservation.HoldResult{Outcome: reservation.Held}
	for rows.Next() {
		var it reservation.HeldItem
		if err := rows.Scan(&it.ItemID, &it.HoldVersion, &it.ExpiresAt); err != nil {
			return reservation.HoldResult{}, err
		}
		res.Items = append(res.Items, it)
	}
	slices.SortFunc(res.Items, func(a, b reservation.HeldItem) int { return int(a.ItemID - b.ItemID) })
	return res, rows.Err()
}

// StartPayment does nothing: the naive strategy has no payment marker.
func (s *Store) StartPayment(context.Context, reservation.HoldRef, time.Duration) (reservation.Outcome, error) {
	return reservation.PaymentStarted, nil
}

// Confirm: check "already confirmed?" (SELECT), then act (INSERT booking, UPDATE item).
func (s *Store) Confirm(ctx context.Context, ref reservation.HoldRef) (reservation.ConfirmResult, error) {
	ids := make([]int64, len(ref.Items))
	for i, r := range ref.Items {
		ids[i] = r.ItemID
	}
	var already int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM items WHERE id = ANY($1) AND state = 'confirmed'`, ids).Scan(&already); err != nil {
		return reservation.ConfirmResult{}, err
	}
	if already > 0 {
		return reservation.ConfirmResult{Outcome: reservation.HoldLost}, nil
	}
	for _, r := range ref.Items {
		// The booking is written first so that, on the protected schema, the unique index
		// refuses a duplicate before anything else changes.
		_, err := s.pool.Exec(ctx, `
			INSERT INTO bookings (item_id, event_id, buyer_id, hold_version)
			SELECT id, event_id, $2, $3 FROM items WHERE id = $1`, r.ItemID, ref.BuyerID, r.HoldVersion)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" { // unique_violation
				s.UniqueViolations.Add(1)
				return reservation.ConfirmResult{Outcome: reservation.HoldLost}, nil
			}
			return reservation.ConfirmResult{}, err
		}
		if _, err := s.pool.Exec(ctx, `UPDATE items SET state = 'confirmed', holder = $2 WHERE id = $1`, r.ItemID, ref.BuyerID); err != nil {
			return reservation.ConfirmResult{}, err
		}
	}
	return reservation.ConfirmResult{Outcome: reservation.Confirmed}, nil
}

// Release frees the items held by the buyer (no version check).
func (s *Store) Release(ctx context.Context, ref reservation.HoldRef) (reservation.Outcome, error) {
	ids := make([]int64, len(ref.Items))
	for i, r := range ref.Items {
		ids[i] = r.ItemID
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE items SET state = 'free', holder = NULL, hold_request_id = NULL,
		       hold_expires_at = NULL, payment_started_at = NULL
		WHERE id = ANY($1) AND holder = $2 AND state = 'held'`, ids, ref.BuyerID)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		return reservation.NotHeld, nil
	}
	return reservation.Released, nil
}

// Availability counts items that are free or whose hold has expired.
func (s *Store) Availability(ctx context.Context, eventID int64) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM items
		WHERE event_id = $1 AND (state = 'free' OR (state = 'held' AND hold_expires_at < clock_timestamp()))`,
		eventID).Scan(&n)
	return n, err
}

// Counters exposes how many double sales the database's unique index refused.
func (s *Store) Counters() map[string]int64 {
	return map[string]int64{"unique_violations": s.UniqueViolations.Load()}
}
