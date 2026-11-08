package db

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"reservationengine/internal/config"
)

// newTestPool creates a throwaway database so tests never touch real data.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, config.FromEnv().DatabaseURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := admin.Ping(ctx); err != nil {
		admin.Close()
		t.Skipf("postgres not reachable (run scripts/up first): %v", err)
	}
	name := fmt.Sprintf("test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	cfg, _ := pgxpool.ParseConfig(config.FromEnv().DatabaseURL)
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect test db: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)")
		admin.Close()
	})
	return pool
}

func TestMigrateAppliesOnceInOrder(t *testing.T) {
	pool := newTestPool(t)
	fsys := fstest.MapFS{
		"m/0002_b.sql": {Data: []byte(`INSERT INTO t VALUES (2);`)},
		"m/0001_a.sql": {Data: []byte(`CREATE TABLE t (n int); INSERT INTO t VALUES (1);`)},
	}
	applied, err := Migrate(context.Background(), pool, fsys, "m")
	if err != nil || strings.Join(applied, ",") != "0001_a.sql,0002_b.sql" {
		t.Fatalf("first run: applied=%v err=%v", applied, err)
	}
	applied, err = Migrate(context.Background(), pool, fsys, "m")
	if err != nil || len(applied) != 0 {
		t.Fatalf("second run should apply nothing: applied=%v err=%v", applied, err)
	}
}

func TestMigrateFailureLeavesNothingHalfApplied(t *testing.T) {
	pool := newTestPool(t)
	fsys := fstest.MapFS{
		"m/0001_bad.sql": {Data: []byte(`CREATE TABLE half (n int); SELECT * FROM does_not_exist;`)},
	}
	if _, err := Migrate(context.Background(), pool, fsys, "m"); err == nil {
		t.Fatal("expected an error")
	}
	var exists bool
	pool.QueryRow(context.Background(), `SELECT to_regclass('half') IS NOT NULL`).Scan(&exists)
	if exists {
		t.Fatal("table from a failed migration must not exist")
	}
}
