// Package reservationtest is the behaviour suite every strategy must pass. It runs against a
// real Postgres. Each strategy's own test file calls Run with a constructor.
package reservationtest

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"reservationengine/internal/reservation"
	"reservationengine/internal/testdb"
)

// Factory builds a strategy on a pool using the protected schema.
type Factory func(pool *pgxpool.Pool) reservation.Service

const (
	longTTL  = time.Minute
	shortTTL = 200 * time.Millisecond // a real sleep past this expires the hold on the database clock
	pastTTL  = 350 * time.Millisecond
)

type env struct {
	t    *testing.T
	ctx  context.Context
	pool *pgxpool.Pool
	svc  reservation.Service
	reqN int
}

// Run executes every behaviour test against the strategy.
func Run(t *testing.T, newSvc Factory) {
	pool := testdb.New(t, true)
	e := &env{t: t, ctx: context.Background(), pool: pool, svc: newSvc(pool)}
	for _, tc := range []struct {
		name string
		fn   func(*env)
	}{
		{"hold a free item", holdFree},
		{"hold of a taken item is unavailable", holdTaken},
		{"multi-item hold is all or nothing", multiAllOrNothing},
		{"hold retry with the same request id is idempotent", holdRetryIdempotent},
		{"hold with a new request id on a held item is unavailable", holdNewRequestID},
		{"an expired hold can be taken over", expiredTakeover},
		{"availability counts expired holds as free", availabilityExpired},
		{"confirm in time books the item", confirmInTime},
		{"late confirm on an untouched item wins", lateConfirmWins},
		{"late confirm after takeover is rejected", lateConfirmAfterTakeover},
		{"late confirm after the new holder confirmed is rejected", lateConfirmAfterOtherConfirmed},
		{"confirm retry is idempotent", confirmRetry},
		{"confirm by another buyer is rejected", confirmWrongBuyer},
		{"multi-item confirm is all or nothing", confirmAllOrNothing},
		{"release frees the item", releaseFrees},
		{"release retry is a no-op", releaseRetry},
		{"stale release does not free another buyers hold", staleRelease},
		{"payment marker blocks a second attempt", paymentInProgress},
		{"payment marker goes stale after the timeout", paymentStale},
		{"payment start on a lost hold is rejected", paymentOnLostHold},
		{"concurrent holds: exactly one wins", concurrentHolds},
		{"confirm racing a takeover never double sells", confirmVsTakeover},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e.t = t
			tc.fn(e)
		})
	}
}

// ---- helpers ----

func (e *env) newEvent(n int) (eventID int64, items []int64) {
	e.t.Helper()
	must(e.t, e.pool.QueryRow(e.ctx, `INSERT INTO events (name, capacity) VALUES ('t', $1) RETURNING id`, n).Scan(&eventID))
	rows, err := e.pool.Query(e.ctx, `
		INSERT INTO items (event_id, seq) SELECT $1, g FROM generate_series(1, $2) g RETURNING id`, eventID, n)
	must(e.t, err)
	defer rows.Close()
	for rows.Next() {
		var id int64
		must(e.t, rows.Scan(&id))
		items = append(items, id)
	}
	must(e.t, rows.Err())
	return eventID, items
}

func (e *env) hold(buyer string, ttl time.Duration, items ...int64) reservation.HoldResult {
	e.t.Helper()
	e.reqN++
	return e.holdWithRequest(buyer, fmt.Sprintf("req-%d", e.reqN), ttl, items...)
}

func (e *env) holdWithRequest(buyer, reqID string, ttl time.Duration, items ...int64) reservation.HoldResult {
	e.t.Helper()
	r, err := e.svc.Hold(e.ctx, reservation.HoldRequest{BuyerID: buyer, RequestID: reqID, ItemIDs: items, TTL: ttl})
	must(e.t, err)
	return r
}

func (e *env) mustHold(buyer string, ttl time.Duration, items ...int64) reservation.HoldResult {
	e.t.Helper()
	r := e.hold(buyer, ttl, items...)
	if r.Outcome != reservation.Held {
		e.t.Fatalf("hold by %s of %v: outcome %q, want held", buyer, items, r.Outcome)
	}
	return r
}

func ref(buyer string, r reservation.HoldResult) reservation.HoldRef {
	return reservation.HoldRef{BuyerID: buyer, Items: r.Refs()}
}

func (e *env) confirm(r reservation.HoldRef) reservation.ConfirmResult {
	e.t.Helper()
	c, err := e.svc.Confirm(e.ctx, r)
	must(e.t, err)
	return c
}

func (e *env) release(r reservation.HoldRef) reservation.Outcome {
	e.t.Helper()
	o, err := e.svc.Release(e.ctx, r)
	must(e.t, err)
	return o
}

