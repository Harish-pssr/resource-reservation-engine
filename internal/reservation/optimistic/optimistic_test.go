package optimistic

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"resourcereservation/internal/db"
	"resourcereservation/internal/reservation"
	"resourcereservation/internal/reservation/reservationtest"
	"resourcereservation/internal/testdb"
)

// The same behaviour suite the locking strategy passes.
func TestBehaviour(t *testing.T) {
	reservationtest.Run(t, func(p *pgxpool.Pool) reservation.Service { return New(p) })
}

func setup(t *testing.T, items int) (*Store, *pgxpool.Pool, []int64) {
	t.Helper()
	pool := testdb.New(t, true)
	_, ids, err := db.CreateEvent(context.Background(), pool, "t", items)
	if err != nil {
		t.Fatal(err)
	}
	return New(pool), pool, ids
}

func hold(s *Store, buyer string, ids ...int64) reservation.HoldResult {
	r, err := s.Hold(context.Background(), reservation.HoldRequest{BuyerID: buyer, RequestID: buyer, ItemIDs: ids, TTL: time.Minute})
	if err != nil {
		panic(err)
	}
	return r
}

// A conflict that leaves the item taken is final: Unavailable, not "retry" (D7).
func TestLostRaceThatTakesTheItemIsUnavailable(t *testing.T) {
	s, pool, ids := setup(t, 1)
	s.afterRead = func() { // another buyer gets in between this buyer's read and write
		if _, err := pool.Exec(context.Background(), `
			UPDATE items SET state='held', holder='rival', hold_version=hold_version+1,
			       hold_expires_at = clock_timestamp() + interval '1 minute' WHERE id=$1`, ids[0]); err != nil {
			t.Error(err)
		}
	}
	got := hold(s, "me", ids[0])
	if got.Outcome != reservation.Unavailable {
		t.Fatalf("outcome %q, want unavailable", got.Outcome)
	}
	if s.Conflicts.Load() != 1 || s.Rereads.Load() != 1 {
		t.Fatalf("one conflict and one re-read expected, got %d and %d", s.Conflicts.Load(), s.Rereads.Load())
	}
}

// A conflict that leaves the item available (a spurious version change) is transient: LockBusy.
func TestSpuriousConflictIsLockBusy(t *testing.T) {
	s, pool, ids := setup(t, 1)
	s.afterRead = func() { // the version changes but the item stays free
		if _, err := pool.Exec(context.Background(), `UPDATE items SET hold_version = hold_version + 1 WHERE id=$1`, ids[0]); err != nil {
			t.Error(err)
		}
	}
	got := hold(s, "me", ids[0])
	if got.Outcome != reservation.LockBusy {
		t.Fatalf("outcome %q, want lock_busy", got.Outcome)
	}
	// And the failed attempt left nothing behind.
	var state string
	pool.QueryRow(context.Background(), `SELECT state FROM items WHERE id=$1`, ids[0]).Scan(&state)
	if state != "free" {
		t.Fatalf("a rolled-back attempt must leave the item free, got %q", state)
	}
}

// A multi-item request that loses on its last item must roll back the items it already wrote.
func TestLostRaceOnLaterItemRollsBackEarlierOnes(t *testing.T) {
	s, pool, ids := setup(t, 3)
	s.afterRead = func() {
		pool.Exec(context.Background(), `
			UPDATE items SET state='held', holder='rival', hold_version=hold_version+1,
			       hold_expires_at = clock_timestamp() + interval '1 minute' WHERE id=$1`, ids[2])
	}
	if got := hold(s, "me", ids...); got.Outcome != reservation.Unavailable {
		t.Fatalf("outcome %q, want unavailable", got.Outcome)
	}
	var free int
	pool.QueryRow(context.Background(), `SELECT count(*) FROM items WHERE id = ANY($1) AND state='free'`, ids[:2]).Scan(&free)
	if free != 2 {
		t.Fatalf("items 0 and 1 must be free again after the rollback, %d are", free)
	}
}

// Real conflicts: two buyers start together for the same item, many times. Exactly one wins each
// round, the loser is told Unavailable (the item is gone, not "retry"), and the conditional
// write really did lose races sometimes (so the conflict path was exercised, not just the pre-check).
func TestRealRacesResolveToOneWinner(t *testing.T) {
	const rounds = 150
	s, _, ids := setup(t, rounds)
	for r := 0; r < rounds; r++ {
		var wg sync.WaitGroup
		out := make([]reservation.Outcome, 2)
		start := make(chan struct{})
		for b := 0; b < 2; b++ {
			wg.Add(1)
			go func(b int) {
				defer wg.Done()
				<-start
				out[b] = hold(s, fmt.Sprintf("buyer-%d", b), ids[r]).Outcome
			}(b)
		}
		close(start)
		wg.Wait()
		held := 0
		for _, o := range out {
			switch o {
			case reservation.Held:
				held++
			case reservation.Unavailable:
			default:
				t.Fatalf("round %d: unexpected outcome %q (the loser must be told the item is gone)", r, o)
			}
		}
		if held != 1 {
			t.Fatalf("round %d: %d winners, want exactly 1 (%v)", r, held, out)
		}
	}
	if s.Conflicts.Load() == 0 {
		t.Fatal("no conditional write ever lost a race in 150 rounds: the conflict path was not exercised")
	}
	t.Logf("conflicts (lost conditional writes) in %d rounds: %d", rounds, s.Conflicts.Load())
}
