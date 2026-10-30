//go:build integration

// Live-PostgreSQL tests for PgStore. The fast unit suite runs entirely against
// MemStore, which cannot see a SQL placeholder/argument mismatch, FOR UPDATE
// semantics, a constraint mapping or a broken migration — none of those are
// compile errors. Everything here is behind the `integration` tag so
// `go test ./...` stays database-free.
//
// Why a compose service + DATABASE_URL rather than testcontainers-go:
// the repo already ships docker-compose.dev.yml with exactly postgres:16-alpine
// and documents `docker compose -f docker-compose.dev.yml up -d` as the normal
// developer loop, so pointing the suite at that service adds no new concept.
// testcontainers-go would pull the Docker/moby client tree into go.mod — a
// large non-test dependency surface for a production monitoring binary that
// currently has none — and would need a Docker socket inside the CI job,
// whereas GitHub Actions already supports `services:` natively. The env var is
// the only new contract.
//
// Run: make test-integration
package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	_ "github.com/golang-migrate/migrate/v4/source/file" // file:// source for the down/up round-trip
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx", as internal/migrations uses

	"github.com/Ariazonaa/statwarden/internal/db"
	"github.com/Ariazonaa/statwarden/internal/migrations"
)

// testPool is the pool every integration test uses. It points at a database
// created for this test binary and dropped again in TestMain.
var testPool *pgxpool.Pool

// TestMain provisions a throwaway database on the server named by DATABASE_URL,
// applies the real migrations to it, and drops it afterwards. A fresh database
// (rather than the one in the URL) means pointing the suite at the dev compose
// service never touches development data.
func TestMain(m *testing.M) {
	baseURL := os.Getenv("DATABASE_URL")
	if baseURL == "" {
		// Deliberately fatal, not a skip: asking for the integration tag is
		// already the opt-in, so a missing URL is a misconfigured run and must
		// not let CI report green without having tested anything.
		fmt.Fprintln(os.Stderr, "DATABASE_URL is required for the integration suite (see `make test-integration`)")
		os.Exit(1)
	}
	ctx := context.Background()
	dsn, drop, err := provisionDatabase(ctx, baseURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "provision test database: %v\n", err)
		os.Exit(1)
	}
	code := func() int {
		defer drop()
		if _, err := migrations.Run(dsn); err != nil {
			fmt.Fprintf(os.Stderr, "apply migrations: %v\n", err)
			return 1
		}
		pool, err := db.Connect(ctx, dsn)
		if err != nil {
			fmt.Fprintf(os.Stderr, "connect test database: %v\n", err)
			return 1
		}
		defer pool.Close()
		testPool = pool
		return m.Run()
	}()
	os.Exit(code)
}

// provisionDatabase creates an empty database next to the one baseURL names and
// returns its DSN plus a function that drops it again.
func provisionDatabase(ctx context.Context, baseURL string) (string, func(), error) {
	cfg, err := pgxpool.ParseConfig(baseURL)
	if err != nil {
		return "", nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	name := fmt.Sprintf("statwarden_it_%d_%d", os.Getpid(), time.Now().UnixNano()%1_000_000)

	admin, err := connectWithRetry(ctx, cfg.ConnConfig)
	if err != nil {
		return "", nil, fmt.Errorf("connect for provisioning: %w", err)
	}
	defer admin.Close(ctx) //nolint:errcheck // best-effort cleanup
	if _, err := admin.Exec(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		return "", nil, fmt.Errorf("create database %s: %w", name, err)
	}

	// Rebuild the DSN against the new database. Going through the parsed config
	// rather than string surgery keeps every other connection parameter
	// (sslmode, host, credentials) exactly as the caller supplied it.
	dsnCfg := cfg.ConnConfig.Copy()
	dsnCfg.Database = name
	dsn := fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		dsnCfg.User, dsnCfg.Password, dsnCfg.Host, dsnCfg.Port, name)

	drop := func() {
		// A separate connection: the pool using this database is already closed
		// by the caller, and FORCE evicts anything else still attached.
		conn, err := pgx.ConnectConfig(context.Background(), cfg.ConnConfig)
		if err != nil {
			fmt.Fprintf(os.Stderr, "drop database %s: connect: %v\n", name, err)
			return
		}
		defer conn.Close(context.Background()) //nolint:errcheck // best-effort cleanup
		if _, err := conn.Exec(context.Background(), `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`); err != nil {
			fmt.Fprintf(os.Stderr, "drop database %s: %v\n", name, err)
		}
	}
	return dsn, drop, nil
}

// connectWithRetry dials until PostgreSQL completes a handshake or the budget
// runs out. A service container accepts TCP before it finishes initialising the
// cluster, so the first connect can legitimately fail in CI.
func connectWithRetry(ctx context.Context, cfg *pgx.ConnConfig) (*pgx.Conn, error) {
	const attempts = 30
	var err error
	for i := range attempts {
		var conn *pgx.Conn
		conn, err = pgx.ConnectConfig(ctx, cfg)
		if err == nil {
			return conn, nil
		}
		if i < attempts-1 {
			time.Sleep(time.Second)
		}
	}
	return nil, fmt.Errorf("after %d attempts: %w", attempts, err)
}

