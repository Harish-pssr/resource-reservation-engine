// Package optimistic is the optimistic-concurrency strategy (D7). It reads the rows without
// locking them, decides, then writes with a condition ("only if the row is still as I saw it").
// If a conditional write changes no row, someone got in between: the transaction is rolled back,
// the rows are re-read ONCE to classify the loss, and the answer goes back to the caller:
//
//	the item is now held or confirmed  -> Unavailable (final)
//	the item is still available        -> LockBusy    (retry shortly)
//
// The server never retries by itself; the retry bound lives in the client policy (D7).
//
// Multi-item requests are made atomic by one short transaction in which the rows are updated one
// at a time in ascending id order. That is the same order the locking strategy uses, so two
// requests cannot deadlock each other. A row update waits if another transaction is writing the
// same row, then re-checks its condition: this is where a lost race shows up as zero rows.
package optimistic

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"resourcereservation/internal/faults"
	"resourcereservation/internal/reservation"
	"resourcereservation/internal/reservation/rowsql"
)

// Store implements reservation.Service with conditional writes.
type Store struct {
	pool *pgxpool.Pool

	// Counters for E2: how much wasted work optimistic concurrency does under contention (D7).
	Conflicts atomic.Int64 // conditional writes that changed no row (the transaction was rolled back)
	Rereads   atomic.Int64 // re-reads done to classify a conflict

	faults *faults.Registry // nil = no fault injection (D13)

	// afterRead runs between the read and the write. Tests use it to simulate another buyer
	// getting in between; it is nil in production.
	afterRead func()
}

// SetFaults hands the strategy the fault registry (D13).
func (s *Store) SetFaults(f *faults.Registry) { s.faults = f }

// New returns a Store on the given pool.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

var _ reservation.Service = (*Store)(nil)

// Hold reserves all the items or none.
func (s *Store) Hold(ctx context.Context, req reservation.HoldRequest) (reservation.HoldResult, error) {
	ids := rowsql.SortedUnique(req.ItemIDs)
	if len(ids) == 0 {
		return reservation.HoldResult{}, errors.New("hold needs at least one item")
	}
	seen, err := rowsql.Read(ctx, s.pool, ids, rowsql.NoLock, 0)
	if err != nil {
		return reservation.HoldResult{}, err
	}
	if rowsql.Replay(seen, ids, req.BuyerID, req.RequestID) {
		return rowsql.ReplayResult(seen, ids), nil
	}
	for _, id := range ids {
		if !seen[id].Takeable() {
			return reservation.HoldResult{Outcome: reservation.Unavailable}, nil
		}
	}
	if s.afterRead != nil {
		s.afterRead()
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return reservation.HoldResult{}, err
	}
	defer tx.Rollback(ctx)
	res := reservation.HoldResult{Outcome: reservation.Held}
	for _, id := range ids { // ascending: no deadlock between requests
		var it reservation.HeldItem
		err := tx.QueryRow(ctx, `
			UPDATE items SET state = 'held', holder = $2, hold_request_id = $3,
			       hold_version = hold_version + 1,
			       hold_expires_at = clock_timestamp() + make_interval(secs => $4),
			       payment_started_at = NULL
			WHERE id = $1 AND hold_version = $5
			  AND (state = 'free' OR (state = 'held' AND hold_expires_at < clock_timestamp()))
			RETURNING id, hold_version, hold_expires_at`,
			id, req.BuyerID, req.RequestID, req.TTL.Seconds(), seen[id].Version).
			Scan(&it.ItemID, &it.HoldVersion, &it.ExpiresAt)
		if errors.Is(err, pgx.ErrNoRows) {
			tx.Rollback(ctx)
			return s.classifyHoldLoss(ctx, ids, req)
		}
		if err != nil {
			if rowsql.IsConflict(err) {
				return reservation.HoldResult{Outcome: reservation.LockBusy}, nil
			}
			return reservation.HoldResult{}, err
		}
		res.Items = append(res.Items, it)
	}
	if err := s.faults.Hit(ctx, "hold.before_commit"); err != nil { // row 8
		return reservation.HoldResult{}, err
	}
	return res, tx.Commit(ctx)
}

// classifyHoldLoss is the single re-read after a lost race (D7).
func (s *Store) classifyHoldLoss(ctx context.Context, ids []int64, req reservation.HoldRequest) (reservation.HoldResult, error) {
	s.Conflicts.Add(1)
	s.Rereads.Add(1)
	now, err := rowsql.Read(ctx, s.pool, ids, rowsql.NoLock, 0)
	if err != nil {
		return reservation.HoldResult{}, err
	}
	// The loser may be a duplicate of a request that just won (a retry racing the original).
	if rowsql.Replay(now, ids, req.BuyerID, req.RequestID) {
		return rowsql.ReplayResult(now, ids), nil
	}
	for _, id := range ids {
		if !now[id].Takeable() {
			return reservation.HoldResult{Outcome: reservation.Unavailable}, nil
		}
	}
	return reservation.HoldResult{Outcome: reservation.LockBusy}, nil
}

