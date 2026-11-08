package db

import (
	"context"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5/pgxpool"

	"resourcereservation/migrations"
)

// Schema names for the two variants (D2).
const (
	SchemaProtected   = "protected"   // core + unique-index backstop
	SchemaUnprotected = "unprotected" // core only: the negative control
)

var schemaName = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// Option adjusts the pool a function opens.
type Option func(*pgxpool.Config)

// WithMaxConns caps the pool at n connections. Without it pgx uses max(4, number of CPUs), which is
// not what a benchmark record would say: always pass the configured size.
func WithMaxConns(n int32) Option { return func(c *pgxpool.Config) { c.MaxConns = n } }

// Open returns a pool whose connections use the given schema ("" = server default).
func Open(ctx context.Context, url, schema string, opts ...Option) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	if schema != "" {
		cfg.ConnConfig.RuntimeParams["search_path"] = schema
	}
	for _, o := range opts {
		o(cfg)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}

// Setup creates the schema if needed, applies the migrations for the variant, and returns a
// pool using it. protected=false builds the unprotected negative-control variant.
func Setup(ctx context.Context, url, schema string, protected bool, opts ...Option) (*pgxpool.Pool, error) {
	if !schemaName.MatchString(schema) {
		return nil, fmt.Errorf("invalid schema name %q", schema)
	}
	admin, err := Open(ctx, url, "", WithMaxConns(1))
	if err != nil {
		return nil, err
	}
	_, err = admin.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+schema)
	admin.Close()
	if err != nil {
		return nil, fmt.Errorf("create schema: %w", err)
	}
	pool, err := Open(ctx, url, schema, opts...)
	if err != nil {
		return nil, err
	}
	dirs := []string{"core"}
	if protected {
		dirs = append(dirs, "backstop")
	}
	dirs = append(dirs, "ops") // fault-injection settings exist in both variants
	for _, d := range dirs {
		if _, err := Migrate(ctx, pool, migrations.FS, d); err != nil {
			pool.Close()
			return nil, err
		}
	}
	return pool, nil
}
