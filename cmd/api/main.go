// Command api runs the reservation HTTP server.
//
// Settings come from the environment (see internal/config):
//
//	STRATEGY         locking (default), optimistic, fastpath (Redis gate + locking) or naive
//	SCHEMA           protected (default) or unprotected; unprotected has no unique-index backstop
//	PAYMENT_PROFILE  fast (default) or realistic; PAYMENT_FAIL_PCT, PAYMENT_TIMEOUT_PCT, PAYMENT_DELAY_MIN_MS and PAYMENT_DELAY_MAX_MS override it
//	HOLD_TTL         hold duration, e.g. 30s (default 300s)
//	FAULTS_ENABLED   "true" turns on the fault-injection hooks (D13); ADMIN_TOKEN guards their controls
//	GATE_PREFIX, ROOM_PREFIX  Redis key prefixes (default rre: and rroom:), so parallel servers do not share keys
//	GATE_PENDING_TTL fast-path claim lifetime before the database confirms it (default 2s)
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
	"strconv"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"resourcereservation/internal/config"
	"resourcereservation/internal/db"
	"resourcereservation/internal/faults"
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
		gate := fastpath.NewRedisGate(redis.NewClient(&redis.Options{Addr: cfg.RedisAddr}), envOr("GATE_PREFIX", "rre:"))
		fcfg := fastpath.DefaultConfig()
		if d, err := time.ParseDuration(os.Getenv("GATE_PENDING_TTL")); err == nil && d > 0 {
			fcfg.PendingTTL = d // how long a claim lives before the database reserve confirms it (default 2s)
		}
		svc = fastpath.New(locking.New(pool), gate, pool, fcfg)
	case "naive":
		svc = naive.New(pool)
	default:
		return errors.New("unknown STRATEGY " + strategy + " (want locking, optimistic, fastpath or naive)")
	}
	// Fault injection (D13): off unless FAULTS_ENABLED=true. The registry polls its settings table.
	reg := faults.New(pool, cfg.FaultsEnabled, 500*time.Millisecond, slog.Default())
	if fa, ok := svc.(faults.Aware); ok {
		fa.SetFaults(reg)
	}
	if reg.Enabled() {
		slog.Warn("FAULT INJECTION IS ENABLED: this server can be made to crash on request")
		go reg.Run(ctx)
	}
	payCfg, err := payment.Profile(envOr("PAYMENT_PROFILE", "fast"))
	if err != nil {
		return err
	}
	// Explicit overrides, so a test can make the simulated payment deterministic (for example 0 percent
	// failures). Benchmarks use a named profile only, which their records state.
	if v, err := strconv.ParseFloat(os.Getenv("PAYMENT_FAIL_PCT"), 64); err == nil {
		payCfg.FailPct = v
	}
	if v, err := strconv.ParseFloat(os.Getenv("PAYMENT_TIMEOUT_PCT"), 64); err == nil {
		payCfg.TimeoutPct = v
	}
	// The delay range can be overridden too: the expiry-race experiment needs payments that outlive a short hold.
	if v, err := strconv.Atoi(os.Getenv("PAYMENT_DELAY_MIN_MS")); err == nil {
		payCfg.DelayMin = time.Duration(v) * time.Millisecond
	}
	if v, err := strconv.Atoi(os.Getenv("PAYMENT_DELAY_MAX_MS")); err == nil {
		payCfg.DelayMax = time.Duration(v) * time.Millisecond
	}

	var room *waitingroom.Room
	if cfg.RoomRate > 0 {
		room = waitingroom.New(redis.NewClient(&redis.Options{Addr: cfg.RedisAddr}), envOr("ROOM_PREFIX", "rroom:"), waitingroom.Config{
			Rate: cfg.RoomRate, MaxQueue: cfg.RoomMaxQueue, PassTTL: cfg.RoomPassTTL, HeartbeatTTL: cfg.RoomHeartbeatTTL})
		go room.Run(ctx) // any number of API instances may run this: the rate budget lives in Redis
	}

	srv := &http.Server{
		Addr: cfg.HTTPAddr,
		Handler: httpapi.New(httpapi.Deps{Service: svc, Payment: payment.New(payCfg), Pool: pool, HoldTTL: cfg.HoldTTL, Room: room,
			Faults: reg, AdminToken: cfg.AdminToken}),
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
