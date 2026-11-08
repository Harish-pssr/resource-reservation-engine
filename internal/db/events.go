package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// CreateEvent creates an event with n items (one row per unit, D2) and returns their ids in order.
func CreateEvent(ctx context.Context, pool *pgxpool.Pool, name string, n int) (eventID int64, itemIDs []int64, err error) {
	if n <= 0 {
		return 0, nil, fmt.Errorf("an event needs at least one item")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback(ctx)
	if err := tx.QueryRow(ctx, `INSERT INTO events (name, capacity) VALUES ($1, $2) RETURNING id`, name, n).Scan(&eventID); err != nil {
		return 0, nil, err
	}
	rows, err := tx.Query(ctx, `
		INSERT INTO items (event_id, seq) SELECT $1, g FROM generate_series(1, $2) g RETURNING id`, eventID, n)
	if err != nil {
		return 0, nil, err
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, nil, err
		}
		itemIDs = append(itemIDs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, nil, err
	}
	return eventID, itemIDs, tx.Commit(ctx)
}
