// Package fastpath is the fast-path strategy (D8): a Redis claim gate in front of a database
// strategy. The gate rejects most losers without touching the database; everything it lets through
// still goes to the inner strategy, and the database stays the final arbiter. If the gate is empty,
// stale or down, requests simply reach the database: slower, never wrong.
//
// A Redis "no" is the only decision the gate makes on its own, and the worst it can do is reject
// a buyer for an item that is actually free (a lost sale, counted and tested), never sell twice.
package fastpath

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"resourcereservation/internal/faults"
	"resourcereservation/internal/reservation"
	"resourcereservation/internal/reservation/rowsql"
)

// Config tunes the gate.
type Config struct {
	// PendingTTL is how long a fresh claim lives while the database reserve runs. Short on
	// purpose: a process that dies right after claiming must not block the item for a whole hold.
	PendingTTL time.Duration
}

// DefaultConfig matches the experiment parameters.
func DefaultConfig() Config { return Config{PendingTTL: 2 * time.Second} }

// Stats counts what the gate did; E2 and E4 report them.
type Stats struct {
	GatePassed   atomic.Int64 // claims that succeeded, so the request went on to the database
	GateRejected atomic.Int64 // requests answered "unavailable" by Redis alone
	GateErrors   atomic.Int64 // Redis errors: the request failed open to the database
	Learned      atomic.Int64 // keys written back after a database "unavailable"
}

// Store wraps an inner strategy with the gate.
type Store struct {
	inner reservation.Service
	gate  Gate
	pool  *pgxpool.Pool // only for the read that refills the gate after a database "unavailable"
	cfg   Config
	Stats Stats

	faults *faults.Registry // nil = no fault injection (D13)
}

// SetFaults hands the strategy the fault registry, and the inner strategy too (D13).
func (s *Store) SetFaults(f *faults.Registry) {
	s.faults = f
	if fa, ok := s.inner.(faults.Aware); ok {
		fa.SetFaults(f)
	}
}

// New returns a Store.
func New(inner reservation.Service, gate Gate, pool *pgxpool.Pool, cfg Config) *Store {
	if cfg.PendingTTL <= 0 {
		cfg.PendingTTL = DefaultConfig().PendingTTL
	}
	return &Store{inner: inner, gate: gate, pool: pool, cfg: cfg}
}

var _ reservation.Service = (*Store)(nil)

// bestEffort runs a gate call that must not fail the request: it outlives the request context
// (so a disconnect cannot skip cleanup) and its errors are only counted.
func (s *Store) bestEffort(ctx context.Context, f func(context.Context) error) {
	c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 250*time.Millisecond)
	defer cancel()
	if err := f(c); err != nil {
		s.Stats.GateErrors.Add(1)
	}
}

// Hold claims at the gate, then lets the inner strategy decide.
func (s *Store) Hold(ctx context.Context, req reservation.HoldRequest) (reservation.HoldResult, error) {
	ids := rowsql.SortedUnique(req.ItemIDs)
	value := claimValue(req.BuyerID, req.RequestID)

	claimed := false
	ok, err := s.gate.Claim(ctx, ids, value, s.cfg.PendingTTL)
	switch {
	case err != nil: // fail open (D8): a cache outage must not become an outage of the service
		s.Stats.GateErrors.Add(1)
	case !ok:
		s.Stats.GateRejected.Add(1)
		return reservation.HoldResult{Outcome: reservation.Unavailable}, nil
	default:
		s.Stats.GatePassed.Add(1)
		claimed = true
		if err := s.faults.Hit(ctx, "hold.after_gate_claim"); err != nil { // row 6: dying here leaks the claim
			return reservation.HoldResult{}, err
		}
	}

	res, err := s.inner.Hold(ctx, req)
	if err != nil || res.Outcome != reservation.Held {
		if claimed {
			s.bestEffort(ctx, func(c context.Context) error { return s.gate.Drop(c, ids, req.BuyerID) })
		}
		if err == nil && res.Outcome == reservation.Unavailable {
			s.learn(ctx, ids) // refill the gate from the database's answer, so the next loser is cheap
		}
		return res, err
	}

	if err := s.faults.Hit(ctx, "hold.before_gate_extend"); err != nil { // row 10: committed, claim not yet extended
		return reservation.HoldResult{}, err
	}
	// The database accepted: make the claim last as long as the hold. For a replayed hold only the
	// remaining time is left, so a retry cannot stretch the gate's view past the database's.
	ttl := req.TTL
	for _, it := range res.Items {
		if left := time.Until(it.ExpiresAt); left < ttl {
			ttl = left
		}
	}
	if ttl > 0 {
		s.bestEffort(ctx, func(c context.Context) error { return s.gate.Extend(c, ids, value, ttl) })
	}
	return res, nil
}

