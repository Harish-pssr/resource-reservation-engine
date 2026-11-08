package checker_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"resourcereservation/internal/checker"
	"resourcereservation/internal/testdb"
)

// seed creates an event with n items (capacity n) and returns the event and item ids.
func seed(t *testing.T, pool *pgxpool.Pool, n int) (int64, []int64) {
	t.Helper()
	ctx := context.Background()
	var ev int64
	if err := pool.QueryRow(ctx, `INSERT INTO events (name, capacity) VALUES ('e', $1) RETURNING id`, n).Scan(&ev); err != nil {
		t.Fatal(err)
	}
	rows, err := pool.Query(ctx, `INSERT INTO items (event_id, seq) SELECT $1, g FROM generate_series(1, $2) g RETURNING id`, ev, n)
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
	return ev, ids
}

func book(t *testing.T, pool *pgxpool.Pool, item int64, buyer string, markConfirmed bool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO bookings (item_id, event_id, buyer_id, hold_version)
		SELECT id, event_id, $2, 1 FROM items WHERE id = $1`, item, buyer); err != nil {
		t.Fatal(err)
	}
	if markConfirmed {
		if _, err := pool.Exec(ctx, `UPDATE items SET state='confirmed', holder=$2 WHERE id=$1`, item, buyer); err != nil {
			t.Fatal(err)
		}
	}
}

func check(t *testing.T, pool *pgxpool.Pool) checker.Report {
	t.Helper()
	r, err := checker.Check(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestCleanStatePasses(t *testing.T) {
	pool := testdb.New(t, true)
	_, it := seed(t, pool, 3)
	book(t, pool, it[0], "a", true)
	r := check(t, pool)
	if !r.OK() || r.ConfirmedBookings != 1 || r.TotalCapacity != 3 {
		t.Fatalf("clean state flagged: %v", r)
	}
}

func TestDetectsDoubleSoldItem(t *testing.T) {
	pool := testdb.New(t, false) // the unprotected schema lets the double sale in
	_, it := seed(t, pool, 3)
	book(t, pool, it[0], "a", true)
	book(t, pool, it[0], "b", false)
	r := check(t, pool)
	if r.OK() || r.DoubleSoldItems != 1 || r.ExtraBookings != 1 {
		t.Fatalf("double sale not detected: %v", r)
	}
	if r.Oversold != 0 {
		t.Fatalf("2 bookings on a 3-seat event is double-sold but not oversold: %v", r)
	}
}

func TestDetectsOversold(t *testing.T) {
	pool := testdb.New(t, false)
	_, it := seed(t, pool, 2)
	book(t, pool, it[0], "a", true)
	book(t, pool, it[0], "b", false)
	book(t, pool, it[1], "c", true)
	book(t, pool, it[1], "d", false)
	r := check(t, pool) // 4 confirmed bookings, capacity 2
	if r.Oversold != 2 || r.DoubleSoldItems != 2 {
		t.Fatalf("oversell not detected correctly: %v", r)
	}
}

func TestDetectsLedgerMismatch(t *testing.T) {
	pool := testdb.New(t, true)
	_, it := seed(t, pool, 2)
	book(t, pool, it[0], "a", false) // booking exists, item not marked confirmed
	if _, err := pool.Exec(context.Background(), `UPDATE items SET state='confirmed', holder='x' WHERE id=$1`, it[1]); err != nil {
		t.Fatal(err)
	}
	r := check(t, pool) // item 1 confirmed with no booking
	if r.BookingsOnUnconfirmedItems != 1 || r.ConfirmedItemsWithoutBooking != 1 {
		t.Fatalf("ledger mismatch not detected: %v", r)
	}
}

func TestExpiredHoldsAreNotStuckAndProbeLeavesNoTrace(t *testing.T) {
	pool := testdb.New(t, true)
	_, it := seed(t, pool, 2)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE items SET state='held', holder='a', hold_expires_at = clock_timestamp() - interval '1 minute' WHERE id=$1`, it[0]); err != nil {
		t.Fatal(err)
	}
	if r := check(t, pool); r.StuckHolds != 0 {
		t.Fatalf("an expired hold must be takeable: %v", r)
	}
	var state string
	pool.QueryRow(ctx, `SELECT state FROM items WHERE id=$1`, it[0]).Scan(&state)
	if state != "held" {
		t.Fatalf("the checker must not change the data, state is now %q", state)
	}
}
