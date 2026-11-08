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
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"resourcereservation/internal/reservation"
)

// Store implements reservation.Service with row locks.
type Store struct{ pool *pgxpool.Pool }

// New returns a Store on the given pool.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

var _ reservation.Service = (*Store)(nil)

// lockNotAvailable is the SQLSTATE Postgres raises when NOWAIT finds a row locked.
const lockNotAvailable = "55P03"

// row is one locked item as seen inside the transaction.
type row struct {
	id        int64
	eventID   int64
	state     string
	holder    string
	requestID string
	version   int64
	expiresAt time.Time
	expired   bool      // held, and the hold time has passed on the database clock
	payStart  time.Time // zero if no payment marker
	payFresh  bool      // marker exists and is younger than the timeout given to the query
}

// takeable is the shared availability predicate (D3): free, or held and expired.
func (r row) takeable() bool { return r.state == "free" || (r.state == "held" && r.expired) }

// lockRows locks the items in ascending id order. ORDER BY makes Postgres return, and so lock,
// the rows in id order: the global lock order that prevents deadlocks between multi-item requests.
func lockRows(ctx context.Context, tx pgx.Tx, ids []int64, noWait bool, payTimeout time.Duration) (map[int64]row, error) {
	q := `SELECT id, event_id, state, COALESCE(holder, ''), COALESCE(hold_request_id, ''), hold_version,
	             COALESCE(hold_expires_at, 'epoch'),
	             state = 'held' AND hold_expires_at < clock_timestamp(),
	             COALESCE(payment_started_at, 'epoch'),
	             payment_started_at IS NOT NULL AND payment_started_at > clock_timestamp() - make_interval(secs => $2)
	      FROM items WHERE id = ANY($1) ORDER BY id FOR UPDATE`
	if noWait {
		q += " NOWAIT"
	}
	rows, err := tx.Query(ctx, q, ids, payTimeout.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]row, len(ids))
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.eventID, &r.state, &r.holder, &r.requestID, &r.version,
			&r.expiresAt, &r.expired, &r.payStart, &r.payFresh); err != nil {
			return nil, err
		}
		out[r.id] = r
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) != len(ids) {
		return nil, fmt.Errorf("unknown item among %v", ids)
	}
	return out, nil
}

func isLockBusy(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == lockNotAvailable
}

// sortedUnique returns the ids ascending with duplicates removed.
func sortedUnique(ids []int64) []int64 {
	out := slices.Clone(ids)
	slices.Sort(out)
	return slices.Compact(out)
}

func refIDs(refs []reservation.ItemRef) []int64 {
	ids := make([]int64, len(refs))
	for i, r := range refs {
		ids[i] = r.ItemID
	}
	return sortedUnique(ids)
}

// Hold reserves all the items or none.
func (s *Store) Hold(ctx context.Context, req reservation.HoldRequest) (reservation.HoldResult, error) {
	ids := sortedUnique(req.ItemIDs)
	if len(ids) == 0 {
		return reservation.HoldResult{}, errors.New("hold needs at least one item")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return reservation.HoldResult{}, err
	}
	defer tx.Rollback(ctx)

	locked, err := lockRows(ctx, tx, ids, true, 0)
	if err != nil {
		if isLockBusy(err) {
			return reservation.HoldResult{Outcome: reservation.LockBusy}, nil
		}
		return reservation.HoldResult{}, err
	}

	// A retry of the same request (D5): every row is already this buyer's hold under this request id.
	replay := true
	for _, id := range ids {
		r := locked[id]
		if !(r.state == "held" && r.holder == req.BuyerID && r.requestID == req.RequestID) {
			replay = false
			break
		}
	}
	if replay {
		res := reservation.HoldResult{Outcome: reservation.Held}
		for _, id := range ids {
			r := locked[id]
			res.Items = append(res.Items, reservation.HeldItem{ItemID: id, HoldVersion: r.version, ExpiresAt: r.expiresAt})
		}
		return res, tx.Commit(ctx)
	}

	for _, id := range ids {
		if !locked[id].takeable() {
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

// ownsHold reports whether the row is still the caller's hold: same buyer, same version.
func ownsHold(r row, buyer string, version int64) bool {
	return r.holder == buyer && r.version == version
}

// StartPayment sets the in-progress marker before the payment step (D5).
func (s *Store) StartPayment(ctx context.Context, ref reservation.HoldRef, timeout time.Duration) (reservation.Outcome, error) {
	ids := refIDs(ref.Items)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	locked, err := lockRows(ctx, tx, ids, false, timeout)
	if err != nil {
		return "", err
	}
	want := versions(ref.Items)
	confirmed := 0
	for _, id := range ids {
		r := locked[id]
		switch {
		case !ownsHold(r, ref.BuyerID, want[id]):
			return reservation.HoldLost, nil
		case r.state == "confirmed":
			confirmed++
		}
	}
	if confirmed == len(ids) {
		return reservation.Confirmed, nil // already booked: nothing to pay for
	}
	for _, id := range ids {
		if locked[id].payFresh {
			return reservation.PaymentInProgress, nil
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE items SET payment_started_at = clock_timestamp() WHERE id = ANY($1)`, ids); err != nil {
		return "", err
	}
	return reservation.PaymentStarted, tx.Commit(ctx)
}

func versions(refs []reservation.ItemRef) map[int64]int64 {
	m := make(map[int64]int64, len(refs))
	for _, r := range refs {
		m[r.ItemID] = r.HoldVersion
	}
	return m
}

// Confirm books the items if every hold version still matches, even after expiry (D4).
func (s *Store) Confirm(ctx context.Context, ref reservation.HoldRef) (reservation.ConfirmResult, error) {
	ids := refIDs(ref.Items)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return reservation.ConfirmResult{}, err
	}
	defer tx.Rollback(ctx)
	locked, err := lockRows(ctx, tx, ids, false, 0)
	if err != nil {
		return reservation.ConfirmResult{}, err
	}
	want := versions(ref.Items)
	held, confirmed, late := 0, 0, false
	for _, id := range ids {
		r := locked[id]
		if !ownsHold(r, ref.BuyerID, want[id]) {
			return reservation.ConfirmResult{Outcome: reservation.HoldLost}, nil
		}
		switch r.state {
		case "held":
			held++
			late = late || r.expired
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
	ids := refIDs(ref.Items)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	locked, err := lockRows(ctx, tx, ids, false, 0)
	if err != nil {
		return "", err
	}
	want := versions(ref.Items)
	var mine []int64
	for _, id := range ids {
		if r := locked[id]; r.state == "held" && ownsHold(r, ref.BuyerID, want[id]) {
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
