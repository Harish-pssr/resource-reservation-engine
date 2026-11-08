package crashtest

import (
	"context"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"resourcereservation/internal/checker"
	"resourcereservation/internal/config"
	"resourcereservation/internal/db"
	"resourcereservation/internal/testdb"
)

// The marker goes stale after the payment timeout (1 s in the fast profile) plus 2 s of slack.
const markerStale = 3300 * time.Millisecond

// gatePending is the lifetime of an unconfirmed fast-path claim in these tests. It is longer than the
// default so that "the leaked claim still refuses another buyer" is observable after a server restart.
const gatePending = 5 * time.Second

type env struct {
	t        *testing.T
	pool     *pgxpool.Pool
	url      string
	items    []int64
	vars     []string
	s        *server
	exitCode int
	killed   bool
}

// needRedis checks that Redis answers. In an ordinary test run a missing Redis skips the scenario; in an
// evidence run (CRASH_RESULTS set) a skip would silently shrink the evidence, so it fails instead.
func needRedis(t *testing.T) {
	conn, err := net.DialTimeout("tcp", config.FromEnv().RedisAddr, 3*time.Second)
	if err != nil {
		if os.Getenv("CRASH_RESULTS") != "" {
			t.Fatalf("redis not reachable during an evidence run: %v", err)
		}
		t.Skipf("redis not reachable (run scripts/up first): %v", err)
	}
	conn.Close()
}

// run executes a scenario reps() times, each on a fresh database with fresh processes, and finishes every
// run with the independent checker.
func run(t *testing.T, row, point, strategy string, redis bool, body func(e *env)) {
	for rep := 1; rep <= reps(); rep++ {
		rep := rep
		t.Run("rep"+string(rune('0'+rep)), func(t *testing.T) {
			if redis {
				needRedis(t)
			}
			pool, url := testdb.NewWithURL(t, true)
			_, items, err := db.CreateEvent(context.Background(), pool, "crash", 6)
			if err != nil {
				t.Fatal(err)
			}
			vars := append(baseEnv(url, strategy, newPrefix()), "GATE_PENDING_TTL="+gatePending.String())
			e := &env{t: t, pool: pool, url: url, items: items, vars: vars}
			e.s = start(t, vars)

			body(e)

			rep2, err := checker.Check(context.Background(), pool)
			if err != nil {
				t.Fatal(err)
			}
			if !rep2.OK() {
				t.Fatalf("the checker found a violation after the crash: %v", rep2)
			}
			record(t, row, point, strategy, rep, e.exitCode, e.killed)
		})
	}
}

// crash arms the point on the running server, runs the request that reaches it, and verifies that the
// process really died with status 137 and the client saw a broken connection, not an answer.
func (e *env) crash(point string, request func() response) {
	e.t.Helper()
	e.s.arm(point)
	r := request()
	if r.err == nil {
		e.t.Fatalf("the client got an answer (%d %v) but the process was meant to die at %s", r.status, r.body, point)
	}
	code, exited := e.s.waitExit(10 * time.Second)
	if !exited {
		e.t.Fatalf("the process did not exit at %s:\n%s", point, e.s.logText())
	}
	if code != 137 {
		e.t.Fatalf("exit status %d, want 137 (an injected crash)", code)
	}
	e.exitCode, e.killed = code, true
}

// restart starts a new server on the same database. The fault was consumed by the crash, so the new
// process finds remaining = 0 in the table and does not crash again.
func (e *env) restart() { e.s = start(e.t, e.vars) }

func (e *env) item(i int) (state, holder string, version int64) {
	var h *string
	e.pool.QueryRow(context.Background(), `SELECT state, holder, hold_version FROM items WHERE id = $1`, e.items[i]).Scan(&state, &h, &version)
	if h != nil {
		holder = *h
	}
	return
}

