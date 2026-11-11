//go:build integration

// Live-PostgreSQL tests for the retention PgStore. The unit tests run against
// a fake Store, which cannot see a placeholder mismatch, a batching loop that
// stops early or a predicate that reaches the wrong rows.
//
// Run: make test-integration
package retention

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Ariazonaa/statwarden/internal/db"
	"github.com/Ariazonaa/statwarden/internal/migrations"
)

// newTestPool provisions a throwaway, fully migrated database next to the one
// DATABASE_URL names and returns a pool on it.
func newTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	base := os.Getenv("DATABASE_URL")
	if base == "" {
		t.Fatal("DATABASE_URL is required for the integration suite (see `make test-integration`)")
	}
	cfg, err := pgx.ParseConfig(base)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	admin, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer admin.Close(ctx) //nolint:errcheck // best-effort cleanup
	name := fmt.Sprintf("statwarden_retention_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		c, err := pgx.ConnectConfig(context.Background(), cfg)
		if err != nil {
			return
		}
		defer c.Close(context.Background()) //nolint:errcheck // best-effort cleanup
		_, _ = c.Exec(context.Background(), `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`)
	})
	dsn := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", cfg.User, cfg.Password, cfg.Host, cfg.Port, name)
	if _, err := migrations.Run(dsn); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestPruneBeforeDeletesInBatches forces several batches and a final partial
// one, and checks that exactly the rows before the cutoff went — on both sides
// of it, so a loop that stops after the first batch or a predicate that
// overshoots both fail.
func TestPruneBeforeDeletesInBatches(t *testing.T) {
	pool := newTestPool(t)
	ctx := context.Background()
	defer func(n int) { heartbeatPruneBatch = n }(heartbeatPruneBatch)
	heartbeatPruneBatch = 7

	var monitorID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO monitors (name, type) VALUES ('retention', 'http') RETURNING id`).Scan(&monitorID); err != nil {
		t.Fatalf("insert monitor: %v", err)
	}
	cutoff := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	// 23 rows before the cutoff (3 full batches + 2), 5 at or after it.
	if _, err := pool.Exec(ctx, `
		INSERT INTO heartbeats (monitor_id, checked_at, status)
		SELECT $1, $2::timestamptz - make_interval(mins => g), 'up' FROM generate_series(1, 23) g
		UNION ALL
		SELECT $1, $2::timestamptz + make_interval(mins => g), 'up' FROM generate_series(0, 4) g`,
		monitorID, cutoff); err != nil {
		t.Fatalf("seed heartbeats: %v", err)
	}

	deleted, err := NewPgStore(pool).PruneBefore(ctx, cutoff)
	if err != nil {
		t.Fatalf("PruneBefore: %v", err)
	}
	if deleted != 23 {
		t.Fatalf("deleted = %d, want 23", deleted)
	}
	var before, after int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FILTER (WHERE checked_at < $1), count(*) FILTER (WHERE checked_at >= $1) FROM heartbeats`,
		cutoff).Scan(&before, &after); err != nil {
		t.Fatalf("count: %v", err)
	}
	if before != 0 || after != 5 {
		t.Fatalf("left %d rows before the cutoff and %d after, want 0 and 5", before, after)
	}

	// A second pass has nothing to do and must say so.
	if deleted, err := NewPgStore(pool).PruneBefore(ctx, cutoff); err != nil || deleted != 0 {
		t.Fatalf("second PruneBefore = %d, %v; want 0, nil", deleted, err)
	}
}

// TestRetentionIndexExists pins that migration 0021 left the index the batched
// delete depends on.
func TestRetentionIndexExists(t *testing.T) {
	pool := newTestPool(t)
	var method string
	if err := pool.QueryRow(context.Background(), `
		SELECT am.amname FROM pg_class c JOIN pg_am am ON am.oid = c.relam
		 WHERE c.relname = 'heartbeats_checked_at_brin_idx'`).Scan(&method); err != nil {
		t.Fatalf("heartbeats_checked_at_brin_idx is missing: %v", err)
	}
	if method != "brin" {
		t.Fatalf("index access method = %q, want brin", method)
	}
}
