package waitingroom

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"resourcereservation/internal/config"
)

func newRoom(t *testing.T, cfg Config) *Room {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: config.FromEnv().RedisAddr, DialTimeout: 500 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("redis not reachable (run scripts/up first): %v", err)
	}
	r := New(rdb, fmt.Sprintf("w%08x:", rand.Uint32()), cfg)
	t.Cleanup(func() { r.Wipe(context.Background()); rdb.Close() })
	return r
}

func join(t *testing.T, r *Room, buyer string) Status {
	t.Helper()
	st, err := r.Join(context.Background(), buyer)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func poll(t *testing.T, r *Room, buyer string) Status {
	t.Helper()
	st, err := r.Poll(context.Background(), buyer)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func admit(t *testing.T, r *Room) int {
	t.Helper()
	n, err := r.Admit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestJoinQueuesInOrderWithPositions(t *testing.T) {
	r := newRoom(t, Config{Rate: 100, Burst: 2}) // a burst of 2: only two buyers can be admitted at once
	var last int64
	for i := 1; i <= 5; i++ {
		st := join(t, r, fmt.Sprintf("b%d", i))
		if st.State != Queued || st.Position != i || st.Ticket <= last {
			t.Fatalf("buyer %d: %+v (previous ticket %d)", i, st, last)
		}
		last = st.Ticket
	}
	if n := admit(t, r); n != 2 {
		t.Fatalf("admitted %d, want exactly the burst of 2", n)
	}
	// With b1 and b2 gone, b5 moves from position 5 to position 3.
	if got := poll(t, r, "b5"); got.State != Queued || got.Position != 3 {
		t.Fatalf("b5 should be third in line: %+v", got)
	}
	if got := poll(t, r, "b1"); got.State != Admitted {
		t.Fatalf("b1 was first and must be admitted: %+v", got)
	}
}

func TestJoiningTwiceKeepsTheOriginalPlace(t *testing.T) {
	r := newRoom(t, Config{Rate: 1000})
	join(t, r, "a")
	first := join(t, r, "b")
	join(t, r, "c")
	again := join(t, r, "b")
	if again.Ticket != first.Ticket || again.Position != first.Position {
		t.Fatalf("a repeated join must not change the place: %+v then %+v", first, again)
	}
	if n, _ := r.QueueLength(context.Background()); n != 3 {
		t.Fatalf("queue length %d, want 3 (no duplicate entry)", n)
	}
}

func TestFullQueueSaysRetryAndDoesNotEnqueue(t *testing.T) {
	r := newRoom(t, Config{Rate: 1000, MaxQueue: 3})
	for i := 0; i < 3; i++ {
		join(t, r, fmt.Sprintf("b%d", i))
	}
	if st := join(t, r, "late"); st.State != Retry {
		t.Fatalf("a full room must say retry: %+v", st)
	}
	if st := poll(t, r, "late"); st.State != NotQueued {
		t.Fatalf("a refused buyer is not queued: %+v", st)
	}
}

func TestPollOfAnUnknownBuyerIsNotQueued(t *testing.T) {
	r := newRoom(t, Config{Rate: 1000})
	if st := poll(t, r, "nobody"); st.State != NotQueued {
		t.Fatalf("%+v", st)
	}
	join(t, r, "a")
	r.Wipe(context.Background()) // a restarted Redis forgets the queue
	if st := poll(t, r, "a"); st.State != NotQueued {
		t.Fatalf("after losing the queue the buyer must be told to join again: %+v", st)
	}
}

// Admission order must equal join order for buyers who keep polling (D9's FIFO guarantee).
func TestAdmissionOrderIsJoinOrder(t *testing.T) {
	r := newRoom(t, Config{Rate: 5000, Burst: 50})
	const n = 300
	for i := 0; i < n; i++ {
		join(t, r, fmt.Sprintf("b%03d", i))
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if q, _ := r.QueueLength(context.Background()); q == 0 {
			break
		}
		admit(t, r)
		time.Sleep(5 * time.Millisecond)
	}
	type rec struct{ ticket, order int64 }
	var recs []rec
	for i := 0; i < n; i++ {
		st := poll(t, r, fmt.Sprintf("b%03d", i))
		if st.State != Admitted {
			t.Fatalf("buyer %d not admitted: %+v", i, st)
		}
		recs = append(recs, rec{ticket: int64(i + 1), order: st.AdmitOrder})
	}
	sort.Slice(recs, func(a, b int) bool { return recs[a].ticket < recs[b].ticket })
	for i := 1; i < len(recs); i++ {
		if recs[i].order <= recs[i-1].order {
			t.Fatalf("order inversion: ticket %d admitted %d, ticket %d admitted %d",
				recs[i-1].ticket, recs[i-1].order, recs[i].ticket, recs[i].order)
		}
	}
}

// The admission rate is global: the budget lives in Redis, so more callers do not admit faster.
func TestAdmissionRateIsGlobalAcrossCallers(t *testing.T) {
	const rate, burst, queued = 200.0, 20, 1500
	r := newRoom(t, Config{Rate: rate, Burst: burst, Tick: 10 * time.Millisecond})
	for i := 0; i < queued; i++ {
		join(t, r, fmt.Sprintf("b%d", i))
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ { // four "API instances", each running its own admission loop
		wg.Add(1)
		go func() { defer wg.Done(); r.Run(ctx) }()
	}
	wg.Wait()
	elapsed := time.Since(start).Seconds()
	got := r.Stats.Admitted.Load()
	upper := float64(burst) + rate*elapsed
	if float64(got) > upper*1.05+2 {
		t.Fatalf("admitted %d in %.2fs; the global bound is about %.0f (4 callers must not multiply the rate)", got, elapsed, upper)
	}
	if float64(got) < upper*0.6 {
		t.Fatalf("admitted only %d in %.2fs, expected near %.0f: the room is starving buyers", got, elapsed, upper)
	}
	t.Logf("admitted %d in %.2fs with four callers; bound %.0f", got, elapsed, upper)
}

func TestAbandonedTicketsAreSkippedWithoutSpendingBudget(t *testing.T) {
	r := newRoom(t, Config{Rate: 1000, HeartbeatTTL: 150 * time.Millisecond})
	join(t, r, "ghost") // never polls again
	join(t, r, "alive")
	time.Sleep(100 * time.Millisecond)
	poll(t, r, "alive")                // keeps its ticket alive
	time.Sleep(100 * time.Millisecond) // ghost's heartbeat (150 ms) has now lapsed; alive's was renewed at 100 ms
	admit(t, r)
	if st := poll(t, r, "alive"); st.State != Admitted {
		t.Fatalf("the live buyer must be admitted: %+v", st)
	}
	if st := poll(t, r, "ghost"); st.State != NotQueued {
		t.Fatalf("the abandoned ticket must be dropped, not admitted: %+v", st)
	}
	if r.Stats.StaleSkipped.Load() != 1 || r.Stats.Admitted.Load() != 1 {
		t.Fatalf("skipped %d admitted %d", r.Stats.StaleSkipped.Load(), r.Stats.Admitted.Load())
	}
}

func TestPassValidationAndExpiry(t *testing.T) {
	r := newRoom(t, Config{Rate: 1000, PassTTL: 250 * time.Millisecond})
	join(t, r, "a")
	for admit(t, r) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	st := poll(t, r, "a")
	if st.State != Admitted || st.Token == "" {
		t.Fatalf("%+v", st)
	}
	ctx := context.Background()
	if buyer, err := r.Validate(ctx, st.Token); err != nil || buyer != "a" {
		t.Fatalf("a valid pass must name its buyer: %q %v", buyer, err)
	}
	if _, err := r.Validate(ctx, "forged"); err != ErrNoPass {
		t.Fatalf("a forged token must be refused: %v", err)
	}
	if _, err := r.Validate(ctx, ""); err != ErrNoPass {
		t.Fatalf("no token must be refused: %v", err)
	}
	time.Sleep(400 * time.Millisecond)
	if _, err := r.Validate(ctx, st.Token); err != ErrNoPass {
		t.Fatalf("an expired pass must be refused: %v", err)
	}
}

func TestAdmittedBuyerGetsTheSamePassOnRejoin(t *testing.T) {
	r := newRoom(t, Config{Rate: 1000})
	join(t, r, "a")
	for admit(t, r) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	first := poll(t, r, "a")
	again := join(t, r, "a")
	if again.State != Admitted || again.Token != first.Token {
		t.Fatalf("rejoining must return the existing pass: %+v vs %+v", first, again)
	}
}

func TestRetryAfterShrinksWithPosition(t *testing.T) {
	r := newRoom(t, Config{Rate: 100})
	if near, far := r.retryAfter(1), r.retryAfter(2000); near >= far || near < 200*time.Millisecond || far > 5*time.Second {
		t.Fatalf("near %v far %v", near, far)
	}
}
