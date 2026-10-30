// Package db provides the PostgreSQL connection pool (pgx/pgxpool) used by
// the whole application, plus small health helpers.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Connect parses databaseURL, opens a pgxpool and verifies connectivity with
// a ping before returning. The caller owns the pool and must Close() it.
func Connect(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create connection pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

// Ping verifies database connectivity with a bounded timeout. It is used by
// the /readyz readiness endpoint.
func Ping(ctx context.Context, pool *pgxpool.Pool, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return pool.Ping(ctx)
}