func (e *env) startPayment(r reservation.HoldRef, timeout time.Duration) reservation.Outcome {
	e.t.Helper()
	o, err := e.svc.StartPayment(e.ctx, r, timeout)
	must(e.t, err)
	return o
}

func (e *env) available(eventID int64) int {
	e.t.Helper()
	n, err := e.svc.Availability(e.ctx, eventID)
	must(e.t, err)
	return n
}

func (e *env) bookings(items ...int64) (n int) {
	e.t.Helper()
	must(e.t, e.pool.QueryRow(e.ctx, `SELECT count(*) FROM bookings WHERE item_id = ANY($1) AND status = 'confirmed'`, items).Scan(&n))
	return n
}

func (e *env) itemRow(id int64) (state, holder string, version int64) {
	e.t.Helper()
	var h *string
	must(e.t, e.pool.QueryRow(e.ctx, `SELECT state, holder, hold_version FROM items WHERE id = $1`, id).Scan(&state, &h, &version))
	if h != nil {
		holder = *h
	}
	return
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func want[T comparable](t *testing.T, what string, got, wantV T) {
	t.Helper()
	if got != wantV {
		t.Fatalf("%s: got %v, want %v", what, got, wantV)
	}
}

// ---- hold ----

func holdFree(e *env) {
	_, it := e.newEvent(1)
	r := e.hold("a", longTTL, it[0])
	want(e.t, "outcome", r.Outcome, reservation.Held)
	want(e.t, "hold_version", r.Items[0].HoldVersion, int64(1))
	if !r.Items[0].ExpiresAt.After(time.Now().Add(-time.Second)) {
		e.t.Fatalf("expires_at %v should be in the future", r.Items[0].ExpiresAt)
	}
	state, holder, _ := e.itemRow(it[0])
	want(e.t, "state", state, "held")
	want(e.t, "holder", holder, "a")
}

func holdTaken(e *env) {
	_, it := e.newEvent(1)
	e.mustHold("a", longTTL, it[0])
	want(e.t, "outcome", e.hold("b", longTTL, it[0]).Outcome, reservation.Unavailable)
	_, holder, _ := e.itemRow(it[0])
	want(e.t, "holder", holder, "a")
}

func multiAllOrNothing(e *env) {
	ev, it := e.newEvent(3)
	e.mustHold("a", longTTL, it[1])
	want(e.t, "outcome", e.hold("b", longTTL, it[0], it[1], it[2]).Outcome, reservation.Unavailable)
	// Nothing of b's request may remain: items 0 and 2 are still free.
	want(e.t, "available", e.available(ev), 2)
	for _, i := range []int{0, 2} {
		state, _, v := e.itemRow(it[i])
		want(e.t, "state", state, "free")
		want(e.t, "version", v, int64(0))
	}
}

func holdRetryIdempotent(e *env) {
	_, it := e.newEvent(2)
	first := e.holdWithRequest("a", "same-request", longTTL, it[0], it[1])
	want(e.t, "first outcome", first.Outcome, reservation.Held)
	second := e.holdWithRequest("a", "same-request", longTTL, it[0], it[1])
	want(e.t, "retry outcome", second.Outcome, reservation.Held)
	for i := range first.Items {
		want(e.t, "version unchanged", second.Items[i].HoldVersion, first.Items[i].HoldVersion)
		if !second.Items[i].ExpiresAt.Equal(first.Items[i].ExpiresAt) {
			e.t.Fatalf("a retry must not extend the hold: %v vs %v", first.Items[i].ExpiresAt, second.Items[i].ExpiresAt)
		}
	}
	_, _, v := e.itemRow(it[0])
	want(e.t, "db version", v, int64(1))
}

func holdNewRequestID(e *env) {
	_, it := e.newEvent(1)
	e.holdWithRequest("a", "r1", longTTL, it[0])
	want(e.t, "outcome", e.holdWithRequest("a", "r2", longTTL, it[0]).Outcome, reservation.Unavailable)
}

func expiredTakeover(e *env) {
	_, it := e.newEvent(1)
	a := e.mustHold("a", shortTTL, it[0])
	time.Sleep(pastTTL)
	b := e.hold("b", longTTL, it[0])
	want(e.t, "outcome", b.Outcome, reservation.Held)
	want(e.t, "version bumped", b.Items[0].HoldVersion, a.Items[0].HoldVersion+1)
	_, holder, _ := e.itemRow(it[0])
	want(e.t, "holder", holder, "b")
}

func availabilityExpired(e *env) {
	ev, it := e.newEvent(3)
	e.mustHold("a", longTTL, it[0])
	e.mustHold("b", shortTTL, it[1])
	want(e.t, "available before expiry", e.available(ev), 1) // only item 2 is free; 0 and 1 are held
	time.Sleep(pastTTL)
	want(e.t, "available after expiry", e.available(ev), 2) // item 1's stale hold counts as free
	e.mustHold("c", longTTL, it[1])
	want(e.t, "available after takeover", e.available(ev), 1)
}

// ---- confirm ----

func confirmInTime(e *env) {
	ev, it := e.newEvent(2)
	a := e.mustHold("a", longTTL, it[0], it[1])
	c := e.confirm(ref("a", a))
	want(e.t, "outcome", c.Outcome, reservation.Confirmed)
	want(e.t, "after expiry", c.AfterExpiry, false)
	want(e.t, "bookings", e.bookings(it...), 2)
	want(e.t, "available", e.available(ev), 0)
	state, _, _ := e.itemRow(it[0])
	want(e.t, "state", state, "confirmed")
}

func lateConfirmWins(e *env) {
	_, it := e.newEvent(1)
	a := e.mustHold("a", shortTTL, it[0])
	time.Sleep(pastTTL)
	c := e.confirm(ref("a", a))
	want(e.t, "outcome", c.Outcome, reservation.Confirmed)
	want(e.t, "after expiry", c.AfterExpiry, true)
	want(e.t, "bookings", e.bookings(it...), 1)
}

func lateConfirmAfterTakeover(e *env) {
	_, it := e.newEvent(1)
	a := e.mustHold("a", shortTTL, it[0])
	time.Sleep(pastTTL)
	e.mustHold("b", longTTL, it[0])
	want(e.t, "outcome", e.confirm(ref("a", a)).Outcome, reservation.HoldLost)
	_, holder, _ := e.itemRow(it[0])
	want(e.t, "holder is still b", holder, "b")
	want(e.t, "bookings", e.bookings(it...), 0)
}

func lateConfirmAfterOtherConfirmed(e *env) {
	_, it := e.newEvent(1)
	a := e.mustHold("a", shortTTL, it[0])
	time.Sleep(pastTTL)
	b := e.mustHold("b", longTTL, it[0])
	want(e.t, "b confirms", e.confirm(ref("b", b)).Outcome, reservation.Confirmed)
	want(e.t, "a's late confirm", e.confirm(ref("a", a)).Outcome, reservation.HoldLost)
	want(e.t, "bookings", e.bookings(it...), 1)
}

func confirmRetry(e *env) {
	_, it := e.newEvent(1)
	a := e.mustHold("a", longTTL, it[0])
	first := e.confirm(ref("a", a))
	want(e.t, "first", first.Outcome, reservation.Confirmed)
	want(e.t, "first replayed", first.Replayed, false)
	second := e.confirm(ref("a", a))
	want(e.t, "retry", second.Outcome, reservation.Confirmed)
	want(e.t, "retry replayed", second.Replayed, true)
	want(e.t, "bookings", e.bookings(it...), 1)
}

func confirmWrongBuyer(e *env) {
	_, it := e.newEvent(1)
	a := e.mustHold("a", longTTL, it[0])
	want(e.t, "outcome", e.confirm(ref("b", a)).Outcome, reservation.HoldLost)
	want(e.t, "bookings", e.bookings(it...), 0)
}

func confirmAllOrNothing(e *env) {
	_, it := e.newEvent(2)
	a0 := e.mustHold("a", longTTL, it[0])
	a1 := e.mustHold("a", shortTTL, it[1])
	time.Sleep(pastTTL)
	e.mustHold("b", longTTL, it[1]) // takes over a's expired hold on item 1
	both := reservation.HoldRef{BuyerID: "a", Items: append(a0.Refs(), a1.Refs()...)}
	want(e.t, "outcome", e.confirm(both).Outcome, reservation.HoldLost)
	state, holder, _ := e.itemRow(it[0])
	want(e.t, "item 0 stays held, not half-confirmed", state, "held")
	want(e.t, "item 0 holder", holder, "a")
	want(e.t, "bookings", e.bookings(it...), 0)
}

// ---- release ----

func releaseFrees(e *env) {
	ev, it := e.newEvent(1)
	a := e.mustHold("a", longTTL, it[0])
	want(e.t, "release", e.release(ref("a", a)), reservation.Released)
	want(e.t, "available", e.available(ev), 1)
	want(e.t, "b can hold", e.hold("b", longTTL, it[0]).Outcome, reservation.Held)
}

func releaseRetry(e *env) {
	_, it := e.newEvent(1)
	a := e.mustHold("a", longTTL, it[0])
	want(e.t, "first", e.release(ref("a", a)), reservation.Released)
	want(e.t, "retry", e.release(ref("a", a)), reservation.NotHeld)
}

func staleRelease(e *env) {
	_, it := e.newEvent(1)
	a := e.mustHold("a", shortTTL, it[0])
	time.Sleep(pastTTL)
	b := e.mustHold("b", longTTL, it[0])
	want(e.t, "a's stale release", e.release(ref("a", a)), reservation.NotHeld)
	_, holder, _ := e.itemRow(it[0])
	want(e.t, "b still holds", holder, "b")
	want(e.t, "b confirms", e.confirm(ref("b", b)).Outcome, reservation.Confirmed)
}

// ---- payment marker ----

func paymentInProgress(e *env) {
	_, it := e.newEvent(1)
	a := ref("a", e.mustHold("a", longTTL, it[0]))
	want(e.t, "first", e.startPayment(a, time.Minute), reservation.PaymentStarted)
	want(e.t, "second", e.startPayment(a, time.Minute), reservation.PaymentInProgress)
}

func paymentStale(e *env) {
	_, it := e.newEvent(1)
	a := ref("a", e.mustHold("a", longTTL, it[0]))
	want(e.t, "first", e.startPayment(a, shortTTL), reservation.PaymentStarted)
	time.Sleep(pastTTL)
	want(e.t, "after the timeout", e.startPayment(a, shortTTL), reservation.PaymentStarted)
}

func paymentOnLostHold(e *env) {
	_, it := e.newEvent(1)
	a := e.mustHold("a", shortTTL, it[0])
	time.Sleep(pastTTL)
	e.mustHold("b", longTTL, it[0])
	want(e.t, "outcome", e.startPayment(ref("a", a), time.Minute), reservation.HoldLost)
}

// ---- concurrency ----

func concurrentHolds(e *env) {
	_, it := e.newEvent(1)
	const buyers = 40
	results := make([]reservation.Outcome, buyers)
	var wg sync.WaitGroup
	for i := 0; i < buyers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r, err := e.svc.Hold(e.ctx, reservation.HoldRequest{
				BuyerID: fmt.Sprintf("buyer-%d", i), RequestID: fmt.Sprintf("c-%d", i), ItemIDs: []int64{it[0]}, TTL: longTTL})
			if err != nil {
				e.t.Errorf("buyer %d: %v", i, err)
				return
			}
			results[i] = r.Outcome
		}(i)
	}
	wg.Wait()
	winners := 0
	for i, o := range results {
		switch o {
		case reservation.Held:
			winners++
			_, holder, _ := e.itemRow(it[0])
			want(e.t, "holder is the winner", holder, fmt.Sprintf("buyer-%d", i))
		case reservation.Unavailable, reservation.LockBusy:
		default:
			e.t.Fatalf("buyer %d: unexpected outcome %q", i, o)
		}
	}
	want(e.t, "winners", winners, 1)
	_, _, v := e.itemRow(it[0])
	want(e.t, "hold_version", v, int64(1))
}