// newPgStore returns a PgStore over an empty schema. Every table is truncated
// so tests do not depend on each other's rows or on identity sequences.
func newPgStore(t *testing.T) *PgStore {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`TRUNCATE users, monitors, status_pages RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return NewPgStore(testPool)
}

// insertMonitor creates a minimal monitor row and returns its id. Status pages
// reference monitors by foreign key, so the layout tests need real ones.
func insertMonitor(t *testing.T, name string) int64 {
	t.Helper()
	var id int64
	err := testPool.QueryRow(context.Background(),
		`INSERT INTO monitors (name, type) VALUES ($1, 'http') RETURNING id`, name).Scan(&id)
	if err != nil {
		t.Fatalf("insert monitor %q: %v", name, err)
	}
	return id
}

// pgErrorCode returns the SQLSTATE and constraint name of a postgres error.
func pgErrorCode(t *testing.T, err error) (string, string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("error %v (%T) is not a *pgconn.PgError", err, err)
	}
	return pgErr.Code, pgErr.ConstraintName
}

// openStdlib opens a database/sql handle on dsn using the same pgx stdlib
// driver internal/migrations registers.
func openStdlib(dsn string) (*sql.DB, error) { return sql.Open("pgx", dsn) }

// listTables returns the public tables of dsn, so a rolled-back schema can be
// asserted empty.
func listTables(dsn string) ([]string, error) {
	sqlDB, err := openStdlib(dsn)
	if err != nil {
		return nil, err
	}
	defer sqlDB.Close() //nolint:errcheck // best-effort cleanup
	rows, err := sqlDB.Query(`SELECT tablename FROM pg_tables WHERE schemaname = 'public' AND tablename <> 'schema_migrations' ORDER BY tablename`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // best-effort cleanup
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// --- migration correctness ---

// TestMigrationsMatchStoreColumns proves the schema the migrations produce
// actually carries every column pageCols selects. A migration that renames or
// forgets a column is otherwise only discovered by a failing query at runtime.
func TestMigrationsMatchStoreColumns(t *testing.T) {
	rows, err := testPool.Query(context.Background(),
		`SELECT column_name FROM information_schema.columns WHERE table_name = 'status_pages'`)
	if err != nil {
		t.Fatalf("read information_schema: %v", err)
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		have[c] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate columns: %v", err)
	}
	for _, c := range splitCols(pageCols) {
		if !have[c] {
			t.Fatalf("pageCols names %q but status_pages has no such column (columns: %v)", c, have)
		}
	}
}

// splitCols turns a column-list constant into the individual column names.
func splitCols(list string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(list, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\t'
	}) {
		out = append(out, f)
	}
	return out
}

// TestMigrationsUpDownUp applies every migration, rolls the whole schema back to
// zero and applies it again on a database of its own. It is the only check that
// the .down.sql files are correct — production never runs them, so a broken one
// stays invisible until somebody needs it.
func TestMigrationsUpDownUp(t *testing.T) {
	ctx := context.Background()
	dsn, drop, err := provisionDatabase(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	defer drop()

	dir, err := filepath.Abs(filepath.Join("..", "migrations"))
	if err != nil {
		t.Fatalf("resolve migrations dir: %v", err)
	}
	source := "file://" + filepath.ToSlash(dir)

	// A single migrate instance for the whole round trip; pgx's stdlib driver is
	// the same one internal/migrations uses in production.
	open := func() (*migrate.Migrate, func()) {
		t.Helper()
		sqlDB, err := openStdlib(dsn)
		if err != nil {
			t.Fatalf("open database: %v", err)
		}
		driver, err := pgxmigrate.WithInstance(sqlDB, &pgxmigrate.Config{})
		if err != nil {
			t.Fatalf("init migrate driver: %v", err)
		}
		m, err := migrate.NewWithDatabaseInstance(source, "pgx5", driver)
		if err != nil {
			t.Fatalf("init migrate: %v", err)
		}
		return m, func() {
			_, _ = m.Close()
			_ = sqlDB.Close()
		}
	}

	m, closeM := open()
	if err := m.Up(); err != nil {
		t.Fatalf("initial up: %v", err)
	}
	upVersion, dirty, err := m.Version()
	if err != nil || dirty {
		t.Fatalf("version after up = %d, dirty=%v, err=%v", upVersion, dirty, err)
	}
	if err := m.Down(); err != nil {
		t.Fatalf("down: %v", err)
	}
	closeM()

	// Every table the migrations created must be gone; only golang-migrate's own
	// bookkeeping table may survive.
	leftover, err := listTables(dsn)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	if len(leftover) != 0 {
		t.Fatalf("down migrations left tables behind: %v", leftover)
	}

	m2, closeM2 := open()
	defer closeM2()
	if err := m2.Up(); err != nil {
		t.Fatalf("re-up after down: %v", err)
	}
	v, dirty, err := m2.Version()
	if err != nil || dirty {
		t.Fatalf("version after re-up = %d, dirty=%v, err=%v", v, dirty, err)
	}
	if v != upVersion {
		t.Fatalf("version after re-up = %d, want the original %d", v, upVersion)
	}
}

// TestMigrationsRunIsIdempotent proves a second startup against an already
// migrated database is a no-op rather than an error.
func TestMigrationsRunIsIdempotent(t *testing.T) {
	ctx := context.Background()
	dsn, drop, err := provisionDatabase(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	defer drop()

	first, err := migrations.Run(dsn)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	second, err := migrations.Run(dsn)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if first != second {
		t.Fatalf("schema version moved on a repeat run: %d then %d", first, second)
	}
}
