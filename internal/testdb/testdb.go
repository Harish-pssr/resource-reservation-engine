// Package testdb gives each test a throwaway Postgres database so tests never touch real data.
package testdb

import (
	"context"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"resourcereservation/internal/config"
	"resourcereservation/internal/db"
)

func create(t *testing.T) (dbURL string) {
	t.Helper()
	ctx := context.Background()
	base := config.FromEnv().DatabaseURL
	admin, err := db.Open(ctx, base, "")
	if err != nil {
		t.Skipf("postgres not reachable (run scripts/up first): %v", err)
	}
	name := fmt.Sprintf("test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		admin.Close()
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
		admin.Close()
	})
	u, _ := url.Parse(base)
	u.Path = "/" + name
	return u.String()
}

// NewEmpty returns a pool on a fresh, empty database.
func NewEmpty(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := db.Open(context.Background(), create(t), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// New returns a pool on a fresh database with the schema variant applied.
func New(t *testing.T, protected bool) *pgxpool.Pool {
	t.Helper()
	schema := db.SchemaUnprotected
	if protected {
		schema = db.SchemaProtected
	}
	pool, err := db.Setup(context.Background(), create(t), schema, protected)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
