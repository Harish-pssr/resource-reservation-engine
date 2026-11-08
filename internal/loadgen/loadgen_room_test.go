package loadgen_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"resourcereservation/internal/checker"
	"resourcereservation/internal/config"
	"resourcereservation/internal/httpapi"
	"resourcereservation/internal/loadgen"
	"resourcereservation/internal/payment"
	"resourcereservation/internal/reconcile"
	"resourcereservation/internal/reservation/locking"
	"resourcereservation/internal/testdb"
	"resourcereservation/internal/waitingroom"
	"resourcereservation/internal/workload"
)

func TestFIFOInversionsCountsOutOfOrderAdmissions(t *testing.T) {
	mk := func(pairs ...[2]int64) []loadgen.BuyerResult {
		var b []loadgen.BuyerResult
		for _, p := range pairs {
			b = append(b, loadgen.BuyerResult{Ticket: p[0], AdmitOrder: p[1]})
		}
		return b
	}
	if n := loadgen.FIFOInversions(mk([2]int64{1, 1}, [2]int64{2, 2}, [2]int64{3, 3})); n != 0 {
		t.Fatalf("in order: %d inversions", n)
	}
	if n := loadgen.FIFOInversions(mk([2]int64{1, 3}, [2]int64{2, 2}, [2]int64{3, 1})); n != 3 {
		t.Fatalf("fully reversed three: %d inversions, want 3", n)
	}
	if n := loadgen.FIFOInversions(mk([2]int64{1, 2}, [2]int64{2, 1}, [2]int64{3, 3})); n != 1 {
		t.Fatalf("one swap: %d inversions, want 1", n)
	}
	// Gaps are fine (abandoned tickets never get an admission order) and unnumbered buyers are ignored.
	if n := loadgen.FIFOInversions(mk([2]int64{1, 1}, [2]int64{5, 2}, [2]int64{9, 3}, [2]int64{0, 0})); n != 0 {
		t.Fatalf("gaps and unnumbered buyers: %d", n)
	}
	if loadgen.FIFOInversions(nil) != 0 {
		t.Fatal("empty")
	}
}

func newRoomStack(t *testing.T, rc waitingroom.Config) (*httptest.Server, *waitingroom.Room, int64, []int64, func() checker.Report, func([]loadgen.BuyerResult) reconcile.Report) {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: config.FromEnv().RedisAddr, DialTimeout: 500 * time.Millisecond})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("redis not reachable: %v", err)
	}
	t.Cleanup(func() { rdb.Close() })
	room := waitingroom.New(rdb, fmt.Sprintf("l%08x:", rand.Uint32()), rc)
	t.Cleanup(func() { room.Wipe(context.Background()) })
	runCtx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	go room.Run(runCtx)

	pool := testdb.New(t, true)
	pcfg := payment.Config{DelayMin: time.Millisecond, DelayMax: 2 * time.Millisecond, Timeout: 30 * time.Millisecond}
	h := httpapi.New(httpapi.Deps{Service: locking.New(pool), Payment: payment.New(pcfg), Pool: pool, HoldTTL: time.Minute, Room: room})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	ev, items, err := loadgen.CreateEvent(context.Background(), srv.URL, 400)
	if err != nil {
		t.Fatal(err)
	}
	chk := func() checker.Report {
		r, err := checker.Check(context.Background(), pool)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	rec := func(b []loadgen.BuyerResult) reconcile.Report {
		r, err := reconcile.Check(context.Background(), pool, ev, b)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	return srv, room, ev, items, chk, rec
}

// A surge of 300 buyers in 1 s into a room that admits 100 per second: everyone waits their turn,
// the order is FIFO, the room really limits the rate, and the database stays consistent.
func TestRoomFlowAdmitsEveryoneInOrderAtTheConfiguredRate(t *testing.T) {
	const rate, burst, buyers = 100.0, 10, 300
	srv, _, _, items, chk, rec := newRoomStack(t, waitingroom.Config{Rate: rate, Burst: burst, Tick: 20 * time.Millisecond})
	res, err := loadgen.Run(context.Background(), loadgen.Config{BaseURL: srv.URL, ItemIDs: items, Picker: workload.NewPicker(len(items), 1.1, 3),
		Rate: buyers, Duration: time.Second, RunID: "room", Room: true})
	if err != nil {
		t.Fatal(err)
	}
	s := res.Summary
	t.Logf("\n%s", loadgen.Format(s))
	if s.RoomAdmitted != buyers || s.RoomTurnedAway != 0 || s.RoomGaveUp != 0 || s.RoomErrors != 0 {
		t.Fatalf("everyone must be admitted eventually: %+v", s)
	}
	if s.FIFOInversions != 0 {
		t.Fatalf("admission must follow arrival order, got %d inversions", s.FIFOInversions)
	}
	if s.HoldNotAdmitted != 0 {
		t.Fatalf("every admitted buyer must be accepted by the API: %d refused", s.HoldNotAdmitted)
	}
	// The rate cap is visible in the wall time: 300 buyers at 100/s after a burst of 10 take about 2.9 s.
	if min := float64(buyers-burst) / rate * 0.8; s.WallS < min {
		t.Fatalf("finished in %.2fs, faster than the room's rate allows (at least %.2fs)", s.WallS, min)
	}
	if s.QueueWaitP99Ms < 1000 {
		t.Fatalf("queue wait p99 %.0f ms: the last buyers should have waited seconds", s.QueueWaitP99Ms)
	}
	if r := chk(); !r.OK() {
		t.Fatalf("checker: %v", r)
	}
	if r := rec(res.Buyers); !r.OK() {
		t.Fatalf("reconcile: %v", r)
	}
}

func TestFullRoomTurnsBuyersAwayWithoutTouchingTheStore(t *testing.T) {
	srv, _, _, items, chk, rec := newRoomStack(t, waitingroom.Config{Rate: 0.001, Burst: 1, MaxQueue: 20, Tick: time.Hour})
	res, _ := loadgen.Run(context.Background(), loadgen.Config{BaseURL: srv.URL, ItemIDs: items, Picker: workload.NewPicker(len(items), 1.1, 4),
		Rate: 100, Duration: time.Second, RunID: "full", Room: true, RoomWaitTimeout: 500 * time.Millisecond})
	s := res.Summary
	if s.RoomTurnedAway != 80 || s.RoomGaveUp != 20 || s.RoomAdmitted != 0 {
		t.Fatalf("100 buyers into a 20-place room that admits nobody: %+v", s)
	}
	if s.HoldHeld+s.HoldUnavailable+s.HoldBusy+s.HoldErrors != 0 {
		t.Fatalf("no buyer may reach the store: %+v", s)
	}
	if r := chk(); !r.OK() || r.ConfirmedBookings != 0 {
		t.Fatalf("%v", r)
	}
	if r := rec(res.Buyers); !r.OK() {
		t.Fatalf("%v", r)
	}
}
