// Package migrations embeds the SQL schema migrations and applies them with
// golang-migrate. Migrations run automatically at startup (main) or manually
// via the --migrate-only flag; golang-migrate's pgx driver serialises
// concurrent runs with a PostgreSQL advisory lock.
package migrations

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	pgxmigrate "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx" for the migrate database driver
)

//go:embed *.sql
var files embed.FS

// Run applies all pending migrations against databaseURL and returns the
// resulting schema version. A schema that is already up to date is not an
// error.
func Run(databaseURL string) (uint, error) {
	src, err := iofs.New(files, ".")
	if err != nil {
		return 0, fmt.Errorf("load embedded migrations: %w", err)
	}

	sqlDB, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return 0, fmt.Errorf("open database: %w", err)
	}
	defer sqlDB.Close() //nolint:errcheck // best-effort cleanup

	driver, err := pgxmigrate.WithInstance(sqlDB, &pgxmigrate.Config{})
	if err != nil {
		return 0, fmt.Errorf("init migrate driver: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", src, "pgx5", driver)
	if err != nil {
		return 0, fmt.Errorf("init migrate: %w", err)
	}
	defer func() {
		srcErr, dbErr := m.Close()
		_ = srcErr
		_ = dbErr // sqlDB is closed above; both closes are best-effort
	}()

	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return 0, fmt.Errorf("apply migrations: %w", err)
	}

	v, dirty, err := m.Version()
	if err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	if dirty {
		return v, fmt.Errorf("schema version %d is dirty; manual intervention required", v)
	}
	return v, nil
}
