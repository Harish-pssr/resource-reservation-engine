// Package locking is the pessimistic strategy (D6). STUB: every method fails until implemented.
package locking

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"reservationengine/internal/reservation"
)

var errNotImplemented = errors.New("not implemented")

// Store implements reservation.Service with row locks.
type Store struct{ pool *pgxpool.Pool }

// New returns a Store on the given pool.
func New(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) Hold(context.Context, reservation.HoldRequest) (reservation.HoldResult, error) {
	return reservation.HoldResult{}, errNotImplemented
}
func (s *Store) StartPayment(context.Context, reservation.HoldRef, time.Duration) (reservation.Outcome, error) {
	return "", errNotImplemented
}
func (s *Store) Confirm(context.Context, reservation.HoldRef) (reservation.ConfirmResult, error) {
	return reservation.ConfirmResult{}, errNotImplemented
}
func (s *Store) Release(context.Context, reservation.HoldRef) (reservation.Outcome, error) {
	return "", errNotImplemented
}
func (s *Store) Availability(context.Context, int64) (int, error) { return 0, errNotImplemented }