// learn reads the items the database refused and writes back who holds them, so a gate that was
// emptied (a restart) refills itself from real traffic without a rebuild job (D8).
func (s *Store) learn(ctx context.Context, ids []int64) {
	rows, err := rowsql.Read(ctx, s.pool, ids, rowsql.NoLock, 0)
	if err != nil {
		return
	}
	var entries []Entry
	for _, id := range ids {
		r := rows[id]
		switch {
		case r.State == "confirmed":
			entries = append(entries, Entry{ItemID: id, Value: Sold})
		case r.State == "held" && !r.Expired:
			// The key carries the real holder's value, so that holder's own retry still passes the gate.
			if ttl := time.Until(r.ExpiresAt); ttl > 0 {
				entries = append(entries, Entry{ItemID: id, Value: claimValue(r.Holder, r.RequestID), TTL: ttl})
			}
		}
	}
	if len(entries) == 0 {
		return
	}
	s.Stats.Learned.Add(int64(len(entries)))
	s.bestEffort(ctx, func(c context.Context) error { return s.gate.Learn(c, entries) })
}

// StartPayment is not on the fast path: the marker is a database fact (D5).
func (s *Store) StartPayment(ctx context.Context, ref reservation.HoldRef, timeout time.Duration) (reservation.Outcome, error) {
	return s.inner.StartPayment(ctx, ref, timeout)
}

// Confirm goes to the database; a confirmed item is then marked sold at the gate (best effort).
func (s *Store) Confirm(ctx context.Context, ref reservation.HoldRef) (reservation.ConfirmResult, error) {
	res, err := s.inner.Confirm(ctx, ref)
	if err == nil && res.Outcome == reservation.Confirmed {
		ids := rowsql.RefIDs(ref.Items)
		s.bestEffort(ctx, func(c context.Context) error {
			if err := s.faults.Hit(c, "gate.sold_marker"); err != nil { // row 18: the marker is lost, harmlessly
				return err
			}
			return s.gate.MarkSold(c, ids)
		})
	}
	return res, err
}

// Release goes to the database; if it freed anything, the buyer's claims are dropped at the gate.
func (s *Store) Release(ctx context.Context, ref reservation.HoldRef) (reservation.Outcome, error) {
	o, err := s.inner.Release(ctx, ref)
	if err == nil && o == reservation.Released {
		ids := rowsql.RefIDs(ref.Items)
		s.bestEffort(ctx, func(c context.Context) error { return s.gate.Drop(c, ids, ref.BuyerID) })
	}
	return o, err
}

// Availability reads the database.
func (s *Store) Availability(ctx context.Context, eventID int64) (int, error) {
	return s.inner.Availability(ctx, eventID)
}

// Counters exposes the gate's counters, and the inner strategy's if it has any.
func (s *Store) Counters() map[string]int64 {
	out := map[string]int64{
		"gate_passed": s.Stats.GatePassed.Load(), "gate_rejected": s.Stats.GateRejected.Load(),
		"gate_errors": s.Stats.GateErrors.Load(), "gate_learned": s.Stats.Learned.Load(),
	}
	if cp, ok := s.inner.(reservation.CounterProvider); ok {
		for k, v := range cp.Counters() {
			out["inner_"+k] = v
		}
	}
	return out
}
