package locking_test

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"resourcereservation/internal/reservation"
	"resourcereservation/internal/reservation/locking"
	"resourcereservation/internal/reservation/reservationtest"
)

func TestBehaviour(t *testing.T) {
	reservationtest.Run(t, func(p *pgxpool.Pool) reservation.Service { return locking.New(p) })
}
