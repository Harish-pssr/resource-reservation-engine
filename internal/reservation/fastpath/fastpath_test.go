package fastpath_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"resourcereservation/internal/checker"
	"resourcereservation/internal/config"
	"resourcereservation/internal/db"
	"resourcereservation/internal/reservation"
	"resourcereservation/internal/reservation/fastpath"
	"resourcereservation/internal/reservation/locking"
	"resourcereservation/internal/reservation/naive"
	"resourcereservation/internal/reservation/reservationtest"
	"resourcereservation/internal/testdb"
)

func redisClient(t *testing.T) *redis.Client {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: config.FromEnv().RedisAddr, DialTimeout: 500 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("redis not reachable (run scripts/up first): %v", err)
	}
	t.Cleanup(func() { rdb.Close() })
	return rdb
}

// newStore builds locking-behind-the-gate with a private key prefix, so tests never share keys.
func newStore(t *testing.T, pool *pgxpool.Pool, rdb *redis.Client, cfg fastpath.Config) (*fastpath.Store, *fastpath.RedisGate) {
	t.Helper()
	gate := fastpath.NewRedisGate(rdb, fmt.Sprintf("t%08x:", rand.Uint32()))
	t.Cleanup(func() { gate.Wipe(context.Background()) })
	return fastpath.New(locking.New(pool), gate, pool, cfg), gate
}

// The same behaviour suite the other strategies pass.
func TestBehaviour(t *testing.T) {
	rdb := redisClient(t)
	reservationtest.Run(t, func(p *pgxpool.Pool) reservation.Service {
		s, _ := newStore(t, p, rdb, fastpath.DefaultConfig())
		return s
	})
}

type fx struct {
	t     *testing.T
	pool  *pgxpool.Pool
	store *fastpath.Store
	gate  *fastpath.RedisGate
	items []int64
}

func setup(t *testing.T, items int, cfg fastpath.Config) *fx {
	t.Helper()
	rdb := redisClient(t)
	pool := testdb.New(t, true)
	_, ids, err := db.CreateEvent(context.Background(), pool, "t", items)
	if err != nil {
		t.Fatal(err)
	}
	s, g := newStore(t, pool, rdb, cfg)
	return &fx{t: t, pool: pool, store: s, gate: g, items: ids}
}

