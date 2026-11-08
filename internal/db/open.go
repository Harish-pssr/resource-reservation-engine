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

// Open returns a pool whose connections use the given schema ("" = server default).
func Open(ctx context.Context, url, schema string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	if schema != "" {
		cfg.ConnConfig.RuntimeParams["search_path"] = schema
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
func Setup(ctx context.Context, url, schema string, protected bool) (*pgxpool.Pool, error) {
	if !schemaName.MatchString(schema) {
		return nil, fmt.Errorf("invalid schema name %q", schema)
	}
	admin, err := Open(ctx, url, "")
	if err != nil {
		return nil, err
	}
	_, err = admin.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+schema)
	admin.Close()
	if err != nil {
		return nil, fmt.Errorf("create schema: %w", err)
	}
	pool, err := Open(ctx, url, schema)
	if err != nil {
		return nil, err
	}
	dirs := []string{"core"}
	if protected {
		dirs = append(dirs, "backstop")
	}
	for _, d := range dirs {
		if _, err := Migrate(ctx, pool, migrations.FS, d); err != nil {
			pool.Close()
			return nil, err
		}
	}
	return pool, nil
}