func (e *env) bookings() (n int) {
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM bookings WHERE status = 'confirmed'`).Scan(&n)
	return n
}

func (e *env) want(what string, got, want any) {
	e.t.Helper()
	if got != want {
		e.t.Fatalf("%s: got %v, want %v", what, got, want)
	}
}

// ---- rows 6 to 10: the hold path ----

// Row 6: killed after the Redis claim, before the database reserve. No hold exists; the leaked claim
// refuses other buyers for at most its short pending lifetime, then lapses.
func TestRow6KilledAfterGateClaim(t *testing.T) {
	run(t, "6", "hold.after_gate_claim", "fastpath", true, func(e *env) {
		e.crash("hold.after_gate_claim", func() response { return e.s.hold("a", "r1", e.items[0]) })
		e.restart()
		st, _, _ := e.item(0)
		e.want("the database holds nothing", st, "free")
		e.want("while the claim lives, another buyer is refused (a bounded lost sale)", e.s.hold("b", "r2", e.items[0]).status, http.StatusConflict)
		time.Sleep(gatePending + 700*time.Millisecond)
		e.want("once the claim lapses, the item is bookable", e.s.hold("b", "r3", e.items[0]).status, http.StatusCreated)
	})
}

// Row 8: killed inside the reserve transaction, before the commit. Postgres rolls it back.
func TestRow8KilledBeforeHoldCommit(t *testing.T) {
	run(t, "8", "hold.before_commit", "locking", false, func(e *env) {
		e.crash("hold.before_commit", func() response { return e.s.hold("a", "r1", e.items[0]) })
		e.restart()
		st, _, _ := e.item(0)
		e.want("the transaction was rolled back", st, "free")
		r := e.s.hold("a", "r1", e.items[0])
		e.want("the retry is a first request and succeeds", r.status, http.StatusCreated)
		e.want("as hold version 1", refsOf(r)[0]["hold_version"], float64(1))
	})
}

// Row 9: killed after the commit, before the reply. The hold exists; the retry returns the same hold.
func TestRow9KilledBeforeHoldReply(t *testing.T) {
	run(t, "9", "hold.before_reply", "locking", false, func(e *env) {
		e.crash("hold.before_reply", func() response { return e.s.hold("a", "r1", e.items[0]) })
		e.restart()
		st, holder, v := e.item(0)
		e.want("the hold was committed", st+"/"+holder, "held/a")
		e.want("as version 1", v, int64(1))
		r := e.s.hold("a", "r1", e.items[0]) // the client retries with the same request id
		e.want("the retry succeeds", r.status, http.StatusCreated)
		e.want("and returns the same hold, not a second one", refsOf(r)[0]["hold_version"], float64(1))
		e.want("another buyer is refused", e.s.hold("b", "r2", e.items[0]).status, http.StatusConflict)
	})
}

// Row 10: killed after the commit, before the claim is extended. The database has the hold; the gate
// only had a short claim, which lapses; the database still refuses everyone else.
func TestRow10KilledBeforeGateExtend(t *testing.T) {
	run(t, "10", "hold.before_gate_extend", "fastpath", true, func(e *env) {
		e.crash("hold.before_gate_extend", func() response { return e.s.hold("a", "r1", e.items[0]) })
		e.restart()
		st, holder, _ := e.item(0)
		e.want("the hold was committed", st+"/"+holder, "held/a")
		time.Sleep(gatePending + 700*time.Millisecond) // the gate's claim has lapsed
		e.want("with an empty gate the database still refuses another buyer", e.s.hold("b", "r2", e.items[0]).status, http.StatusConflict)
		r := e.s.hold("a", "r1", e.items[0])
		e.want("the holder's retry still gets its hold", r.status, http.StatusCreated)
		e.want("the same one", refsOf(r)[0]["hold_version"], float64(1))
	})
}

// ---- rows 12 to 17: the confirm path ----

// confirmCrash is the shared shape of rows 12, 13, 15 and 16: the buyer holds, the server dies somewhere in
// the confirm, and after a restart the retry is told "in progress" until the marker is stale, then proceeds.
func confirmCrash(t *testing.T, row, point string, afterRestart func(e *env)) {
	run(t, row, point, "locking", false, func(e *env) {
		h := e.s.hold("a", "r1", e.items[0])
		e.want("the hold", h.status, http.StatusCreated)
		refs := refsOf(h)
		e.crash(point, func() response { return e.s.confirm("a", refs) })
		e.restart()
		afterRestart(e)
		r := e.s.confirm("a", refs)
		e.want("a retry right after the crash is told a payment may be in flight", r.status, http.StatusConflict)
		e.want("with that outcome", r.body["outcome"], "payment_in_progress")
		time.Sleep(markerStale)
		r = e.s.confirm("a", refs)
		e.want("once the marker is stale the retry goes through", r.status, http.StatusOK)
		e.want("and is booked exactly once", e.bookings(), 1)
	})
}

// Row 12: killed after the payment marker committed, before the payment started.
func TestRow12KilledAfterPaymentMarker(t *testing.T) {
	confirmCrash(t, "12", "confirm.after_marker", func(e *env) {
		e.want("nothing is booked", e.bookings(), 0)
	})
}

// Row 13: killed in the middle of the payment. A real provider might have charged the buyer: the
// retry after the marker goes stale would pay again (a documented limitation); the booking is still once.
func TestRow13KilledDuringPayment(t *testing.T) {
	confirmCrash(t, "13", "confirm.during_payment", func(e *env) {
		e.want("nothing is booked", e.bookings(), 0)
	})
}

// Row 15: killed after the payment succeeded, before the confirm. Paid, not booked; the hold persists.
func TestRow15KilledAfterPayment(t *testing.T) {
	confirmCrash(t, "15", "confirm.after_payment", func(e *env) {
		st, holder, _ := e.item(0)
		e.want("the hold persists", st+"/"+holder, "held/a")
		e.want("nothing is booked", e.bookings(), 0)
	})
}

// Row 16: killed inside the confirm transaction, before the commit. Nothing is booked; the hold is intact.
func TestRow16KilledBeforeConfirmCommit(t *testing.T) {
	confirmCrash(t, "16", "confirm.before_commit", func(e *env) {
		st, holder, _ := e.item(0)
		e.want("the transaction was rolled back and the hold is intact", st+"/"+holder, "held/a")
		e.want("nothing is booked", e.bookings(), 0)
	})
}

// Row 17: killed after the confirm committed, before the reply. Booked once; the retry is a replay.
func TestRow17KilledBeforeConfirmReply(t *testing.T) {
	run(t, "17", "confirm.before_reply", "locking", false, func(e *env) {
		h := e.s.hold("a", "r1", e.items[0])
		refs := refsOf(h)
		e.crash("confirm.before_reply", func() response { return e.s.confirm("a", refs) })
		e.restart()
		e.want("the booking was committed", e.bookings(), 1)
		r := e.s.confirm("a", refs)
		e.want("the retry succeeds at once, no waiting for a marker", r.status, http.StatusOK)
		e.want("and is recognised as a replay", r.body["replayed"], true)
		e.want("no second payment was started for an item that was already booked", e.s.paymentAttempts(), float64(0))
		e.want("there is still exactly one booking", e.bookings(), 1)
	})
}

// ---- negative controls: the same crashes against the naive strategy ----

// Row 17 control: without the replay protection, the retry of a confirm that already committed is told
// the hold is lost, although the booking exists: the client would believe it did not book.
func TestControlRow17NaiveStrategyMisreportsTheRetry(t *testing.T) {
	run(t, "17-control", "confirm.before_reply", "naive", false, func(e *env) {
		h := e.s.hold("a", "r1", e.items[0])
		refs := refsOf(h)
		e.crash("confirm.before_reply", func() response { return e.s.confirm("a", refs) })
		e.restart()
		e.want("the booking exists", e.bookings(), 1)
		r := e.s.confirm("a", refs)
		e.want("the naive retry is told the hold is lost although the booking exists", r.status, http.StatusConflict)
		e.want("and a second payment was started for an item that was already booked", r.body["outcome"], "hold_lost")
	})
}

// Row 9 control: without idempotent holds, the retry of a hold that already committed is told the item
// is unavailable although the buyer holds it.
func TestControlRow9NaiveStrategyRefusesTheHoldersOwnRetry(t *testing.T) {
	run(t, "9-control", "hold.before_reply", "naive", false, func(e *env) {
		e.crash("hold.before_reply", func() response { return e.s.hold("a", "r1", e.items[0]) })
		e.restart()
		st, holder, _ := e.item(0)
		e.want("the buyer does hold the item", st+"/"+holder, "held/a")
		e.want("yet the naive retry is refused", e.s.hold("a", "r1", e.items[0]).status, http.StatusConflict)
	})
}