func (f *fx) hold(buyer, req string, ids ...int64) reservation.HoldResult {
	f.t.Helper()
	r, err := f.store.Hold(context.Background(), reservation.HoldRequest{BuyerID: buyer, RequestID: req, ItemIDs: ids, TTL: time.Minute})
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

func (f *fx) key(id int64) string {
	f.t.Helper()
	v, err := f.gate.Get(context.Background(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return v
}

func (f *fx) dbVersion(id int64) (v int64) {
	f.pool.QueryRow(context.Background(), `SELECT hold_version FROM items WHERE id=$1`, id).Scan(&v)
	return v
}

// ---- gate-level (Lua) behaviour ----

func TestClaimIsAllOrNone(t *testing.T) {
	f := setup(t, 3, fastpath.DefaultConfig())
	ctx := context.Background()
	if ok, _ := f.gate.Claim(ctx, f.items[1:2], "a|r", time.Minute); !ok {
		t.Fatal("a free key must be claimable")
	}
	if ok, _ := f.gate.Claim(ctx, f.items, "b|r", time.Minute); ok {
		t.Fatal("a claim that includes a taken item must fail")
	}
	if f.key(f.items[0]) != "" || f.key(f.items[2]) != "" {
		t.Fatal("a failed claim must leave the other items unclaimed")
	}
	if ok, _ := f.gate.Claim(ctx, f.items[1:2], "a|r", time.Minute); !ok {
		t.Fatal("re-claiming with the same value (a retry) must succeed")
	}
}

func TestDropOnlyRemovesTheBuyersOwnClaims(t *testing.T) {
	f := setup(t, 2, fastpath.DefaultConfig())
	ctx := context.Background()
	f.gate.Claim(ctx, f.items[:1], "a|r1", time.Minute)
	f.gate.Claim(ctx, f.items[1:], "b|r2", time.Minute)
	f.gate.Drop(ctx, f.items, "a") // a's delayed cleanup covers both items
	if f.key(f.items[0]) != "" {
		t.Fatal("a's own claim must be dropped")
	}
	if f.key(f.items[1]) != "b|r2" {
		t.Fatalf("b's claim must survive a's cleanup, key is %q", f.key(f.items[1]))
	}
}

func TestExtendOnlyTouchesExactlyMatchingClaims(t *testing.T) {
	f := setup(t, 2, fastpath.DefaultConfig())
	ctx := context.Background()
	f.gate.Claim(ctx, f.items[:1], "a|r1", 100*time.Millisecond)
	f.gate.Claim(ctx, f.items[1:], "b|r2", 100*time.Millisecond)
	f.gate.Extend(ctx, f.items, "a|r1", time.Minute)
	time.Sleep(250 * time.Millisecond)
	if f.key(f.items[0]) != "a|r1" {
		t.Fatal("a's matching claim must have been extended")
	}
	if f.key(f.items[1]) != "" {
		t.Fatal("b's claim must not have been extended by a's call")
	}
}

// ---- strategy-level behaviour (D8) ----

func TestGateRejectsWithoutTouchingTheDatabase(t *testing.T) {
	f := setup(t, 1, fastpath.DefaultConfig())
	f.hold("a", "r1", f.items[0])
	if got := f.hold("b", "r2", f.items[0]); got.Outcome != reservation.Unavailable {
		t.Fatalf("outcome %q", got.Outcome)
	}
	if f.store.Stats.GateRejected.Load() != 1 {
		t.Fatalf("the loser must be rejected by Redis alone, rejected=%d", f.store.Stats.GateRejected.Load())
	}
	if v := f.dbVersion(f.items[0]); v != 1 {
		t.Fatalf("the database must not have been written by the rejected request, version %d", v)
	}
}

// Losing the whole gate is safe: the database still arbitrates, and the gate refills itself.
func TestEmptiedGateIsSafeAndRefillsFromTheDatabase(t *testing.T) {
	f := setup(t, 1, fastpath.DefaultConfig())
	f.hold("a", "ra", f.items[0])
	f.gate.Wipe(context.Background()) // what a restarted, non-persistent Redis looks like

	if got := f.hold("b", "rb", f.items[0]); got.Outcome != reservation.Unavailable {
		t.Fatalf("with an empty gate the database must still refuse b, got %q", got.Outcome)
	}
	if f.store.Stats.GateRejected.Load() != 0 {
		t.Fatal("the empty gate cannot have rejected anyone")
	}
	if f.key(f.items[0]) != "a|ra" {
		t.Fatalf("the gate must have learned the real holder, key is %q", f.key(f.items[0]))
	}
	if got := f.hold("c", "rc", f.items[0]); got.Outcome != reservation.Unavailable || f.store.Stats.GateRejected.Load() != 1 {
		t.Fatalf("after refilling, the next loser must be rejected at the gate: %q rejected=%d", got.Outcome, f.store.Stats.GateRejected.Load())
	}
	// The real holder's own retry still passes: the learned key carries its value.
	if got := f.hold("a", "ra", f.items[0]); got.Outcome != reservation.Held {
		t.Fatalf("a's retry of the same request must still succeed, got %q", got.Outcome)
	}
}

// A process that claimed and then died leaves a claim that must lapse on its own, quickly.
func TestLeakedClaimLapsesAfterThePendingTTL(t *testing.T) {
	f := setup(t, 1, fastpath.Config{PendingTTL: 150 * time.Millisecond})
	f.gate.Claim(context.Background(), f.items, "ghost|x", 150*time.Millisecond) // claimed, then the process died
	if got := f.hold("b", "rb", f.items[0]); got.Outcome != reservation.Unavailable {
		t.Fatalf("while the claim lives the gate refuses: %q", got.Outcome) // the (bounded) false-rejection window
	}
	time.Sleep(300 * time.Millisecond)
	if got := f.hold("b", "rb2", f.items[0]); got.Outcome != reservation.Held {
		t.Fatalf("the item must be bookable once the pending claim lapses: %q", got.Outcome)
	}
}

func TestFailsOpenWhenRedisIsDown(t *testing.T) {
	pool := testdb.New(t, true)
	_, ids, _ := db.CreateEvent(context.Background(), pool, "t", 1)
	dead := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 100 * time.Millisecond, MaxRetries: -1})
	defer dead.Close()
	s := fastpath.New(locking.New(pool), fastpath.NewRedisGate(dead, "x:"), pool, fastpath.DefaultConfig())
	r, err := s.Hold(context.Background(), reservation.HoldRequest{BuyerID: "a", RequestID: "r", ItemIDs: ids, TTL: time.Minute})
	if err != nil || r.Outcome != reservation.Held {
		t.Fatalf("a Redis outage must not stop bookings: %v %v", r.Outcome, err)
	}
	if s.Stats.GateErrors.Load() == 0 {
		t.Fatal("the failed-open calls must be counted")
	}
}

func TestConfirmMarksTheItemSoldAtTheGate(t *testing.T) {
	f := setup(t, 1, fastpath.DefaultConfig())
	a := f.hold("a", "ra", f.items[0])
	c, err := f.store.Confirm(context.Background(), reservation.HoldRef{BuyerID: "a", Items: a.Refs()})
	if err != nil || c.Outcome != reservation.Confirmed {
		t.Fatalf("%v %v", c, err)
	}
	if f.key(f.items[0]) != fastpath.Sold {
		t.Fatalf("key is %q, want sold", f.key(f.items[0]))
	}
	f.hold("b", "rb", f.items[0])
	if f.store.Stats.GateRejected.Load() != 1 {
		t.Fatal("a sold item must be rejected at the gate")
	}
}

// With the gate wiped repeatedly during a contended run, nothing is double sold. NOTE: this test
// alone is weak evidence. A mutation check showed it also passes with the unprotected naive strategy
// behind the gate, because the gate suppresses most races. The sensitive version is the blind-gate
// test below, which has a negative control.
func TestWipingTheGateDuringLoadNeverDoubleSells(t *testing.T) {
	const items, buyers = 10, 400
	pool := testdb.New(t, false)
	_, ids, _ := db.CreateEvent(context.Background(), pool, "hot", items)
	store, gate := newStore(t, pool, redisClient(t), fastpath.DefaultConfig())

	stop := make(chan struct{})
	var wipes int
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
				gate.Wipe(context.Background())
				wipes++
			}
		}
	}()

	var bw sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < buyers; i++ {
		bw.Add(1)
		go func(i int) {
			defer bw.Done()
			buyer := fmt.Sprintf("buyer-%d", i)
			<-start
			h, err := store.Hold(context.Background(), reservation.HoldRequest{
				BuyerID: buyer, RequestID: buyer, ItemIDs: []int64{ids[rand.IntN(items)]}, TTL: time.Minute})
			if err == nil && h.Outcome == reservation.Held {
				store.Confirm(context.Background(), reservation.HoldRef{BuyerID: buyer, Items: h.Refs()})
			}
		}(i)
	}
	close(start)
	bw.Wait()
	close(stop)
	wg.Wait()

	rep, err := checker.Check(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("wipes=%d passed=%d rejected=%d errors=%d -> %v", wipes, store.Stats.GatePassed.Load(),
		store.Stats.GateRejected.Load(), store.Stats.GateErrors.Load(), rep)
	if !rep.OK() {
		t.Fatalf("wiping the gate must not cause a double sale: %v", rep)
	}
}

