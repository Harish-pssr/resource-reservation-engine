// Package locking is the pessimistic strategy (D6): every operation locks the item rows it
// touches, in ascending id order, inside one short transaction, then decides and writes.
//
// Hold uses FOR UPDATE NOWAIT: if another transaction is mid-way through reserving a row the
// caller gets LockBusy instead of queueing. Confirm, release and payment start wait for the
// lock instead; they are rare and a paid buyer should not lose a confirm to a millisecond lock.
//
// Time always comes from the database (clock_timestamp), never from the application (D3).
package locking

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"resourcereservation/internal/reservation"
	"resourcereservation/internal/reservation/rowsql"
)

// Store implements reservation.Service with row locks.
type Store struct {
	pool *pgxpool.Pool
	// LockBusy counts holds refused because a row lock was taken (the NOWAIT outcome).
	LockBusy atomic.Int64
}

// New returns a Store on the given pool.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

var _ reservation.Service = (*Store)(nil)

// Hold reserves all the items or none.
func (s *Store) Hold(ctx context.Context, req reservation.HoldRequest) (reservation.HoldResult, error) {
	ids := rowsql.SortedUnique(req.ItemIDs)
	if len(ids) == 0 {
		return reservation.HoldResult{}, errors.New("hold needs at least one item")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return reservation.HoldResult{}, err
	}
	defer tx.Rollback(ctx)

	locked, err := rowsql.Read(ctx, tx, ids, rowsql.NoWait, 0)
	if err != nil {
		if rowsql.IsConflict(err) {
			s.LockBusy.Add(1)
			return reservation.HoldResult{Outcome: reservation.LockBusy}, nil
		}
		return reservation.HoldResult{}, err
	}

	// A retry of the same request (D5): every row is already this buyer's hold under this request id.
	if rowsql.Replay(locked, ids, req.BuyerID, req.RequestID) {
		return rowsql.ReplayResult(locked, ids), tx.Commit(ctx)
	}
	for _, id := range ids {
		if !locked[id].Takeable() {
			return reservation.HoldResult{Outcome: reservation.Unavailable}, nil
		}
	}

	// Taking over an expired hold and holding a free item are the same write: the version
	// bump is what makes the previous holder's late calls recognisable (D3, D4).
	rows, err := tx.Query(ctx, `
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
	res := reservation.HoldResult{Outcome: reservation.Held}
	for rows.Next() {
		var it reservation.HeldItem
		if err := rows.Scan(&it.ItemID, &it.HoldVersion, &it.ExpiresAt); err != nil {
			rows.Close()
			return reservation.HoldResult{}, err
		}
		res.Items = append(res.Items, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return reservation.HoldResult{}, err
	}
	slices.SortFunc(res.Items, func(a, b reservation.HeldItem) int { return int(a.ItemID - b.ItemID) })
	return res, tx.Commit(ctx)
}

// StartPayment sets the in-progress marker before the payment step (D5).
func (s *Store) StartPayment(ctx context.Context, ref reservation.HoldRef, timeout time.Duration) (reservation.Outcome, error) {
	ids := rowsql.RefIDs(ref.Items)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	locked, err := rowsql.Read(ctx, tx, ids, rowsql.Wait, timeout)
	if err != nil {
		return "", err
	}
	want := rowsql.Versions(ref.Items)
	confirmed := 0
	for _, id := range ids {
		r := locked[id]
		switch {
		case !r.OwnedBy(ref.BuyerID, want[id]):
			return reservation.HoldLost, nil
		case r.State == "confirmed":
			confirmed++
		}
	}
	if confirmed == len(ids) {
		return reservation.Confirmed, nil // already booked: nothing to pay for
	}
	for _, id := range ids {
		if locked[id].PayFresh {
			return reservation.PaymentInProgress, nil
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE items SET payment_started_at = clock_timestamp() WHERE id = ANY($1)`, ids); err != nil {
		return "", err
	}
	return reservation.PaymentStarted, tx.Commit(ctx)
}

// Confirm books the items if every hold version still matches, even after expiry (D4).
func (s *Store) Confirm(ctx context.Context, ref reservation.HoldRef) (reservation.ConfirmResult, error) {
	ids := rowsql.RefIDs(ref.Items)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return reservation.ConfirmResult{}, err
	}
	defer tx.Rollback(ctx)
	locked, err := rowsql.Read(ctx, tx, ids, rowsql.Wait, 0)
	if err != nil {
		return reservation.ConfirmResult{}, err
	}
	want := rowsql.Versions(ref.Items)
	held, confirmed, late := 0, 0, false
	for _, id := range ids {
		r := locked[id]
		if !r.OwnedBy(ref.BuyerID, want[id]) {
			return reservation.ConfirmResult{Outcome: reservation.HoldLost}, nil
		}
		switch r.State {
		case "held":
			held++
			late = late || r.Expired
		case "confirmed":
			confirmed++
		}
	}
	if confirmed == len(ids) { // a retry of a confirm that already succeeded (D5)
		return reservation.ConfirmResult{Outcome: reservation.Confirmed, Replayed: true}, nil
	}
	if held != len(ids) {
		return reservation.ConfirmResult{Outcome: reservation.HoldLost}, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE items SET state = 'confirmed', payment_started_at = NULL WHERE id = ANY($1)`, ids); err != nil {
		return reservation.ConfirmResult{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO bookings (item_id, event_id, buyer_id, hold_version)
		SELECT id, event_id, $2, hold_version FROM items WHERE id = ANY($1)`, ids, ref.BuyerID); err != nil {
		return reservation.ConfirmResult{}, err
	}
	return reservation.ConfirmResult{Outcome: reservation.Confirmed, AfterExpiry: late}, tx.Commit(ctx)
}

// Release frees the items that are still the caller's hold; anything else is left alone (D4, D5).
func (s *Store) Release(ctx context.Context, ref reservation.HoldRef) (reservation.Outcome, error) {
	ids := rowsql.RefIDs(ref.Items)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	locked, err := rowsql.Read(ctx, tx, ids, rowsql.Wait, 0)
	if err != nil {
		return "", err
	}
	want := rowsql.Versions(ref.Items)
	var mine []int64
	for _, id := range ids {
		if r := locked[id]; r.State == "held" && r.OwnedBy(ref.BuyerID, want[id]) {
			mine = append(mine, id)
		}
	}
	if len(mine) == 0 {
		return reservation.NotHeld, nil
	}
	if _, err := tx.Exec(ctx, `
		UPDATE items SET state = 'free', holder = NULL, hold_request_id = NULL,
		       hold_expires_at = NULL, payment_started_at = NULL
		WHERE id = ANY($1)`, mine); err != nil {
		return "", err
	}
	return reservation.Released, tx.Commit(ctx)
}

// Availability counts items that are free or whose hold has expired (D3).
func (s *Store) Availability(ctx context.Context, eventID int64) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT count(*) FROM items
		WHERE event_id = $1 AND (state = 'free' OR (state = 'held' AND hold_expires_at < clock_timestamp()))`,
		eventID).Scan(&n)
	return n, err
}

// Counters exposes the strategy's counters for /metrics.
func (s *Store) Counters() map[string]int64 { return map[string]int64{"lock_busy": s.LockBusy.Load()} }
