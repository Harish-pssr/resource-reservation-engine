// Package rowsql holds the item-row reading code shared by the strategies that use Postgres:
// one query that returns each item's state together with facts computed on the database
// clock (D3), and the small helpers around it.
package rowsql

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"resourcereservation/internal/reservation"
)

// Row is one item as read in a query.
type Row struct {
	ID        int64
	EventID   int64
	State     string
	Holder    string
	RequestID string
	Version   int64
	ExpiresAt time.Time
	Expired   bool      // held, and the hold time has passed on the database clock
	PayStart  time.Time // zero if no payment marker
	PayFresh  bool      // a marker exists and is younger than the timeout given to Read
}

// Takeable is the shared availability predicate (D3): free, or held and expired.
func (r Row) Takeable() bool { return r.State == "free" || (r.State == "held" && r.Expired) }

// OwnedBy reports whether the row is still the caller's hold: same buyer, same version.
func (r Row) OwnedBy(buyer string, version int64) bool {
	return r.Holder == buyer && r.Version == version
}

// Lock says how Read locks the rows.
type Lock int

const (
	NoLock Lock = iota // plain read (optimistic)
	Wait               // FOR UPDATE: queue behind other transactions
	NoWait             // FOR UPDATE NOWAIT: fail at once if a row is locked
)

// Querier is satisfied by both a pool and a transaction.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Read returns the items by id. With a lock it is a locking read; ORDER BY makes Postgres
// return, and so lock, the rows in id order: the global lock order that prevents deadlocks (D6).
// payTimeout decides what counts as a fresh payment marker.
func Read(ctx context.Context, q Querier, ids []int64, lock Lock, payTimeout time.Duration) (map[int64]Row, error) {
	sql := `SELECT id, event_id, state, COALESCE(holder, ''), COALESCE(hold_request_id, ''), hold_version,
	               COALESCE(hold_expires_at, 'epoch'),
	               state = 'held' AND hold_expires_at < clock_timestamp(),
	               COALESCE(payment_started_at, 'epoch'),
	               payment_started_at IS NOT NULL AND payment_started_at > clock_timestamp() - make_interval(secs => $2)
	        FROM items WHERE id = ANY($1) ORDER BY id`
	switch lock {
	case Wait:
		sql += " FOR UPDATE"
	case NoWait:
		sql += " FOR UPDATE NOWAIT"
	}
	rows, err := q.Query(ctx, sql, ids, payTimeout.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]Row, len(ids))
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.ID, &r.EventID, &r.State, &r.Holder, &r.RequestID, &r.Version,
			&r.ExpiresAt, &r.Expired, &r.PayStart, &r.PayFresh); err != nil {
			return nil, err
		}
		out[r.ID] = r
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) != len(ids) {
		return nil, fmt.Errorf("unknown item among %v", ids)
	}
	return out, nil
}

// SQLSTATEs that mean "another transaction got in the way; try again".
const (
	lockNotAvailable = "55P03" // NOWAIT found a locked row
	deadlockDetected = "40P01"
	serialization    = "40001"
)

// IsConflict reports whether err is a transient conflict between transactions.
func IsConflict(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == lockNotAvailable || pgErr.Code == deadlockDetected || pgErr.Code == serialization
}

// SortedUnique returns the ids ascending with duplicates removed.
func SortedUnique(ids []int64) []int64 {
	out := slices.Clone(ids)
	slices.Sort(out)
	return slices.Compact(out)
}

// RefIDs returns the sorted unique item ids of the references.
func RefIDs(refs []reservation.ItemRef) []int64 {
	ids := make([]int64, len(refs))
	for i, r := range refs {
		ids[i] = r.ItemID
	}
	return SortedUnique(ids)
}

// Versions maps item id to the hold version the caller presented.
func Versions(refs []reservation.ItemRef) map[int64]int64 {
	m := make(map[int64]int64, len(refs))
	for _, r := range refs {
		m[r.ItemID] = r.HoldVersion
	}
	return m
}

// Replay reports whether every id is already held by this buyer under this request id (D5).
func Replay(rows map[int64]Row, ids []int64, buyer, requestID string) bool {
	for _, id := range ids {
		r := rows[id]
		if !(r.State == "held" && r.Holder == buyer && r.RequestID == requestID) {
			return false
		}
	}
	return true
}

// ReplayResult builds the Held result for a replayed hold from the current rows.
func ReplayResult(rows map[int64]Row, ids []int64) reservation.HoldResult {
	res := reservation.HoldResult{Outcome: reservation.Held}
	for _, id := range ids {
		r := rows[id]
		res.Items = append(res.Items, reservation.HeldItem{ItemID: id, HoldVersion: r.Version, ExpiresAt: r.ExpiresAt})
	}
	return res
}
