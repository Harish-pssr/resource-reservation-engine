// Package testdb gives each test a throwaway Postgres database so tests never touch real data.
package testdb

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"resourcereservation/internal/config"
	"resourcereservation/internal/db"
)

// testConns bounds every test pool, so many test packages running in parallel cannot exhaust the server.
const testConns = 10

func create(t *testing.T) (dbURL string) {
	t.Helper()
	ctx := context.Background()
	base := config.FromEnv().DatabaseURL
	admin, err := db.Open(ctx, base, "", db.WithMaxConns(2))
	if err != nil {
		t.Skipf("postgres not reachable (run scripts/up first): %v", err)
	}
	// Parallel test packages run as separate processes, and the Windows clock is coarse, so the
	// clock alone is not unique: add the process id and random bytes.
	name := fmt.Sprintf("test_%d_%d_%08x", os.Getpid(), time.Now().UnixNano(), rand.Uint32())
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

// NewWithURL is New, and also returns the URL of the throwaway database, so a test can start a separate
// process (a real API server) against the same data.
func NewWithURL(t *testing.T, protected bool) (*pgxpool.Pool, string) {
	t.Helper()
	schema := db.SchemaUnprotected
	if protected {
		schema = db.SchemaProtected
	}
	url := create(t)
	pool, err := db.Setup(context.Background(), url, schema, protected, db.WithMaxConns(testConns))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool, url
}

// NewEmpty returns a pool on a fresh, empty database.
func NewEmpty(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := db.Open(context.Background(), create(t), "", db.WithMaxConns(testConns))
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
	pool, err := db.Setup(context.Background(), create(t), schema, protected, db.WithMaxConns(testConns))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}
