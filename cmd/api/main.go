// Command api runs the reservation HTTP server.
//
// Settings come from the environment (see internal/config):
//
//	STRATEGY         locking (default), optimistic or naive
//	SCHEMA           protected (default) or unprotected; unprotected has no unique-index backstop
//	PAYMENT_PROFILE  fast (default) or realistic
//	HOLD_TTL         hold duration, e.g. 30s (default 300s)
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

	"resourcereservation/internal/config"
	"resourcereservation/internal/db"
	"resourcereservation/internal/httpapi"
	"resourcereservation/internal/payment"
	"resourcereservation/internal/reservation"
	"resourcereservation/internal/reservation/locking"
	"resourcereservation/internal/reservation/naive"
	"resourcereservation/internal/reservation/optimistic"
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
	pool, err := db.Setup(ctx, cfg.DatabaseURL, schema, schema == db.SchemaProtected)
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
	case "naive":
		svc = naive.New(pool)
	default:
		return errors.New("unknown STRATEGY " + strategy + " (want locking, optimistic or naive)")
	}
	payCfg, err := payment.Profile(envOr("PAYMENT_PROFILE", "fast"))
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:    cfg.HTTPAddr,
		Handler: httpapi.New(httpapi.Deps{Service: svc, Payment: payment.New(payCfg), Pool: pool, HoldTTL: cfg.HoldTTL}),
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	slog.Info("listening", "addr", cfg.HTTPAddr, "strategy", strategy, "schema", schema, "hold_ttl", cfg.HoldTTL)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