// StartPayment sets the in-progress marker before the payment step (D5).
func (s *Store) StartPayment(ctx context.Context, ref reservation.HoldRef, timeout time.Duration) (reservation.Outcome, error) {
	ids := rowsql.RefIDs(ref.Items)
	want := rowsql.Versions(ref.Items)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	for _, id := range ids {
		tag, err := tx.Exec(ctx, `
			UPDATE items SET payment_started_at = clock_timestamp()
			WHERE id = $1 AND holder = $2 AND hold_version = $3 AND state = 'held'
			  AND (payment_started_at IS NULL
			       OR payment_started_at <= clock_timestamp() - make_interval(secs => $4))`,
			id, ref.BuyerID, want[id], timeout.Seconds())
		if err != nil {
			return "", err
		}
		if tag.RowsAffected() == 0 {
			tx.Rollback(ctx)
			return s.classifyPaymentLoss(ctx, ids, ref, want, timeout)
		}
	}
	return reservation.PaymentStarted, tx.Commit(ctx)
}

func (s *Store) classifyPaymentLoss(ctx context.Context, ids []int64, ref reservation.HoldRef, want map[int64]int64, timeout time.Duration) (reservation.Outcome, error) {
	s.Conflicts.Add(1)
	s.Rereads.Add(1)
	now, err := rowsql.Read(ctx, s.pool, ids, rowsql.NoLock, timeout)
	if err != nil {
		return "", err
	}
	confirmed := 0
	for _, id := range ids {
		r := now[id]
		if !r.OwnedBy(ref.BuyerID, want[id]) {
			return reservation.HoldLost, nil
		}
		if r.State == "confirmed" {
			confirmed++
		}
	}
	if confirmed == len(ids) {
		return reservation.Confirmed, nil
	}
	return reservation.PaymentInProgress, nil
}

// Confirm books the items if every hold version still matches, even after expiry (D4).
func (s *Store) Confirm(ctx context.Context, ref reservation.HoldRef) (reservation.ConfirmResult, error) {
	ids := rowsql.RefIDs(ref.Items)
	want := rowsql.Versions(ref.Items)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return reservation.ConfirmResult{}, err
	}
	defer tx.Rollback(ctx)
	late := false
	for _, id := range ids {
		var expired bool
		err := tx.QueryRow(ctx, `
			UPDATE items SET state = 'confirmed', payment_started_at = NULL
			WHERE id = $1 AND holder = $2 AND hold_version = $3 AND state = 'held'
			RETURNING hold_expires_at < clock_timestamp()`,
			id, ref.BuyerID, want[id]).Scan(&expired)
		if errors.Is(err, pgx.ErrNoRows) {
			tx.Rollback(ctx)
			return s.classifyConfirmLoss(ctx, ids, ref, want)
		}
		if err != nil {
			return reservation.ConfirmResult{}, err
		}
		late = late || expired
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO bookings (item_id, event_id, buyer_id, hold_version)
		SELECT id, event_id, $2, hold_version FROM items WHERE id = ANY($1)`, ids, ref.BuyerID); err != nil {
		return reservation.ConfirmResult{}, err
	}
	if err := s.faults.Hit(ctx, "confirm.before_commit"); err != nil { // row 16
		return reservation.ConfirmResult{}, err
	}
	return reservation.ConfirmResult{Outcome: reservation.Confirmed, AfterExpiry: late}, tx.Commit(ctx)
}

func (s *Store) classifyConfirmLoss(ctx context.Context, ids []int64, ref reservation.HoldRef, want map[int64]int64) (reservation.ConfirmResult, error) {
	s.Conflicts.Add(1)
	s.Rereads.Add(1)
	now, err := rowsql.Read(ctx, s.pool, ids, rowsql.NoLock, 0)
	if err != nil {
		return reservation.ConfirmResult{}, err
	}
	confirmed := 0
	for _, id := range ids {
		r := now[id]
		if !r.OwnedBy(ref.BuyerID, want[id]) {
			return reservation.ConfirmResult{Outcome: reservation.HoldLost}, nil
		}
		if r.State == "confirmed" {
			confirmed++
		}
	}
	if confirmed == len(ids) { // a retry of a confirm that already succeeded (D5)
		return reservation.ConfirmResult{Outcome: reservation.Confirmed, Replayed: true}, nil
	}
	return reservation.ConfirmResult{Outcome: reservation.HoldLost}, nil
}

// Release frees the items that are still the caller's hold; anything else is left alone (D4, D5).
func (s *Store) Release(ctx context.Context, ref reservation.HoldRef) (reservation.Outcome, error) {
	ids := rowsql.RefIDs(ref.Items)
	want := rowsql.Versions(ref.Items)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	released := 0
	for _, id := range ids { // ascending
		tag, err := tx.Exec(ctx, `
			UPDATE items SET state = 'free', holder = NULL, hold_request_id = NULL,
			       hold_expires_at = NULL, payment_started_at = NULL
			WHERE id = $1 AND holder = $2 AND hold_version = $3 AND state = 'held'`,
			id, ref.BuyerID, want[id])
		if err != nil {
			return "", err
		}
		released += int(tag.RowsAffected())
	}
	if released == 0 {
		return reservation.NotHeld, nil
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

// Counters exposes the wasted-work counters for /metrics.
func (s *Store) Counters() map[string]int64 {
	return map[string]int64{"conflicts": s.Conflicts.Load(), "rereads": s.Rereads.Load()}
}
