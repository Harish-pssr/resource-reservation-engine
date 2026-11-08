// Command api runs the reservation HTTP server.
//
// Settings come from the environment (see internal/config):
//
//	STRATEGY         locking (default), optimistic, fastpath (Redis gate + locking) or naive
//	SCHEMA           protected (default) or unprotected; unprotected has no unique-index backstop
//	PAYMENT_PROFILE  fast (default) or realistic
//	HOLD_TTL         hold duration, e.g. 30s (default 300s)
//	ROOM_RATE        waiting room admission rate in buyers per second; 0 (default) = no waiting room
//	ROOM_MAX_QUEUE, ROOM_PASS_TTL, ROOM_HEARTBEAT_TTL  waiting room tuning
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"resourcereservation/internal/config"
	"resourcereservation/internal/db"
	"resourcereservation/internal/httpapi"
	"resourcereservation/internal/payment"
	"resourcereservation/internal/reservation"
	"resourcereservation/internal/reservation/fastpath"
	"resourcereservation/internal/reservation/locking"
	"resourcereservation/internal/reservation/naive"
	"resourcereservation/internal/reservation/optimistic"
	"resourcereservation/internal/waitingroom"
)

func main() {
	if err := run(); err != nil {
		slog.Error("api failed", "error", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func run() error {
	cfg := config.FromEnv()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	schema := envOr("SCHEMA", db.SchemaProtected)
	pool, err := db.Setup(ctx, cfg.DatabaseURL, schema, schema == db.SchemaProtected, db.WithMaxConns(int32(cfg.PoolSize)))
	if err != nil {
		return err
	}
	defer pool.Close()

	var svc reservation.Service
	strategy := envOr("STRATEGY", "locking")
	switch strategy {
	case "locking":
		svc = locking.New(pool)
	case "optimistic":
		svc = optimistic.New(pool)
	case "fastpath":
		gate := fastpath.NewRedisGate(redis.NewClient(&redis.Options{Addr: cfg.RedisAddr}), "rre:")
		svc = fastpath.New(locking.New(pool), gate, pool, fastpath.DefaultConfig())
	case "naive":
		svc = naive.New(pool)
	default:
		return errors.New("unknown STRATEGY " + strategy + " (want locking, optimistic, fastpath or naive)")
	}
	payCfg, err := payment.Profile(envOr("PAYMENT_PROFILE", "fast"))
	if err != nil {
		return err
	}

	var room *waitingroom.Room
	if cfg.RoomRate > 0 {
		room = waitingroom.New(redis.NewClient(&redis.Options{Addr: cfg.RedisAddr}), "rroom:", waitingroom.Config{
			Rate: cfg.RoomRate, MaxQueue: cfg.RoomMaxQueue, PassTTL: cfg.RoomPassTTL, HeartbeatTTL: cfg.RoomHeartbeatTTL})
		go room.Run(ctx) // any number of API instances may run this: the rate budget lives in Redis
	}

	srv := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: httpapi.New(httpapi.Deps{Service: svc, Payment: payment.New(payCfg), Pool: pool, HoldTTL: cfg.HoldTTL, Room: room}),
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	slog.Info("listening", "addr", cfg.HTTPAddr, "strategy", strategy, "schema", schema, "hold_ttl", cfg.HoldTTL, "room_rate", cfg.RoomRate)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
