package db_test

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"

	"resourcereservation/internal/config"
	"resourcereservation/internal/db"
	"resourcereservation/internal/testdb"
)

func TestMigrateAppliesOnceInOrder(t *testing.T) {
	pool := testdb.NewEmpty(t)
	fsys := fstest.MapFS{
		"m/0002_b.sql": {Data: []byte(`INSERT INTO t VALUES (2);`)},
		"m/0001_a.sql": {Data: []byte(`CREATE TABLE t (n int); INSERT INTO t VALUES (1);`)},
	}
	applied, err := db.Migrate(context.Background(), pool, fsys, "m")
	if err != nil || strings.Join(applied, ",") != "0001_a.sql,0002_b.sql" {
		t.Fatalf("first run: applied=%v err=%v", applied, err)
	}
	applied, err = db.Migrate(context.Background(), pool, fsys, "m")
	if err != nil || len(applied) != 0 {
		t.Fatalf("second run should apply nothing: applied=%v err=%v", applied, err)
	}
}

func TestMigrateFailureLeavesNothingHalfApplied(t *testing.T) {
	pool := testdb.NewEmpty(t)
	fsys := fstest.MapFS{
		"m/0001_bad.sql": {Data: []byte(`CREATE TABLE half (n int); SELECT * FROM does_not_exist;`)},
	}
	if _, err := db.Migrate(context.Background(), pool, fsys, "m"); err == nil {
		t.Fatal("expected an error")
	}
	var exists bool
	pool.QueryRow(context.Background(), `SELECT to_regclass('half') IS NOT NULL`).Scan(&exists)
	if exists {
		t.Fatal("table from a failed migration must not exist")
	}
}

// The pool size that is configured must be the pool size that is used: a benchmark record states it.
func TestPoolSizeOptionIsApplied(t *testing.T) {
	pool, err := db.Open(context.Background(), config.FromEnv().DatabaseURL, "", db.WithMaxConns(3))
	if err != nil {
		t.Skipf("postgres not reachable: %v", err)
	}
	defer pool.Close()
	if got := pool.Config().MaxConns; got != 3 {
		t.Fatalf("MaxConns %d, want 3", got)
	}
}