// confirmVsTakeover races A's late confirm against B's takeover of the same expired hold.
// Whoever's write lands first must win and the other must lose; never both, never a double sale.
func confirmVsTakeover(e *env) {
	for round := 0; round < 25; round++ {
		_, it := e.newEvent(1)
		a := e.mustHold("a", 50*time.Millisecond, it[0])
		time.Sleep(120 * time.Millisecond)

		var (
			wg   sync.WaitGroup
			conf reservation.ConfirmResult
			bh   reservation.HoldResult
			ec   error
			eh   error
		)
		start := make(chan struct{})
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			conf, ec = e.svc.Confirm(e.ctx, ref("a", a))
		}()
		go func() {
			defer wg.Done()
			<-start
			bh, eh = e.svc.Hold(e.ctx, reservation.HoldRequest{BuyerID: "b", RequestID: "b-req", ItemIDs: []int64{it[0]}, TTL: longTTL})
		}()
		close(start)
		wg.Wait()
		must(e.t, ec)
		must(e.t, eh)

		aWon := conf.Outcome == reservation.Confirmed
		bWon := bh.Outcome == reservation.Held
		if aWon && bWon {
			e.t.Fatalf("round %d: both the late confirm and the takeover succeeded", round)
		}
		if !aWon && !bWon && bh.Outcome != reservation.LockBusy {
			e.t.Fatalf("round %d: neither won (confirm %q, hold %q)", round, conf.Outcome, bh.Outcome)
		}
		n := e.bookings(it...)
		if aWon {
			want(e.t, "bookings when A won", n, 1)
		} else {
			want(e.t, "bookings when A lost", n, 0)
		}
	}
}
