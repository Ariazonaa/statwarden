//go:build integration

// Live-PostgreSQL tests for individual migrations whose data transforms the
// up/down round trip in internal/api cannot see: that test runs on an empty
// schema, so a backfill that hashes the wrong thing, or hashes twice, passes it.
//
// Run: make test-integration
package migrations

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
)

// freshDatabase creates an empty database next to the one DATABASE_URL names
// and returns its DSN; it is dropped when the test ends.
func freshDatabase(t *testing.T) string {
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
	name := fmt.Sprintf("statwarden_mig_%d", time.Now().UnixNano())
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
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable", cfg.User, cfg.Password, cfg.Host, cfg.Port, name)
}

// TestPushTokenHashMigrationIsIdempotentAndNeedsNoExtension covers migration
// 0011, which hashes the plaintext push tokens in place.
//
// Two defects, both pinned here. It used pgcrypto's digest(), so the migration
// needed CREATE EXTENSION — a superuser-only step on many managed databases —
// for a hash PostgreSQL has built in. And its UPDATE hashed every non-empty
// value, so down followed by up hashed the hashes, and every push monitor's
// token silently stopped matching.
func TestPushTokenHashMigrationIsIdempotentAndNeedsNoExtension(t *testing.T) {
	dsn := freshDatabase(t)
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close() //nolint:errcheck // best-effort cleanup
	src, err := iofs.New(files, ".")
	if err != nil {
		t.Fatal(err)
	}
	driver, err := pgxmigrate.WithInstance(sqlDB, &pgxmigrate.Config{})
	if err != nil {
		t.Fatal(err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "pgx5", driver)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Migrate(10); err != nil {
		t.Fatalf("migrate to 10: %v", err)
	}

	const seeded = "0123456789abcdef0123456789abcdef" // the shape push.GenerateToken mints
	sum := sha256.Sum256([]byte(seeded))
	want := hex.EncodeToString(sum[:])
	if _, err := sqlDB.Exec(
		`INSERT INTO monitors (name, type, push_token) VALUES ('cron', 'push', $1)`, seeded); err != nil {
		t.Fatalf("seed monitor: %v", err)
	}

	hashOf := func() string {
		t.Helper()
		var got string
		if err := sqlDB.QueryRow(`SELECT push_token_hash FROM monitors WHERE name = 'cron'`).Scan(&got); err != nil {
			t.Fatalf("read hash: %v", err)
		}
		return got
	}

	if err := m.Migrate(11); err != nil {
		t.Fatalf("migrate to 11: %v", err)
	}
	if got := hashOf(); got != want {
		t.Fatalf("hash after 0011 = %q, want push.HashToken's %q", got, want)
	}
	var ext int
	if err := sqlDB.QueryRow(`SELECT count(*) FROM pg_extension WHERE extname = 'pgcrypto'`).Scan(&ext); err != nil {
		t.Fatal(err)
	}
	if ext != 0 {
		t.Fatal("migration 0011 installed pgcrypto; the built-in sha256() needs no extension")
	}

	// Down and up again: the stored value is already a hash and must stay it.
	if err := m.Migrate(10); err != nil {
		t.Fatalf("migrate down to 10: %v", err)
	}
	if err := m.Migrate(11); err != nil {
		t.Fatalf("migrate up to 11 again: %v", err)
	}
	if got := hashOf(); got != want {
		t.Fatalf("hash after down+up = %q, want it unchanged %q — the token was hashed twice", got, want)
	}
}
