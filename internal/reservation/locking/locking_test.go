package locking_test

import (
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"reservationengine/internal/reservation"
	"reservationengine/internal/reservation/locking"
	"reservationengine/internal/reservation/reservationtest"
)

func TestBehaviour(t *testing.T) {
	reservationtest.Run(t, func(p *pgxpool.Pool) reservation.Service { return locking.New(p) })
}
