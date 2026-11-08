package db_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"reservationengine/internal/testdb"
)

// seedItem creates one event with one item and returns the item id.
func seedItem(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	ctx := context.Background()
	var eventID, itemID int64
	if err := pool.QueryRow(ctx, `INSERT INTO events (name, capacity) VALUES ('e', 1) RETURNING id`).Scan(&eventID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO items (event_id, seq) VALUES ($1, 1) RETURNING id`, eventID).Scan(&itemID); err != nil {
		t.Fatal(err)
	}
	return itemID
}

func insertBooking(pool *pgxpool.Pool, itemID int64, buyer string) error {
	_, err := pool.Exec(context.Background(), `
		INSERT INTO bookings (item_id, event_id, buyer_id, hold_version)
		SELECT id, event_id, $2, 1 FROM items WHERE id = $1`, itemID, buyer)
	return err
}

// The hard guarantee (D2): the protected schema refuses a second confirmed booking for an item.
func TestProtectedSchemaRefusesSecondConfirmedBooking(t *testing.T) {
	pool := testdb.New(t, true)
	item := seedItem(t, pool)
	if err := insertBooking(pool, item, "a"); err != nil {
		t.Fatalf("first booking must succeed: %v", err)
	}
	if err := insertBooking(pool, item, "b"); err == nil {
		t.Fatal("second confirmed booking for the same item must be refused")
	}
}

// Negative control for the schema itself: without the backstop the same insert succeeds, so
// the checker can see a double sale in the database.
func TestUnprotectedSchemaAllowsSecondConfirmedBooking(t *testing.T) {
	pool := testdb.New(t, false)
	item := seedItem(t, pool)
	if err := insertBooking(pool, item, "a"); err != nil {
		t.Fatal(err)
	}
	if err := insertBooking(pool, item, "b"); err != nil {
		t.Fatalf("the unprotected variant must allow the double sale: %v", err)
	}
}

func TestItemStateConstraints(t *testing.T) {
	pool := testdb.New(t, true)
	item := seedItem(t, pool)
	ctx := context.Background()
	// held without a holder, and held without an expiry, are both illegal states.
	if _, err := pool.Exec(ctx, `UPDATE items SET state = 'held' WHERE id = $1`, item); err == nil {
		t.Fatal("held without a holder must be refused")
	}
	if _, err := pool.Exec(ctx, `UPDATE items SET state = 'held', holder = 'a' WHERE id = $1`, item); err == nil {
		t.Fatal("held without an expiry must be refused")
	}
	if _, err := pool.Exec(ctx, `UPDATE items SET state = 'held', holder = 'a', hold_expires_at = now() WHERE id = $1`, item); err != nil {
		t.Fatalf("a well-formed hold must be accepted: %v", err)
	}
}
