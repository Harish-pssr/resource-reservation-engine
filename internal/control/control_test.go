// Package control proves the instrument works (negative control, D2). The same concurrent
// workload runs against {naive, locking} x {unprotected schema, protected schema}, and the
// independent checker reads the result:
//
//	naive   + unprotected  -> must show double sales   (the checker can detect the problem)
//	naive   + protected    -> must show none, and the unique index must have refused some
//	locking/optimistic + unprotected -> must show none (the strategy itself is safe)
//	locking/optimistic + protected   -> must show none
//
// A zero is only meaningful because the first cell is not zero.
package control_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"resourcereservation/internal/checker"
	"resourcereservation/internal/reservation"
	"resourcereservation/internal/reservation/locking"
	"resourcereservation/internal/reservation/naive"
	"resourcereservation/internal/reservation/optimistic"
	"resourcereservation/internal/testdb"
)

const (
	hotItems = 10
	buyers   = 400
	maxRound = 10 // contention is probabilistic: retry with a fresh database until the race shows
)

type tally struct{ held, confirmed, other int }

// race starts all buyers at once; each holds one random hot item and, if it won the hold, confirms.
func race(pool *pgxpool.Pool, svc reservation.Service, items []int64) tally {
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		t     tally
		start = make(chan struct{})
	)
	for i := 0; i < buyers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			item := items[rand.IntN(len(items))]
			buyer := fmt.Sprintf("buyer-%d", i)
			<-start
			h, err := svc.Hold(context.Background(), reservation.HoldRequest{
				BuyerID: buyer, RequestID: buyer, ItemIDs: []int64{item}, TTL: time.Minute})
			mu.Lock()
			defer mu.Unlock()
			if err != nil || h.Outcome != reservation.Held {
				t.other++
				return
			}
			t.held++
			mu.Unlock()
			c, err := svc.Confirm(context.Background(), reservation.HoldRef{BuyerID: buyer, Items: h.Refs()})
			mu.Lock()
			if err == nil && c.Outcome == reservation.Confirmed {
				t.confirmed++
			}
		}(i)
	}
	close(start)
	wg.Wait()
	return t
}

func newRun(t *testing.T, protected bool) (*pgxpool.Pool, []int64) {
	pool := testdb.New(t, protected)
	ctx := context.Background()
	var ev int64
	if err := pool.QueryRow(ctx, `INSERT INTO events (name, capacity) VALUES ('hot', $1) RETURNING id`, hotItems).Scan(&ev); err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, `INSERT INTO items (event_id, seq) SELECT $1, g FROM generate_series(1, $2) g RETURNING id`, ev, hotItems)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	return pool, ids
}

func report(t *testing.T, pool *pgxpool.Pool) checker.Report {
	r, err := checker.Check(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Negative control: the naive strategy on the unprotected schema must oversell.
func TestNaiveOnUnprotectedSchemaDoubleSells(t *testing.T) {
	for round := 1; round <= maxRound; round++ {
		pool, items := newRun(t, false)
		tl := race(pool, naive.New(pool), items)
		r := report(t, pool)
		t.Logf("round %d: held=%d confirmed=%d -> %v", round, tl.held, tl.confirmed, r)
		if r.DoubleSoldItems > 0 {
			return
		}
	}
	t.Fatalf("the naive strategy never double sold in %d rounds: the checker has not been shown to work", maxRound)
}

// The protected schema's unique index turns the same race into refusals, not double sales.
func TestNaiveOnProtectedSchemaIsStoppedByTheIndex(t *testing.T) {
	var refusals int64
	for round := 1; round <= maxRound; round++ {
		pool, items := newRun(t, true)
		store := naive.New(pool)
		tl := race(pool, store, items)
		r := report(t, pool)
		t.Logf("round %d: held=%d confirmed=%d unique_violations=%d -> %v", round, tl.held, tl.confirmed, store.UniqueViolations.Load(), r)
		if r.DoubleSoldItems != 0 || r.ExtraBookings != 0 || r.Oversold != 0 {
			t.Fatalf("the unique index must make a double sale impossible: %v", r)
		}
		refusals += store.UniqueViolations.Load()
		if refusals > 0 {
			return
		}
	}
	t.Fatalf("the index never had to refuse a booking in %d rounds: the control is not exercising the backstop", maxRound)
}

func TestProtectedStrategiesAreSafeOnBothSchemas(t *testing.T) {
	strategies := map[string]func(*pgxpool.Pool) reservation.Service{
		"locking":    func(p *pgxpool.Pool) reservation.Service { return locking.New(p) },
		"optimistic": func(p *pgxpool.Pool) reservation.Service { return optimistic.New(p) },
	}
	for name, build := range strategies {
		for _, protected := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/protected=%v", name, protected), func(t *testing.T) {
				for round := 1; round <= 3; round++ {
					pool, items := newRun(t, protected)
					tl := race(pool, build(pool), items)
					r := report(t, pool)
					t.Logf("round %d: held=%d confirmed=%d -> %v", round, tl.held, tl.confirmed, r)
					if !r.OK() {
						t.Fatalf("%s must never double sell: %v", name, r)
					}
					if tl.held != tl.confirmed || tl.confirmed > hotItems {
						t.Fatalf("every winner confirms and winners cannot exceed items: %+v", tl)
					}
				}
			})
		}
	}
}