// blindGate protects nothing: every claim succeeds. It is the worst case of losing the cache, and
// the way to prove the database, not the gate, is what prevents the double sale (D8).
type blindGate struct{}

func (blindGate) Claim(context.Context, []int64, string, time.Duration) (bool, error) {
	return true, nil
}
func (blindGate) Extend(context.Context, []int64, string, time.Duration) error { return nil }
func (blindGate) Drop(context.Context, []int64, string) error                  { return nil }
func (blindGate) MarkSold(context.Context, []int64) error                      { return nil }
func (blindGate) Learn(context.Context, []fastpath.Entry) error                { return nil }

func raceThroughBlindGate(t *testing.T, inner func(*pgxpool.Pool) reservation.Service) checker.Report {
	const items, buyers = 10, 400
	pool := testdb.New(t, false) // no unique index: only the strategy can protect
	_, ids, _ := db.CreateEvent(context.Background(), pool, "hot", items)
	store := fastpath.New(inner(pool), blindGate{}, pool, fastpath.DefaultConfig())
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < buyers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			buyer := fmt.Sprintf("buyer-%d", i)
			<-start
			h, err := store.Hold(context.Background(), reservation.HoldRequest{
				BuyerID: buyer, RequestID: buyer, ItemIDs: []int64{ids[rand.IntN(items)]}, TTL: time.Minute})
			if err == nil && h.Outcome == reservation.Held {
				store.Confirm(context.Background(), reservation.HoldRef{BuyerID: buyer, Items: h.Refs()})
			}
		}(i)
	}
	close(start)
	wg.Wait()
	rep, err := checker.Check(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// Negative control: a gate that protects nothing in front of the naive strategy must double sell,
// so the check below is able to fail.
func TestBlindGateInFrontOfNaiveDoubleSells(t *testing.T) {
	for round := 1; round <= 10; round++ {
		rep := raceThroughBlindGate(t, func(p *pgxpool.Pool) reservation.Service { return naive.New(p) })
		t.Logf("round %d: %v", round, rep)
		if rep.DoubleSoldItems > 0 {
			return
		}
	}
	t.Fatal("the control never double sold: the blind-gate test below would prove nothing")
}

// The claim of D8: with a gate that protects nothing, the database-backed strategy behind it still
// never double sells. The gate only saves work; it is never what makes the sale correct.
func TestBlindGateInFrontOfLockingNeverDoubleSells(t *testing.T) {
	for round := 1; round <= 3; round++ {
		rep := raceThroughBlindGate(t, func(p *pgxpool.Pool) reservation.Service { return locking.New(p) })
		t.Logf("round %d: %v", round, rep)
		if !rep.OK() {
			t.Fatalf("losing the gate must not cause a double sale: %v", rep)
		}
	}
}
